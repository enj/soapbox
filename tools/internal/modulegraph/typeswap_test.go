package modulegraph_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/modulegraph"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

func TestTypeswapAdaptsEveryField(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{Retained: []string{facadePkg}})
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}

	internal := lookupTypeswap(t, adapted, internalPkg)
	if internal.Types == nil || internal.Info == nil {
		t.Fatal("the internal package arrived without type information")
	}
	if len(internal.CompiledGoFiles) != len(internal.Syntax) {
		t.Fatalf("%d compiled files and %d parsed files cannot be aligned",
			len(internal.CompiledGoFiles), len(internal.Syntax))
	}
	if !slices.Contains(internal.CompiledGoFiles, "types.go") {
		t.Fatalf("compiled files %v are not base names", internal.CompiledGoFiles)
	}
	if !filepath.IsAbs(internal.Dir) {
		t.Fatalf("package directory %q is not absolute", internal.Dir)
	}

	facade := lookupTypeswap(t, adapted, facadePkg)
	if !slices.Contains(facade.Imports, internalPkg) {
		t.Fatalf("imports %v do not include the internal package", facade.Imports)
	}
	if !slices.IsSorted(facade.Imports) {
		t.Fatalf("imports %v are not sorted", facade.Imports)
	}
}

// TestTypeswapKeepsComments is the assertion the whole marker proof rests on. A
// loader that dropped comments produces exactly the same symptom as a tree that
// records no pairing, and the analysis would then report that upstream pairs
// nothing.
func TestTypeswapKeepsComments(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{})
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}

	published := lookupTypeswap(t, adapted, publishedPkg)
	var directives []string
	for _, file := range published.Syntax {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				directives = append(directives, comment.Text)
			}
		}
	}
	want := "// +k8s:conversion-gen=" + internalPkg
	if !slices.Contains(directives, want) {
		t.Fatalf("the generator directive %q did not survive the load, found %v", want, directives)
	}
}

// TestTypeswapExcludesTheStandardLibrary keeps the marker scan out of GOROOT. A
// standard library package can never be a pair and can never be retained,
// because it is never part of the generated module.
func TestTypeswapExcludesTheStandardLibrary(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{})
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}
	for _, pkg := range adapted.Packages {
		if pkg.ImportPath == "strings" {
			t.Fatal("the standard library was carried into the type policy graph")
		}
	}
}

// TestTypeswapRefusesAnUnprovenRelabel is the load bearing test of this package.
//
// typeswap looks a pair up by import path and decides whether retained code
// still uses an internal type by comparing the package path on the type checked
// object. A relabel that moved only the first would produce a graph where the
// pair resolves and every use of it is invisible, and the analysis would then
// report that nothing references the internal package and that pruning it is
// safe. That is the false pass that deletes code a consumer depends on, so the
// relabel is refused instead.
func TestTypeswapRefusesAnUnprovenRelabel(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{
		Relabel: modulegraph.Relabel{
			ModulePath:     generatedModule,
			InternalPrefix: internalPrefix,
			SourcePrefix:   sourcePrefix,
		},
	})
	if err == nil {
		t.Fatalf("an unproven relabel was accepted and returned %v", adapted)
	}
	if !errors.Is(err, modulegraph.ErrRelabelUnproven) {
		t.Fatalf("error = %v, want ErrRelabelUnproven", err)
	}
	for _, want := range []string{internalPkg, sourcePrefix + "/pkg/apis/rbac", "type identity"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v does not mention %q, so it does not say which identity disagreed", err, want)
		}
	}
	if !strings.Contains(err.Error(), "pruning it is safe") {
		t.Fatalf("error %v does not explain the consequence an operator is being protected from", err)
	}
}

// TestTypeswapAcceptsARelabelThatMatchesNothing proves the refusal above is
// about the proof rather than about the feature being configured. A relabel
// naming a module the graph does not contain relabels nothing, so every
// identity still agrees.
func TestTypeswapAcceptsARelabelThatMatchesNothing(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{
		Relabel: modulegraph.Relabel{
			ModulePath:     "example.test/other",
			InternalPrefix: internalPrefix,
			SourcePrefix:   sourcePrefix,
		},
	})
	if err != nil {
		t.Fatalf("a relabel that matches nothing was refused: %v", err)
	}
	lookupTypeswap(t, adapted, internalPkg)
}

// TestTypeswapRelabelRespectsPathBoundaries covers the prefix that only looks
// like one. A module whose path extends the generated one by a character rather
// than by a path element must not be relabelled.
func TestTypeswapRelabelRespectsPathBoundaries(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{
		Relabel: modulegraph.Relabel{
			// The generated module relocates below internal/kk, so this names
			// internal/k, which is a textual prefix of it and not a path one.
			ModulePath:     generatedModule,
			InternalPrefix: "internal/k",
			SourcePrefix:   sourcePrefix,
		},
	})
	if err != nil {
		t.Fatalf("a textual prefix was treated as a path prefix: %v", err)
	}
	lookupTypeswap(t, adapted, internalPkg)
}

