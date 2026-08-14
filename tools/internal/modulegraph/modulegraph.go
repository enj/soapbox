// Package modulegraph loads one already resolved Go module and hands the policy
// packages the typed graphs they judge.
//
// It exists because deppolicy and typeswap deliberately do not load anything.
// Both are pure analyzers over a graph the caller supplies, which is what keeps
// them free of go/packages, of a module resolver, and of any ambient
// environment. That choice moves one job to their caller rather than removing
// it: something still has to run the loader, prove that what came back is
// complete, and translate it into each package's own shape. Doing that inline at
// two call sites would produce two adapters that could disagree about what a
// missing field means, and the answer to that question is what decides whether a
// module ships.
//
// So the rules live here once:
//
//   - The environment is supplied in full and never inherited. A load runs the
//     go command, so an ambient environment would let GOFLAGS, a module cache,
//     or a proxy chosen by whoever started the process decide which code was
//     type checked. gocli.Runner.LoaderEnv builds the environment this expects.
//   - Everything asked of the loader has to come back. go/packages leaves
//     unrequested fields nil, so a field that is missing after being requested
//     means the load did not deliver it, and every check reading that field
//     would pass without looking at anything. Missing data therefore fails the
//     load instead of quietly weakening the analyses downstream.
//   - Nothing is measured as zero when it was not measured. An unmeasured cost
//     and a cost of zero are the same number and opposite meanings, and only one
//     of them is evidence.
//
// The two adapters are narrow on purpose. Neither invents a fact: the typeswap
// adapter refuses a relabel it cannot prove rather than producing a graph whose
// import paths and type identities disagree, and the dependency adapter records
// module identity from the load while leaving zip size, cadence, and licensing
// to the caller that actually measured them.
package modulegraph

