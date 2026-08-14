package generate

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
	"github.com/enj/soapbox/tools/internal/modulegraph"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// runDependencies decides which staging packages the generated module owns.
//
// The decision is reached over the complete post-prune module, facade included,
// which is why the facade files are installed into the scratch module before the
// graph is loaded. The facade is the module's public boundary, so a graph that
// did not contain it would be asked which dependencies the boundary needs while
// the boundary was missing, and every answer would be about the relocated
// packages rather than about what a consumer actually compiles.
//
// Installing the facade first also closes the one way the published go.mod could
// be wrong. It was tidied before the facade existed, so a facade import that
// needs a requirement the relocated code does not, or that turns an indirect
// requirement direct, would leave the module metadata describing a tree that is
// not the one being published. Tidying is therefore re-run as a diff: it writes
// nothing and refuses if anything would change.
func (r *run) runDependencies(ctx context.Context) error {
	if err := r.installFacade(ctx); err != nil {
		return err
	}

	graph, err := r.loadModuleGraph(ctx)
	if err != nil {
		return runtimeError(stageDependencies, err)
	}

	// Translating the profile is pure, so a failure is a statement about what
	// the dependency section says.
	options, err := dependencyOptions(r.cfg, r.opts.sourceRelease())
	if err != nil {
		return policyError(stageDependencies, err)
	}

	// Measure real module facts for candidate staging modules. The cost gates
	// need exact zip bytes, release cadence, and verified licences; fabricated
	// values would approve a copy that exceeds ceilings.
	moduleFacts, evidence, err := r.measureCandidateModuleFacts(ctx, options.Proposals)
	if err != nil {
		return runtimeError(stageDependencies, err)
	}
	r.copyEvidence = evidence

	// Candidates are the staging packages a copy would take ownership of.
	// Stating an empty candidate set is the honest answer rather than a skipped
	// phase: the decision is that nothing is copied, and a report that recorded
	// no decision at all would be indistinguishable from one where the phase
	// never ran.
	depGraph, err := graph.Deppolicy(ctx, modulegraph.DeppolicySpec{
		Boundary:   []string{r.cfg.Destination.Module},
		Candidates: options.Proposals,
		Modules:    moduleFacts,
	})
	if err != nil {
		return classify(stageDependencies, err, dependencySemantic...)
	}

	decider, err := deppolicy.New(ctx, options)
	if err != nil {
		return classify(stageDependencies, err, dependencySemantic...)
	}
	result, err := decider.Decide(ctx, depGraph)
	if err != nil {
		return classify(stageDependencies, err, dependencySemantic...)
	}

	if len(result.Copy) > 0 {
		copyFiles, err := r.materializeCopies(ctx, result, depGraph)
		if err != nil {
			return runtimeError(stageDependencies, err)
		}

		// Rewrite imports of the copied staging module in BOTH the copy files
		// and the retained extracted files. The retained code imports the
		// staging module externally; those imports must point at the relocated
		// copy for the requirement to actually drop from go.mod.
		//
		// Copied files receive a modification notice (noNotice=false) because
		// the staging rewrite is their first modification. Retained files
		// already carry the extraction notice and must not receive a duplicate
		// (noNotice=true).
		copyResult, err := r.rewriteStagingImports(ctx, copyFiles, result.Copy, false)
		if err != nil {
			return runtimeError(stageDependencies, err)
		}
		r.copyFiles = copyResult.files

		// Rewrite the extracted files in place so the scratch module's source
		// also reflects the new imports before we tidy.
		retainedResult, err := r.rewriteStagingImports(ctx, r.post.Files, result.Copy, true)
		if err != nil {
			return runtimeError(stageDependencies, err)
		}
		r.post.Files = retainedResult.files

		// Build provenance records for copied packages so the provenance
		// cross-check can account for them in the tree. The changes from the
		// staging rewrite are included so the per-package record says which
		// imports were rewritten.
		if err := r.addCopyProvenance(r.copyEvidence, copyResult.changes); err != nil {
			return runtimeError(stageDependencies, err)
		}

		// Merge the retained files' staging rewrite changes into the existing
		// extraction provenance records so a reviewer can see every import
		// path the engine modified, not just the extraction rewrites.
		r.mergeRetainedChanges(retainedResult.changes)

		// Install the copied files and rewritten extracted files into the
		// scratch module and re-tidy so the module metadata reflects the
		// requirements the copy removed.
		if err := r.installCopies(ctx); err != nil {
			return err
		}

		// Verify the post-copy module compiles, the metadata is stable,
		// and the accepted copy delivered its predicted module removals.
		if err := r.verifyPostCopy(ctx, result); err != nil {
			return err
		}
	}

	r.report.recordDependencies(result)

	// Enforce forbidden modules against the final module state. This runs
	// after any copy has been installed and tidied, so the check sees the
	// module as it will be published. For a no-copy profile the check runs
	// against the facade-installed module.
	if err := r.checkForbiddenModules(ctx); err != nil {
		return err
	}

	return nil
}