func TestTypeswapRejectsAPartialRelabel(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	tests := []struct {
		name    string
		relabel modulegraph.Relabel
		want    string
	}{
		{
			name:    "no source prefix",
			relabel: modulegraph.Relabel{ModulePath: generatedModule, InternalPrefix: internalPrefix},
			want:    "an upstream source prefix is required",
		},
		{
			name:    "no module path",
			relabel: modulegraph.Relabel{InternalPrefix: internalPrefix, SourcePrefix: sourcePrefix},
			want:    "a generated module path is required",
		},
		{
			name: "absolute internal prefix",
			relabel: modulegraph.Relabel{
				ModulePath: generatedModule, InternalPrefix: "/internal/kk", SourcePrefix: sourcePrefix,
			},
			want: "must be a module relative directory",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{Relabel: test.relabel})
			if err == nil {
				t.Fatalf("a partial relabel was accepted and returned %v", adapted)
			}
			if !errors.Is(err, modulegraph.ErrOptions) {
				t.Fatalf("error = %v, want ErrOptions", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

func TestTypeswapRefusesAbsentPackages(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	tests := []struct {
		name string
		spec modulegraph.TypeswapSpec
		want string
	}{
		{
			name: "named package",
			spec: modulegraph.TypeswapSpec{Packages: []string{facadePkg, "example.test/absent"}},
			want: `package "example.test/absent" is not in the module graph`,
		},
		{
			name: "retained package",
			spec: modulegraph.TypeswapSpec{Retained: []string{"example.test/absent"}},
			want: `retained package "example.test/absent" is not in the graph`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapted, err := graph.Typeswap(t.Context(), test.spec)
			if err == nil {
				t.Fatalf("an absent package was accepted and returned %v", adapted)
			}
			if !errors.Is(err, modulegraph.ErrPackageMissing) {
				t.Fatalf("error = %v, want ErrPackageMissing", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

func TestTypeswapSelectsOnlyTheNamedPackages(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{
		Packages: []string{internalPkg, publishedPkg},
	})
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}
	got := make([]string, 0, len(adapted.Packages))
	for _, pkg := range adapted.Packages {
		got = append(got, pkg.ImportPath)
	}
	if want := []string{internalPkg, publishedPkg}; !slices.Equal(got, want) {
		t.Fatalf("packages = %v, want %v", got, want)
	}
}

// TestTypeswapGraphIsAcceptedByTheAnalyzer runs the real analyzer over the real
// adapted graph. It is the only test here that proves the shape is right rather
// than merely self consistent: typeswap validates the graph itself, and every
// invariant it checks is one this package has to satisfy without being able to
// call the unexported check.
func TestTypeswapGraphIsAcceptedByTheAnalyzer(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{
		Retained: []string{facadePkg, publishedPkg},
	})
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}
	analyzer, err := typeswap.New(t.Context(), typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{{Internal: internalPkg, External: publishedPkg}},
	})
	if err != nil {
		t.Fatalf("create type analyzer: %v", err)
	}

	result, err := analyzer.Analyze(t.Context(), adapted)
	if err != nil {
		t.Fatalf("the adapted graph was refused by the analyzer: %v", err)
	}
	if len(result.Pairs) != 1 {
		t.Fatalf("got %d pair reports, want 1", len(result.Pairs))
	}

	// The marker analysis found the pairing the fixture records, which is only
	// possible if the comments, the compiled file names, and the syntax all
	// arrived and lined up with each other.
	var markers typeswap.AnalysisReport
	for _, analysis := range result.Pairs[0].Analyses {
		if analysis.Name == typeswap.AnalysisMarkers {
			markers = analysis
		}
	}
	if len(markers.Blockers) > 0 {
		t.Fatalf("the marker analysis did not find the recorded pairing: %v", markers.Blockers)
	}
	if len(markers.Evidence) == 0 {
		t.Fatal("the marker analysis produced no evidence, so the directives were not read")
	}
}

// TestTypeswapCopiesWhatItHandsOut proves an adapted graph cannot be changed
// through a later one. Two policies read the same load, and a mutation through
// one would silently change what the other judged.
func TestTypeswapCopiesWhatItHandsOut(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	spec := modulegraph.TypeswapSpec{
		Retained:    []string{facadePkg},
		PrunedFiles: []string{"pkg/apis/rbac/v1/zz_generated.conversion.go"},
	}
	first, err := graph.Typeswap(t.Context(), spec)
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}
	first.Retained[0] = "example.test/mutated"
	first.PrunedFiles[0] = "mutated"
	lookupTypeswap(t, first, internalPkg).Syntax = nil
	spec.Retained[0] = "example.test/also-mutated"

	_, err = graph.Typeswap(t.Context(), spec)
	if err == nil {
		t.Fatal("a mutated specification was accepted, so the specification was not copied")
	}
	if !errors.Is(err, modulegraph.ErrPackageMissing) {
		t.Fatalf("error = %v, want ErrPackageMissing", err)
	}

	spec.Retained[0] = facadePkg
	second, err := graph.Typeswap(t.Context(), spec)
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}
	if second.Retained[0] != facadePkg {
		t.Fatalf("retained = %q, want the mutation to the first graph to be invisible", second.Retained[0])
	}
	if len(lookupTypeswap(t, second, internalPkg).Syntax) == 0 {
		t.Fatal("clearing one graph's syntax cleared the next one's")
	}
}

// lookupTypeswap returns one package of an adapted graph.
func lookupTypeswap(t *testing.T, graph *typeswap.Graph, importPath string) *typeswap.Package {
	t.Helper()
	for _, pkg := range graph.Packages {
		if pkg.ImportPath == importPath {
			return pkg
		}
	}
	t.Fatalf("package %q is not in the adapted graph", importPath)
	return nil
}
