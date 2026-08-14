package modulegraph

import (
	"context"
	"fmt"
	"slices"

	"golang.org/x/tools/go/packages"

	"github.com/enj/soapbox/tools/internal/deppolicy"
)

// DeppolicySpec describes the dependency policy graph to build.
type DeppolicySpec struct {
	// Boundary are the import paths whose exported surface forms the generated
	// module's public boundary. For a curated facade that is the facade
	// package; before the facade exists it is the relocated root packages.
	Boundary []string
	// Candidates are the staging/src relative package paths under
	// consideration, whether or not a profile proposes them. Each names a
	// package that is currently an external dependency of the build, which is
	// what a copy would take ownership of.
	Candidates []string
	// Modules carries the module facts a load cannot establish: zip size,
	// upstream release cadence, and verified licensing. They are merged onto
	// the module identities the load resolved, and a module named here that
	// the build does not contain is refused rather than carried.
	//
	// A module the caller says nothing about keeps every measured flag false,
	// which the policy reads as unmeasured and refuses on. That is the whole
	// reason this is an input rather than something guessed here.
	Modules []deppolicy.Module
}

// Deppolicy adapts the loaded graph into a dependency policy graph.
//
// Three things are built rather than copied, and each one is a place where the
// obvious shortcut produces a policy that passes on missing evidence.
//
// The build graph is every loaded package with its resolved imports, because
// the diamond gate reasons entirely over it: an incomplete build graph would
// find no retained reacher and pass every candidate, which is the most
// expensive possible way to be wrong. Import edges are recorded by the resolved
// package's own path rather than the path written in the source, so every edge
// names a node that exists.
//
// Module identity comes from the load and measured module facts come from the
// caller, and the two are never mixed up. Zip size, cadence, and licensing are
// not derivable from a type checked graph, so this package does not derive
// them; it also does not default them, because a zero zip size and an unmeasured
// zip size are the same number with opposite meanings. Facts that contradict the
// identity the load resolved are refused rather than reconciled, since either
// resolution would attach a measurement to a module it was not taken against.
//
// Compiled line counts are measured from the parsed files, which is a real
// measurement of exactly the files that were type checked rather than a second
// walk of the directory that could disagree with it.
func (g *Graph) Deppolicy(ctx context.Context, spec DeppolicySpec) (*deppolicy.Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("adapt dependency policy graph: %w", err)
	}
	const action = "adapt dependency policy graph"

	boundary, err := g.boundaryPackages(action, spec.Boundary)
	if err != nil {
		return nil, err
	}
	candidates, err := g.candidatePackages(action, spec.Candidates)
	if err != nil {
		return nil, err
	}
	build, err := g.buildGraph(action)
	if err != nil {
		return nil, err
	}
	modules, err := g.modules(action, spec.Modules)
	if err != nil {
		return nil, err
	}
	return &deppolicy.Graph{
		Fset:       g.fset,
		Boundary:   boundary,
		Candidates: candidates,
		Build:      build,
		Modules:    modules,
	}, nil
}

// boundaryPackages resolves the packages forming the public boundary.
func (g *Graph) boundaryPackages(action string, paths []string) ([]*deppolicy.Package, error) {
	var p problems
	if len(paths) == 0 {
		// The interoperability gate asks whether a candidate's types cross the
		// public boundary. With no boundary nothing crosses it, so every
		// candidate would pass a gate that was never run.
		p.addf("boundary: at least one boundary package is required, because a gate with no boundary passes every candidate")
		return nil, p.err(g.redactor, action, ErrOptions)
	}
	wanted := slices.Compact(slices.Sorted(slices.Values(paths)))
	boundary := make([]*deppolicy.Package, 0, len(wanted))
	for _, importPath := range wanted {
		pkg, ok := g.lookup(importPath)
		if !ok {
			p.addf("boundary package %q is not in the module graph", importPath)
			continue
		}
		boundary = append(boundary, g.adaptPackage(pkg))
	}
	if err := p.err(g.redactor, action, ErrPackageMissing); err != nil {
		return nil, err
	}
	return boundary, nil
}

// candidatePackages resolves the staging packages under consideration.
//
// The import path is derived from the staging path rather than accepted
// alongside it. The staging tree maps directly onto module paths, deppolicy
// checks that the two agree, and letting a caller state both would be a way to
// build a candidate the policy then rejects for a reason the caller introduced.
func (g *Graph) candidatePackages(action string, stagingPaths []string) ([]deppolicy.Candidate, error) {
	var malformed, missing problems
	wanted := slices.Compact(slices.Sorted(slices.Values(stagingPaths)))
	candidates := make([]deppolicy.Candidate, 0, len(wanted))
	for _, stagingPath := range wanted {
		importPath := deppolicy.ImportPathOf(stagingPath)
		if importPath == stagingPath {
			// deppolicy owns the staging path rule and states it in full. This
			// only catches the case where nothing was stripped, because that is
			// the one that would otherwise become a lookup failure against an
			// import path nobody meant to name.
			malformed.addf("candidate %q is not a staging path, so the import path it provides is unknown", stagingPath)
			continue
		}
		pkg, ok := g.lookup(importPath)
		if !ok {
			// A candidate that silently vanished would be judged as owning no
			// types and costing nothing, which passes every gate it should
			// fail. An upstream rename has to fail the run instead.
			missing.addf("candidate %q provides import path %q, which is not in the module graph", stagingPath, importPath)
			continue
		}
		candidates = append(candidates, deppolicy.Candidate{StagingPath: stagingPath, Package: g.adaptPackage(pkg)})
	}
	// A malformed path is reported first and on its own, because it is the
	// caller's spelling rather than a fact about the build, and a spelling
	// error explains any lookup failure that follows it.
	if err := malformed.err(g.redactor, action, ErrOptions); err != nil {
		return nil, err
	}
	if err := missing.err(g.redactor, action, ErrPackageMissing); err != nil {
		return nil, err
	}
	return candidates, nil
}