// installFacade writes the generated facade into the post-prune scratch module
// and proves the module metadata still describes it.
func (r *run) installFacade(ctx context.Context) error {
	for _, file := range r.postFacade.Files {
		path := filepath.Join(r.paths.PostModule, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
		}
		if err := os.WriteFile(path, file.Contents, file.Mode.FileMode().Perm()); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
		}
	}

	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
	}
	// A tidy that could not run is a toolchain condition. A tidy that ran and
	// reported a difference is the finding: the published metadata would not
	// describe the published tree.
	if err := runner.Tidy(ctx, gocli.TidyOptions{Diff: true}); err != nil {
		if errors.Is(err, gocli.ErrTidyRequired) {
			return policyError(stageDependencies, fmt.Errorf("the generated facade needs module requirements the tidied go.mod does not state, so the published metadata would not describe the published tree: %w", err))
		}
		return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
	}
	return nil
}

// loadModuleGraph type checks the complete generated module.
func (r *run) loadModuleGraph(ctx context.Context) (*modulegraph.Graph, error) {
	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return nil, fmt.Errorf("module graph: %w", err)
	}
	env, err := runner.LoaderEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("module graph: %w", err)
	}
	graph, err := modulegraph.Load(ctx, modulegraph.Options{
		Dir:      r.paths.PostModule,
		Env:      env,
		Patterns: []string{"./..."},
		Redactor: runner.Redactor(),
	})
	if err != nil {
		return nil, fmt.Errorf("module graph: %w", err)
	}
	return graph, nil
}

// installCopies writes the materialized copies and the rewritten extracted
// files into the scratch module, re-tidies the module metadata, and verifies
// the post-copy module still type checks.
//
// The copy replaces an external module dependency with owned source, and the
// retained files have had their imports rewritten to point at the copy rather
// than the external module. Tidying is run as a write because the change is
// expected: the whole point of a copy is to drop a requirement.
//
// After tidying, the typed graph is reloaded. This is the post-copy compilation
// gate: a rewrite that broke an import, a copy that introduced a cycle, or a
// file the build constraints exclude would all surface here as a load failure
// rather than reaching provenance with a tree that does not compile.
//
// The module report is refreshed so the provenance and output phases compose
// the metadata the toolchain actually settled on rather than stale pre-copy
// bytes.
func (r *run) installCopies(ctx context.Context) error {
	// Write the copy files.
	for _, file := range r.copyFiles.Files {
		path := filepath.Join(r.paths.PostModule, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("copy install: %w", err))
		}
		if err := os.WriteFile(path, file.Contents, file.Mode.FileMode().Perm()); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("copy install: %w", err))
		}
	}

	// Overwrite the extracted files that had their imports rewritten.
	for _, file := range r.post.Files.Files {
		path := filepath.Join(r.paths.PostModule, filepath.FromSlash(file.Path))
		if err := os.WriteFile(path, file.Contents, file.Mode.FileMode().Perm()); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("copy install rewrite: %w", err))
		}
	}

	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("copy install: %w", err))
	}
	if err := runner.Tidy(ctx, gocli.TidyOptions{}); err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("copy install tidy: %w", err))
	}

	// Reload the typed graph to prove the post-copy module compiles. A copy
	// that broke an import path, introduced a cycle, or left a reference to
	// the now-absent external module would fail the load rather than reaching
	// the provenance phase with a tree that does not build.
	if _, err := r.loadModuleGraph(ctx); err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("post-copy verification: %w", err))
	}

	// A tidy-diff confirms that the module metadata the provenance phase will
	// read is exactly what the toolchain would produce from this tree. The
	// write-tidy above settled the requirements; this diff-tidy proves nothing
	// drifted between the write and the reload.
	if err := runner.Tidy(ctx, gocli.TidyOptions{Diff: true}); err != nil {
		if errors.Is(err, gocli.ErrTidyRequired) {
			return policyError(stageDependencies, fmt.Errorf("the post-copy module metadata does not describe the post-copy tree: %w", err))
		}
		return runtimeError(stageDependencies, fmt.Errorf("copy install verify: %w", err))
	}

	// Refresh the module report with the post-tidy metadata. The provenance
	// phase reads GoMod and GoSum from here, and they changed when tidying
	// dropped the requirement the copy replaced.
	if err := r.refreshModuleReport(ctx); err != nil {
		return classify(stageDependencies, fmt.Errorf("copy install: %w", err), moduleSemantic...)
	}
	return nil
}