import (
	"context"
	"fmt"
	"go/token"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// loadMode is exactly the information the policy packages read.
//
// Every bit is load bearing, and asking for less would make some check silently
// vacuous rather than fail. Names and modules identify a package and separate
// one the generated module owns from a dependency. Files anchor cost
// measurement and cgo detection to real paths, and compiled files are what the
// parsed syntax actually came from, which is not the same list for a cgo
// package. Syntax carries the generator directives a pairing is proved from and
// the statements a global state scan reads. Types and type information are what
// resolve a selector to a real object instead of guessing from its spelling.
// Imports and dependencies extend all of it to the transitive graph, which is
// where a leaked type, an unimplemented interface, and a diamond actually live.
const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo |
	packages.NeedImports |
	packages.NeedDeps |
	packages.NeedModule

// Options configures one load.
type Options struct {
	// Dir is the module directory the load runs in. It must be absolute, so
	// which module is loaded never depends on the process working directory.
	Dir string
	// Env is the complete environment the go command runs under. It is never
	// inherited and never extended here: a caller that supplies nothing is
	// refused rather than defaulted, because the default would be whatever
	// shell started the engine. gocli.Runner.LoaderEnv produces it.
	Env []string
	// Patterns are the package patterns to load. They are the roots; the
	// transitive graph follows from NeedDeps.
	Patterns []string
	// Redactor removes credentials from everything this package reports. It is
	// required, for the same reason Env is.
	//
	// Env carries GOPROXY, and a module proxy URL is a normal place for a token
	// to live. Every other Go subprocess in the engine runs through
	// gocli.Runner, which captures its output through a redactor seeded with
	// exactly that credential. A package load does not: it starts the go
	// command outside that runner, so the diagnostics come back through
	// go/packages with nothing between them and the caller. A failed module
	// fetch quotes the URL it was fetching from, and that string then travels
	// into a report, a log, or a tracking issue.
	//
	// gocli.Runner.Redactor returns the one to pass, already seeded with the
	// proxy credential and every Env value, so the loader and the runner scrub
	// the same secrets. A caller with nothing to hide states
	// gitcli.NewRedactor(), which redacts nothing but says so on purpose.
	Redactor *gitcli.Redactor
}

// Graph is one loaded, validated module graph.
//
// It is immutable once returned. Every accessor and adapter copies what it
// hands out, so a caller cannot reach back into the loader's state and change
// what a later adapter sees.
type Graph struct {
	fset    *token.FileSet
	ordered []*packages.Package
	byPath  map[string]*packages.Package
	// redactor scrubs what the adapters report. It is carried on the graph
	// rather than taken again per adaptation because the strings an adapter
	// quotes, module roots and resolved versions, came from the same go command
	// whose output the load already had to scrub.
	redactor *gitcli.Redactor
}

// Load type checks the module in opts.Dir and everything the patterns reach.
//
// It fails rather than returning a partial graph. A load that half worked is
// the one input none of the policies can detect: they would run every analysis,
// find nothing wrong in the packages that are missing, and report a clean
// result over code that was never examined.
func Load(ctx context.Context, opts Options) (*Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load module graph: %w", err)
	}
	switch {
	case opts.Dir == "":
		return nil, fmt.Errorf("load module graph: %w: a module directory is required", ErrOptions)
	case !filepath.IsAbs(opts.Dir):
		return nil, fmt.Errorf("load module graph: %w: module directory %q must be absolute", ErrOptions, opts.Dir)
	case len(opts.Env) == 0:
		return nil, fmt.Errorf("load module graph: %w: an explicit environment is required, because an inherited one would let the ambient GOFLAGS, module cache, and proxy decide which code the policies are decided from", ErrOptions)
	case len(opts.Patterns) == 0:
		return nil, fmt.Errorf("load module graph: %w: at least one package pattern is required", ErrOptions)
	case opts.Redactor == nil:
		// Refused rather than defaulted to a redactor that scrubs nothing. The
		// default would be silent, and the thing it would silently stop
		// scrubbing is the proxy credential in Env.
		return nil, fmt.Errorf("load module graph: %w: a redactor is required, because the environment carries GOPROXY and a failed module fetch quotes the URL it used; pass gocli.Runner.Redactor, or gitcli.NewRedactor() to state that there is nothing to scrub", ErrOptions)
	}
	redactor := opts.Redactor

	// One file set is shared by every package, which is what makes a position
	// from one package comparable with a position from another. Evidence in
	// both policy reports carries positions, so two file sets would render two
	// unrelated coordinate spaces that happen to look alike.
	fset := token.NewFileSet()
	config := &packages.Config{
		Context: ctx,
		Mode:    loadMode,
		Dir:     opts.Dir,
		Fset:    fset,
		// The environment is passed through verbatim. Appending to it here
		// would hide a knob from the caller that decides what gets type
		// checked.
		Env: slices.Clone(opts.Env),
		// Test variants would bring a package's test only declarations into
		// scope, and neither policy judges a module's tests.
		Tests: false,
	}
	patterns := slices.Clone(opts.Patterns)
	roots, err := packages.Load(config, patterns...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("load %s: %w", redactedPatterns(redactor, patterns), ctxErr)
		}
		return nil, fmt.Errorf("load %s: %w: %s", redactedPatterns(redactor, patterns), ErrLoad, redactor.String(err.Error()))
	}

	graph := &Graph{fset: fset, byPath: map[string]*packages.Package{}, redactor: redactor}
	var duplicates []string
	// Visit walks the roots and every dependency in a deterministic order, so
	// a module with several broken packages reports the same problems in the
	// same order on every run.
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg.PkgPath == "" {
			// A package with no import path cannot be indexed, named in a
			// problem, or looked up by an adapter. It is collected separately
			// so validation still reports it.
			graph.ordered = append(graph.ordered, pkg)
			return
		}
		if _, seen := graph.byPath[pkg.PkgPath]; seen {
			duplicates = append(duplicates, pkg.PkgPath)
			return
		}
		graph.byPath[pkg.PkgPath] = pkg
		graph.ordered = append(graph.ordered, pkg)
	})
	// A pattern that matched nothing is a silent success in go/packages: it
	// returns no package and no error. Downstream that becomes a policy run
	// over an empty graph, which passes everything, so the empty match is
	// reported as what it is.
	if len(graph.ordered) == 0 {
		return nil, fmt.Errorf("load %s: %w: no package matched", redactedPatterns(redactor, patterns), ErrLoad)
	}
	slices.SortFunc(graph.ordered, func(a, b *packages.Package) int {
		return strings.Compare(a.PkgPath, b.PkgPath)
	})

	if err := graph.validate(patterns, duplicates); err != nil {
		return nil, err
	}
	return graph, nil
}

// validate refuses a graph that cannot support an analysis.
//
// The checks are split by whether a package is in the standard library. A
// standard library package is supplied by GOROOT, is never owned by the
// generated module, and in the case of unsafe has no source files at all, so
// requiring syntax or a module of it would refuse every load ever attempted.
// Everything else was asked for in full and has to arrive in full.
func (g *Graph) validate(patterns, duplicates []string) error {
	var p problems
	for _, duplicate := range duplicates {
		// Two packages at one import path make lookup ambiguous, and an
		// adapter resolving a boundary package or a candidate would silently
		// take whichever arrived first.
		p.addf("package %q was loaded more than once, so a lookup cannot say which one a policy would judge", duplicate)
	}
	for _, pkg := range g.ordered {
		if pkg.PkgPath == "" {
			p.addf("a loaded package has no import path, so it cannot be identified")
			continue
		}
		for _, problem := range pkg.Errors {
			position := problem.Pos
			if position == "" {
				position = pkg.PkgPath
			}
			p.addf("%s: %s", position, problem.Msg)
		}
		if pkg.Types == nil {
			p.addf("package %q is not type checked", pkg.PkgPath)
		}
		// Syntax is parsed from the compiled files, which for a cgo package are
		// the translated outputs rather than the originals. Both policies name
		// a file by indexing the compiled list with a syntax index, so a pair
		// that does not line up would attribute a directive or an effect to the
		// wrong file, or index out of range.
		if len(pkg.CompiledGoFiles) != len(pkg.Syntax) {
			p.addf("package %q has %d compiled files and %d parsed files, which cannot be aligned",
				pkg.PkgPath, len(pkg.CompiledGoFiles), len(pkg.Syntax))
		}
		if isStandard(pkg.PkgPath) {
			continue
		}
		if pkg.TypesInfo == nil {
			p.addf("package %q carries no type information, so a selector could not be resolved to an object", pkg.PkgPath)
		}
		if len(pkg.Syntax) == 0 {
			p.addf("package %q carries no syntax, so a scan over it would find nothing and report it as clean", pkg.PkgPath)
		}
		if pkg.Module == nil || pkg.Module.Path == "" {
			p.addf("package %q has no module identity, so which module a copy would take ownership of is unknown", pkg.PkgPath)
		}
	}
	return p.err(g.redactor, "load "+redactedPatterns(g.redactor, patterns), ErrLoad)
}