// buildGraph renders the resolved consumer build, one entry per loaded package.
func (g *Graph) buildGraph(action string) ([]deppolicy.BuildPackage, error) {
	var p problems
	build := make([]deppolicy.BuildPackage, 0, len(g.ordered))
	for _, pkg := range g.ordered {
		modulePath := ""
		if pkg.Module != nil {
			modulePath = pkg.Module.Path
		}
		imports := importPaths(pkg)
		for _, imported := range imports {
			if _, ok := g.lookup(imported); !ok {
				// The diamond gate walks these edges. An edge naming a node the
				// graph does not hold ends a walk early, and a walk that ends
				// early reports no reacher, which passes the candidate.
				p.addf("package %q imports %q, which is not in the module graph, so the build graph is incomplete",
					pkg.PkgPath, imported)
			}
		}
		build = append(build, deppolicy.BuildPackage{
			ImportPath: pkg.PkgPath,
			Module:     modulePath,
			Imports:    imports,
			Lines:      countLines(g.fset, pkg),
		})
	}
	if err := p.err(g.redactor, action, ErrLoad); err != nil {
		return nil, err
	}
	return build, nil
}

// modules merges the module identities the load resolved with the facts the
// caller measured.
func (g *Graph) modules(action string, supplied []deppolicy.Module) ([]deppolicy.Module, error) {
	var p problems

	// Identity first, taken from the load. Two packages of one module must
	// agree about its version and root; if they do not, the graph holds two
	// different modules under one path and no measurement could be attached to
	// the right one.
	identity := make(map[string]deppolicy.Module)
	var order []string
	for _, pkg := range g.ordered {
		if pkg.Module == nil || pkg.Module.Path == "" {
			continue
		}
		found := deppolicy.Module{Path: pkg.Module.Path, Version: pkg.Module.Version, Dir: pkg.Module.Dir}
		previous, seen := identity[found.Path]
		if !seen {
			identity[found.Path] = found
			order = append(order, found.Path)
			continue
		}
		if previous.Version != found.Version || previous.Dir != found.Dir {
			p.addf("module %q is resolved both as version %q in %q and as version %q in %q",
				found.Path, previous.Version, previous.Dir, found.Version, found.Dir)
		}
	}
	if err := p.err(g.redactor, action, ErrModuleConflict); err != nil {
		return nil, err
	}

	for _, fact := range supplied {
		resolved, ok := identity[fact.Path]
		if !ok {
			// A measurement of a module the build does not contain is stale
			// evidence. Carrying it would let an operator believe a gate was
			// answered for a dependency that is no longer there.
			p.addf("module facts name %q, which the build does not contain", fact.Path)
			continue
		}
		if fact.Version != "" && fact.Version != resolved.Version {
			p.addf("module %q was measured at version %q but the build resolved version %q",
				fact.Path, fact.Version, resolved.Version)
			continue
		}
		if fact.Dir != "" && fact.Dir != resolved.Dir {
			p.addf("module %q was measured in %q but the build resolved it in %q",
				fact.Path, fact.Dir, resolved.Dir)
			continue
		}
		// Identity stays the load's and only the measured fields are taken, so
		// a caller cannot rename a module by supplying facts about it.
		merged := resolved
		merged.ZipBytes = fact.ZipBytes
		merged.ZipBytesKnown = fact.ZipBytesKnown
		merged.ReleasesPerMinor = fact.ReleasesPerMinor
		merged.CadenceKnown = fact.CadenceKnown
		merged.Licenses = cloneLicenses(fact.Licenses)
		merged.LicensesVerified = fact.LicensesVerified
		identity[fact.Path] = merged
	}
	if err := p.err(g.redactor, action, ErrModuleConflict); err != nil {
		return nil, err
	}

	slices.Sort(order)
	modules := make([]deppolicy.Module, 0, len(order))
	for _, modulePath := range order {
		// A module nobody measured is emitted with its identity and every
		// measured flag false. That is not an omission: the policy refuses a
		// copy on unmeasured evidence, and leaving the module out entirely
		// would look the same as a module with nothing to answer for.
		modules = append(modules, identity[modulePath])
	}
	return modules, nil
}

// adaptPackage copies one loaded package into the dependency policy's shape.
func (g *Graph) adaptPackage(pkg *packages.Package) *deppolicy.Package {
	modulePath := ""
	if pkg.Module != nil {
		modulePath = pkg.Module.Path
	}
	return &deppolicy.Package{
		ImportPath: pkg.PkgPath,
		Module:     modulePath,
		Dir:        packageDir(pkg),
		Types:      pkg.Types,
		Syntax:     slices.Clone(pkg.Syntax),
		Info:       pkg.TypesInfo,
		// The policy opens Dir as a root and reads the named files inside it,
		// so these are base names rather than the loader's absolute paths.
		GoFiles:      baseNames(pkg.GoFiles),
		OtherFiles:   baseNames(pkg.OtherFiles),
		IgnoredFiles: baseNames(pkg.IgnoredFiles),
		Imports:      importPaths(pkg),
	}
}

// cloneLicenses deep copies verified licence records so a caller cannot mutate
// what the graph reports after it was built.
func cloneLicenses(licenses []deppolicy.License) []deppolicy.License {
	if licenses == nil {
		return nil
	}
	out := make([]deppolicy.License, 0, len(licenses))
	for _, license := range licenses {
		license.Files = slices.Clone(license.Files)
		out = append(out, license)
	}
	return out
}