// refreshModuleReport re-reads go.mod and go.sum from the scratch module after
// a tidy that changed them.
func (r *run) refreshModuleReport(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	goMod, err := os.ReadFile(filepath.Join(r.paths.PostModule, "go.mod"))
	if err != nil {
		return fmt.Errorf("read refreshed go.mod: %w", err)
	}
	goSum, err := os.ReadFile(filepath.Join(r.paths.PostModule, "go.sum"))
	if errors.Is(err, os.ErrNotExist) {
		// A module requiring nothing outside the standard library has no
		// checksums, so an absent go.sum is the honest empty state.
		goSum = nil
	} else if err != nil {
		return fmt.Errorf("read refreshed go.sum: %w", err)
	}

	// Re-parse and reconcile the requirements before replacing the report. A
	// copy may remove requirements, but it may neither invent a new pin nor
	// change a surviving pin's version.
	kept, err := parseKeptRequirements(goMod)
	if err != nil {
		return fmt.Errorf("parse refreshed requirements: %w", err)
	}
	refreshed, err := reconcileRefreshedRequirements(r.moduleReport, kept)
	if err != nil {
		return err
	}
	refreshed.GoMod = goMod
	refreshed.GoSum = goSum
	r.moduleReport = refreshed
	r.report.refreshModule(refreshed)
	return nil
}