// Fset reports the file set every package in the graph was parsed with.
func (g *Graph) Fset() *token.FileSet { return g.fset }

// ImportPaths reports every loaded import path, sorted.
func (g *Graph) ImportPaths() []string {
	paths := make([]string, 0, len(g.ordered))
	for _, pkg := range g.ordered {
		paths = append(paths, pkg.PkgPath)
	}
	return paths
}

// Packages reports every loaded package in sorted order.
//
// The caller must not modify the returned slice or the packages it contains.
func (g *Graph) Packages() []*packages.Package {
	return g.ordered
}

// lookup reports the package loaded at an import path.
func (g *Graph) lookup(importPath string) (*packages.Package, bool) {
	pkg, ok := g.byPath[importPath]
	return pkg, ok
}

// isStandard reports whether an import path belongs to the standard library.
//
// The test is the go command's own: a standard library path's first element
// carries no dot, because it is a domain name that makes a path a module path.
func isStandard(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// packageDir reports the directory holding a package's own source files.
//
// It is derived from GoFiles rather than from CompiledGoFiles because a cgo
// package's compiled files are generated into a build directory that contains
// none of its source. Both policies open this directory to read the package's

// redactedPatterns renders the requested patterns for an error message.
//
// Patterns come from the caller rather than from the go command, so they are
// the least likely thing here to hold a secret. They are scrubbed anyway,
// because "this input is trusted" is the assumption every leak is built on and
// the cost of not making it is one function call.
func redactedPatterns(redactor *gitcli.Redactor, patterns []string) string {
	return strings.Join(redactor.Strings(patterns), " ")
}

// files, so naming the build directory would measure a tree that does not exist
// after the run.
func packageDir(pkg *packages.Package) string {
	for _, file := range pkg.GoFiles {
		return filepath.Dir(file)
	}
	for _, file := range pkg.OtherFiles {
		return filepath.Dir(file)
	}
	return ""
}

// baseNames reports the base name of each path, sorted.
//
// The policies record file names relative to the package directory, so an
// absolute path from the loader is reduced here rather than at each field.
func baseNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// cgoOriginals reports the package's own Go source files that were not compiled
// as written, by base name and in the loader's order.
//
// go/packages has no cgo file list of its own: its GoFiles is the go command's
// GoFiles and CgoFiles joined together, and the translated outputs appear in
// CompiledGoFiles under a build directory under different names. What is left
// is the difference between the two lists, which is exactly the set of source
// files the toolchain replaced before compiling.
//
// The derivation is deliberately inclusive. A file the loader listed as source
// but did not compile is reported here whatever the reason, so a package that
// uses cgo can never come back with an empty list. An empty list is read
// downstream as an absence of cgo, and that is the reading that must not be
// produced by accident.
func cgoOriginals(pkg *packages.Package) []string {
	compiled := make(map[string]bool, len(pkg.CompiledGoFiles))
	for _, file := range pkg.CompiledGoFiles {
		compiled[filepath.Base(file)] = true
	}
	var originals []string
	for _, file := range pkg.GoFiles {
		if name := filepath.Base(file); !compiled[name] {
			originals = append(originals, name)
		}
	}
	return originals
}

// importPaths reports the resolved import paths of a package's imports, sorted.
//
// The resolved package's own path is used rather than the map key. The key is
// the path as it is written in the source, and a build graph whose edges were
// spelled that way could name an edge that matches no node, which is exactly
// the shape the diamond gate walks.
func importPaths(pkg *packages.Package) []string {
	paths := make([]string, 0, len(pkg.Imports))
	for key, imported := range pkg.Imports {
		if imported != nil && imported.PkgPath != "" {
			paths = append(paths, imported.PkgPath)
			continue
		}
		paths = append(paths, key)
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// countLines reports how many lines the toolchain compiles for a package.
//
// It is measured from the parsed files rather than by reading the directory
// again, so the count covers exactly the files that were type checked. A
// package whose syntax the loader did not deliver reports zero, which both
// policies read as unmeasured rather than as a package of no size.
func countLines(fset *token.FileSet, pkg *packages.Package) int {
	total := 0
	for _, file := range pkg.Syntax {
		if file == nil || !file.FileStart.IsValid() {
			continue
		}
		if handle := fset.File(file.FileStart); handle != nil {
			total += handle.LineCount()
		}
	}
	return total
}