// reconcileRefreshedRequirements carries the source-relative dependency report
// through a copy-induced tidy. A copied package is allowed to remove a module;
// it is not allowed to make the Go command select a dependency the verified
// pre-copy module did not contain or to change a surviving version.
func reconcileRefreshedRequirements(previous *modgen.Report, kept []gomodmap.Requirement) (*modgen.Report, error) {
	if previous == nil {
		return nil, errors.New("reconcile refreshed requirements: prior module report is required")
	}

	previousByPath := make(map[string]gomodmap.Requirement, len(previous.Kept))
	sourceIndirect := make(map[string]bool, len(previous.Kept))
	for _, requirement := range previous.Kept {
		if _, exists := previousByPath[requirement.Path]; exists {
			return nil, fmt.Errorf("reconcile refreshed requirements: duplicate prior requirement %s", requirement.Path)
		}
		previousByPath[requirement.Path] = requirement
		sourceIndirect[requirement.Path] = requirement.Indirect
	}

	added := make(map[string]bool, len(previous.Added))
	for _, requirement := range previous.Added {
		if _, exists := previousByPath[requirement.Path]; !exists {
			return nil, fmt.Errorf("reconcile refreshed requirements: prior added requirement %s is not kept", requirement.Path)
		}
		added[requirement.Path] = true
	}
	for _, reclassified := range previous.Reclassified {
		if _, exists := previousByPath[reclassified.Path]; !exists {
			return nil, fmt.Errorf("reconcile refreshed requirements: prior reclassification %s is not kept", reclassified.Path)
		}
		if added[reclassified.Path] {
			return nil, fmt.Errorf("reconcile refreshed requirements: added requirement %s cannot be source-reclassified", reclassified.Path)
		}
		// Reclassification records the generated marking, so the source marking
		// is its opposite.
		sourceIndirect[reclassified.Path] = !reclassified.Indirect
	}

	refreshed := *previous
	refreshed.Kept = slices.Clone(kept)
	refreshed.Added = nil
	refreshed.Reclassified = nil
	refreshed.Dropped = slices.Clone(previous.Dropped)
	dropped := make(map[string]bool, len(refreshed.Dropped)+len(previous.Kept))
	for _, modulePath := range refreshed.Dropped {
		dropped[modulePath] = true
	}

	keptPaths := make(map[string]bool, len(kept))
	for _, requirement := range kept {
		if keptPaths[requirement.Path] {
			return nil, fmt.Errorf("%w: copy tidy retained duplicate requirement %s", modgen.ErrModuleDrift, requirement.Path)
		}
		keptPaths[requirement.Path] = true
		prior, exists := previousByPath[requirement.Path]
		if !exists {
			return nil, fmt.Errorf("%w: copy tidy added %s %s", modgen.ErrModuleDrift, requirement.Path, requirement.Version)
		}
		if requirement.Version != prior.Version {
			return nil, fmt.Errorf("%w: copy tidy changed %s from %s to %s", modgen.ErrPinFloated, requirement.Path, prior.Version, requirement.Version)
		}
		if added[requirement.Path] {
			refreshed.Added = append(refreshed.Added, requirement)
			continue
		}
		if requirement.Indirect != sourceIndirect[requirement.Path] {
			refreshed.Reclassified = append(refreshed.Reclassified, modgen.Reclassification{
				Path:     requirement.Path,
				Indirect: requirement.Indirect,
			})
		}
	}
	for modulePath := range previousByPath {
		if keptPaths[modulePath] || added[modulePath] || dropped[modulePath] {
			continue
		}
		refreshed.Dropped = append(refreshed.Dropped, modulePath)
		dropped[modulePath] = true
	}

	slices.Sort(refreshed.Dropped)
	slices.SortFunc(refreshed.Added, func(a, b gomodmap.Requirement) int {
		return cmp.Compare(a.Path, b.Path)
	})
	slices.SortFunc(refreshed.Reclassified, func(a, b modgen.Reclassification) int {
		return cmp.Compare(a.Path, b.Path)
	})
	return &refreshed, nil
}

// verifyPostCopy confirms the post-copy module compiles, the metadata is
// stable, and each accepted candidate's predicted module removals were
// delivered.
//
// It type-checks the post-copy module, tidy-diffs the metadata, then checks
// each accepted candidate's Score.ModulesRemoved against the actual post-copy
// build. A module the pre-copy score predicted would leave must actually be
// absent; one the score predicted would stay may still be present.
//
// Module list errors are runtime failures: an unresolvable record makes the
// complete build unknown.
func (r *run) verifyPostCopy(ctx context.Context, preCopyResult *deppolicy.Result) error {
	// Type check the post-copy module to catch broken imports or cycles.
	if _, err := r.loadModuleGraph(ctx); err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("post-copy verification: %w", err))
	}

	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("post-copy verification: %w", err))
	}
	if err := runner.Tidy(ctx, gocli.TidyOptions{Diff: true}); err != nil {
		if errors.Is(err, gocli.ErrTidyRequired) {
			return policyError(stageDependencies, fmt.Errorf("post-copy module metadata does not describe the post-copy tree: %w", err))
		}
		return runtimeError(stageDependencies, fmt.Errorf("post-copy verification tidy: %w", err))
	}

	// Build the post-copy module set from go list -m all. Every record must
	// resolve; an error makes the build graph unknown.
	buildModules, err := runner.ListModules(ctx, "all")
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("post-copy verification: list modules: %w", err))
	}
	buildModuleSet := make(map[string]bool, len(buildModules))
	for _, m := range buildModules {
		if m.Main {
			continue
		}
		if m.Error != nil {
			return runtimeError(stageDependencies, fmt.Errorf(
				"post-copy verification: module %s could not be resolved: %s", m.Path, m.Error.Err))
		}
		buildModuleSet[m.Path] = true
	}

	// Reconcile each accepted candidate's predicted removals against the
	// actual post-copy build.
	for _, candidate := range preCopyResult.Candidates {
		if candidate.Action != deppolicy.ActionCopy || !candidate.Proposed {
			continue
		}
		for _, removed := range candidate.Score.ModulesRemoved {
			if buildModuleSet[removed] {
				return policyError(stageDependencies, fmt.Errorf(
					"post-copy verification: candidate %s predicted module %s would leave the build, but it is still present",
					candidate.ImportPath, removed))
			}
		}
	}

	return nil
}

// checkForbiddenModules refuses a generated module that depends on a module
// the profile explicitly forbids.
//
// The check is exact-match by module path, not a prefix. Five surfaces are
// inspected so a forbidden module cannot hide behind any single layer:
//
//  1. Raw Go imports in all included files (retained and copied). A source
//     file that imports a package belonging to a forbidden module is caught
//     even if the module would be pruned from go.mod by the toolchain.
//  2. Parsed go.mod requirements (the Kept list after tidying).
//  3. Parsed go.sum entries. A forbidden module that appears only in go.sum
//     means the toolchain resolved it, and the published checksum database
//     entry would carry it.
//  4. The complete build module list (go list -m all), which includes
//     indirect dependencies that go.mod may not surface.
//  5. Typed module graph identities. Every package the type checker loaded
//     carries its resolved module path, and a package whose module is
//     forbidden must not appear in the compiled graph.
//
// Module list errors in surface 4 are runtime failures: an unresolvable
// record makes the complete build unknown, so a forbidden module could be
// hiding behind the error. Silently continuing would let it through.
func (r *run) checkForbiddenModules(ctx context.Context) error {
	forbidden := r.cfg.Dependencies.ForbiddenModules
	if len(forbidden) == 0 {
		return nil
	}
	forbiddenSet := make(map[string]bool, len(forbidden))
	for _, m := range forbidden {
		forbiddenSet[m] = true
	}

	// Surface 1: raw Go imports in all included files.
	if err := r.checkForbiddenImports(forbiddenSet); err != nil {
		return err
	}

	// Surface 2: go.mod requirements.
	for _, req := range r.moduleReport.Kept {
		if forbiddenSet[req.Path] {
			return policyError(stageDependencies, fmt.Errorf(
				"the generated module requires forbidden module %s %s", req.Path, req.Version))
		}
	}

	// Surface 3: go.sum entries.
	if err := checkForbiddenGoSum(r.moduleReport.GoSum, forbiddenSet); err != nil {
		return err
	}

	// Surface 4: the complete build via go list -m all, which includes
	// indirect dependencies that the go.mod Kept list may not surface.
	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("forbidden modules: %w", err))
	}
	buildModules, err := runner.ListModules(ctx, "all")
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("forbidden modules: list all: %w", err))
	}
	for _, m := range buildModules {
		if m.Main {
			continue
		}
		if m.Error != nil {
			return runtimeError(stageDependencies, fmt.Errorf(
				"forbidden modules: module %s could not be resolved, so the complete build is unknown: %s", m.Path, m.Error.Err))
		}
		if forbiddenSet[m.Path] {
			return policyError(stageDependencies, fmt.Errorf(
				"the build depends on forbidden module %s (version %s)", m.Path, m.Version))
		}
	}

	// Surface 5: typed module graph identities.
	if err := r.checkForbiddenGraphModules(ctx, forbiddenSet); err != nil {
		return err
	}

	return nil
}

// checkForbiddenImports parses every Go file in the retained and copied
// file sets and refuses any import whose path belongs to a forbidden module.
//
// Belonging is exact module matching with a path boundary: import path P
// belongs to module M when P == M or P starts with M + "/". This prevents
// k8s.io/component-helpersX from matching k8s.io/component-helpers while
// ensuring k8s.io/component-helpers/auth/rbac/validation does match.
func (r *run) checkForbiddenImports(forbiddenSet map[string]bool) error {
	check := func(files []relocate.File) error {
		fset := token.NewFileSet()
		for _, file := range files {
			if !strings.HasSuffix(file.Path, ".go") {
				continue
			}
			f, err := parser.ParseFile(fset, file.Path, file.Contents, parser.ImportsOnly)
			if err != nil {
				return runtimeError(stageDependencies, fmt.Errorf("parse imports in %s: %w", file.Path, err))
			}
			for _, imp := range f.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				for mod := range forbiddenSet {
					if importBelongsToModule(importPath, mod) {
						return policyError(stageDependencies, fmt.Errorf(
							"file %s imports %s, which belongs to forbidden module %s",
							file.Path, importPath, mod))
					}
				}
			}
		}
		return nil
	}

	if err := check(r.post.Files.Files); err != nil {
		return err
	}
	if err := check(r.copyFiles.Files); err != nil {
		return err
	}
	return nil
}

// checkForbiddenGoSum parses go.sum lines and refuses any entry whose module
// path is forbidden. Each go.sum line has the form "module version hash", and
// the module path portion is exact-matched against the forbidden set.
func checkForbiddenGoSum(goSum []byte, forbiddenSet map[string]bool) error {
	if len(goSum) == 0 {
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(goSum))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// go.sum lines have exactly "module version hash". A malformed line is
		// not safe to ignore because it could otherwise terminate or bypass one
		// of the independent forbidden-module evidence surfaces.
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return runtimeError(stageDependencies, fmt.Errorf("go.sum line %d has %d fields, want 3", lineNumber, len(fields)))
		}
		if forbiddenSet[fields[0]] {
			return policyError(stageDependencies, fmt.Errorf(
				"go.sum contains an entry for forbidden module %s", fields[0]))
		}
	}
	if err := scanner.Err(); err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("scan go.sum: %w", err))
	}
	return nil
}

// checkForbiddenGraphModules reloads the typed module graph and refuses any
// package whose resolved module identity is forbidden.
func (r *run) checkForbiddenGraphModules(ctx context.Context, forbiddenSet map[string]bool) error {
	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("forbidden modules graph: %w", err))
	}
	env, err := runner.LoaderEnv(ctx)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("forbidden modules graph: %w", err))
	}
	graph, err := modulegraph.Load(ctx, modulegraph.Options{
		Dir:      r.paths.PostModule,
		Env:      env,
		Patterns: []string{"./..."},
		Redactor: runner.Redactor(),
	})
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("forbidden modules graph: %w", err))
	}
	for _, pkg := range graph.Packages() {
		if pkg.Module == nil {
			continue
		}
		if forbiddenSet[pkg.Module.Path] {
			return policyError(stageDependencies, fmt.Errorf(
				"the typed module graph contains package %s from forbidden module %s",
				pkg.PkgPath, pkg.Module.Path))
		}
	}
	return nil
}

// importBelongsToModule reports whether an import path belongs to a module.
//
// An import path belongs to a module when it equals the module path exactly,
// or when it starts with the module path followed by a "/". This is the
// standard Go module path matching rule and prevents false positives from
// modules that share a common prefix but are distinct (e.g.,
// k8s.io/component-helpers vs k8s.io/component-helpersX).
func importBelongsToModule(importPath, modulePath string) bool {
	if importPath == modulePath {
		return true
	}
	return strings.HasPrefix(importPath, modulePath+"/")
}

// dependencyOptions translates the profile's dependency section into the
// decider's shape.
//
// The identity requirements come from the facade's interface assertions rather
// than from a list of their own. An assertion is a promise that a facade type
// implements a real upstream interface, so the module owning that interface can
// never be copied: a copied interface is a different type, and the assertion
// would then prove something about the copy while consumers pass the original.
func dependencyOptions(cfg *config.Config, sourceTag string) (deppolicy.Options, error) {
	minor, err := sourceMinor(sourceTag)
	if err != nil {
		return deppolicy.Options{}, err
	}
	options := deppolicy.Options{
		ModulePath:     cfg.Destination.Module,
		InternalPrefix: cfg.Destination.InternalPrefix,
		SourceMinor:    minor,
		Policy:         cfg.Dependencies.Policy,
		Proposals:      cfg.Dependencies.CopyPackages,
		Gates: deppolicy.Gates{
			Interoperability: cfg.Dependencies.Gates.Interoperability,
			GlobalState:      cfg.Dependencies.Gates.GlobalState,
			Diamond:          cfg.Dependencies.Gates.Diamond,
			Cost: deppolicy.CostCeilings{
				MaxCopiedPackages:   cfg.Dependencies.Gates.Cost.MaxCopiedPackages,
				MaxCopiedLines:      cfg.Dependencies.Gates.Cost.MaxCopiedLines,
				MaxGeneratedFiles:   cfg.Dependencies.Gates.Cost.MaxGeneratedFiles,
				MaxDistinctLicenses: cfg.Dependencies.Gates.Cost.MaxDistinctLicenses,
				MaxModuleZipBytes:   cfg.Dependencies.Gates.Cost.MaxModuleZipBytes,
				MaxReleasesPerMinor: cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor,
				MinModulesRemoved:   cfg.Dependencies.Gates.Cost.MinModulesRemoved,
				MinPackagesRemoved:  cfg.Dependencies.Gates.Cost.MinPackagesRemoved,
				MinLinesRemoved:     cfg.Dependencies.Gates.Cost.MinLinesRemoved,
			},
		},
	}
	for _, assertion := range cfg.Facade.InterfaceAssertions {
		options.IdentityRequired = append(options.IdentityRequired, assertion.Interface)
	}
	for _, override := range cfg.Dependencies.Overrides {
		expires, err := overrideMinor(override.ExpiresAfter)
		if err != nil {
			return deppolicy.Options{}, fmt.Errorf("dependency override for %s: %w", override.Package, err)
		}
		options.Overrides = append(options.Overrides, deppolicy.Override{
			StagingPath:       override.Package,
			Gate:              override.Gate,
			Justification:     override.Justification,
			Approver:          override.Approver,
			ExpiresAfterMinor: expires,
		})
	}
	return options, nil
}

// sourceMinor reads the Kubernetes minor series out of the upstream release tag.
//
// Overrides expire relative to it, so a relaxation granted for one release
// cannot outlive the reason it was granted.
func sourceMinor(sourceTag string) (int, error) {
	version, err := config.ParseSemver(sourceTag)
	if err != nil {
		return 0, fmt.Errorf("source release %q: %w", sourceTag, err)
	}
	return version.Minor, nil
}

// overrideMinor reads the minor series an override is believed through.
func overrideMinor(expiresAfter string) (int, error) {
	_, minor, err := config.ParseMinorSeries(expiresAfter)
	if err != nil {
		return 0, fmt.Errorf("expiry %q: %w", expiresAfter, err)
	}
	return minor, nil
}

// facadeFiles renders the generated facade as relocated files for composition.
func (r *run) facadeFiles() []relocate.File {
	return r.postFacade.Files
}

// parseKeptRequirements extracts the surviving requirements from a go.mod file.
//
// It is used after a re-tidy to refresh the module report without re-running the
// full verification pass. The requirements are what the provenance phase reads to
// produce module mappings, so they must reflect the tidied state.
func parseKeptRequirements(goMod []byte) ([]gomodmap.Requirement, error) {
	file, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	var kept []gomodmap.Requirement
	for _, require := range file.Require {
		kept = append(kept, gomodmap.Requirement{
			Path:     require.Mod.Path,
			Version:  require.Mod.Version,
			Indirect: require.Indirect,
		})
	}
	slices.SortFunc(kept, func(a, b gomodmap.Requirement) int {
		return cmp.Compare(a.Path, b.Path)
	})
	return kept, nil
}
