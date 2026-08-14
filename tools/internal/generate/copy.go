package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

// ErrCopyNotPortable reports a staging package whose Go files are not the same
// on every platform. A copy that reads only the host-matched files would
// compile on the build machine and fail elsewhere, so this is refused rather
// than approximated.
var ErrCopyNotPortable = errors.New("the candidate has Go files excluded by the current build constraints, so a copy would be host-specific")

// stagingRewriteResult holds the rewritten file set together with the per-file
// changes the rewrite produced. The changes are what provenance records to
// account for every import the engine modified.
type stagingRewriteResult struct {
	files   relocate.FileSet
	changes map[string][]rewrite.Change // keyed by destination path
}

// materializeCopies reads the approved staging packages from the module cache
// directory the graph already resolved and produces a relocated file set.
//
// Each candidate carries the absolute directory where go/packages found its
// source. For a staging module resolved through the proxy that directory lives
// in the module cache; for a staging module resolved through a replacement it
// lives wherever the replacement points. Either way the loader already proved
// the directory holds the files it reported, so reading from it is both correct
// and hermetic.
//
// The function reads through an os.Root opened on each candidate's directory,
// so every file access is contained. Only regular files are accepted; a
// symbolic link, device node, or directory with a file's name is refused
// because git records none of those as a blob and the materialized tree would
// disagree with the provenance that describes it.
func (r *run) materializeCopies(ctx context.Context, result *deppolicy.Result, graph *deppolicy.Graph) (relocate.FileSet, error) {
	if len(result.Copy) == 0 {
		return relocate.FileSet{}, nil
	}

	// Index the graph candidates by staging path for file list access.
	// Refuse a duplicate rather than silently taking the last one.
	candidateIndex := make(map[string]deppolicy.Candidate, len(graph.Candidates))
	for _, candidate := range graph.Candidates {
		if _, exists := candidateIndex[candidate.StagingPath]; exists {
			return relocate.FileSet{}, fmt.Errorf("copy: duplicate candidate staging path %s in graph", candidate.StagingPath)
		}
		candidateIndex[candidate.StagingPath] = candidate
	}

	var plan relocate.Plan
	for _, stagingPath := range result.Copy {
		if err := ctx.Err(); err != nil {
			return relocate.FileSet{}, fmt.Errorf("copy staging packages: %w", err)
		}
		candidate, ok := candidateIndex[stagingPath]
		if !ok {
			return relocate.FileSet{}, fmt.Errorf("copy %s: approved staging path has no graph candidate", stagingPath)
		}

		// Resolve the evidence for this staging path to validate the
		// candidate's identity and directory.
		ev, err := evidenceForStagingPath(r.copyEvidence, stagingPath)
		if err != nil {
			return relocate.FileSet{}, fmt.Errorf("copy %s: %w", stagingPath, err)
		}

		if candidate.Package == nil {
			return relocate.FileSet{}, fmt.Errorf("copy %s: candidate has no loaded package", stagingPath)
		}

		// Require the candidate's module path matches the evidence.
		if candidate.Package.Module != ev.ModulePath {
			return relocate.FileSet{}, fmt.Errorf("copy %s: candidate module %s does not match evidence module %s",
				stagingPath, candidate.Package.Module, ev.ModulePath)
		}

		// Compute the module-relative package suffix.
		relSuffix := strings.TrimPrefix(stagingPath, ev.StagingDir+"/")
		if relSuffix == stagingPath {
			relSuffix = "" // package IS the module root
		}

		// Require the candidate's import path matches the evidence module +
		// relative suffix exactly.
		expectedImport := ev.ModulePath
		if relSuffix != "" {
			expectedImport = ev.ModulePath + "/" + relSuffix
		}
		if candidate.Package.ImportPath != expectedImport {
			return relocate.FileSet{}, fmt.Errorf("copy %s: candidate import path %s does not match expected %s",
				stagingPath, candidate.Package.ImportPath, expectedImport)
		}

		// Require the candidate's directory equals exactly the measured
		// module dir joined with the relative suffix, after symlink
		// resolution. A sibling package inside the same module must not
		// satisfy evidence for another proposed path.
		candidateDir, err := filepath.EvalSymlinks(candidate.Package.Dir)
		if err != nil {
			return relocate.FileSet{}, fmt.Errorf("copy %s: eval symlinks %s: %w", stagingPath, candidate.Package.Dir, err)
		}
		moduleDir, err := filepath.EvalSymlinks(ev.ModuleDir)
		if err != nil {
			return relocate.FileSet{}, fmt.Errorf("copy %s: eval symlinks %s: %w", stagingPath, ev.ModuleDir, err)
		}
		expectedDir := moduleDir
		if relSuffix != "" {
			expectedDir = filepath.Join(moduleDir, filepath.FromSlash(relSuffix))
		}
		if candidateDir != expectedDir {
			return relocate.FileSet{}, fmt.Errorf("copy %s: candidate directory %s does not match expected %s",
				stagingPath, candidateDir, expectedDir)
		}

		files, err := readCandidatePackage(ctx, stagingPath, candidate.Package)
		if err != nil {
			return relocate.FileSet{}, fmt.Errorf("copy %s: %w", stagingPath, err)
		}
		plan.Files = append(plan.Files, files...)
	}

	set, err := relocate.Build(ctx, plan, relocate.Options{
		InternalPrefix: r.cfg.Destination.InternalPrefix,
		Symlinks:       relocate.SymlinkReject,
	})
	if err != nil {
		return relocate.FileSet{}, fmt.Errorf("copy relocation: %w", err)
	}
	set, err = normalizeCopySources(set, r.copyEvidence)
	if err != nil {
		return relocate.FileSet{}, fmt.Errorf("copy source identity: %w", err)
	}
	return set, nil
}

// normalizeCopySources changes provenance source paths from Kubernetes-root
// staging paths to paths relative to the separate staging repository that
// supplied the measured bytes. Destination paths retain the full staging/src
// path so nested internal visibility is unchanged.
func normalizeCopySources(set relocate.FileSet, evidence map[string]copyEvidence) (relocate.FileSet, error) {
	for i := range set.Packages {
		ev, err := evidenceForStagingPath(evidence, set.Packages[i].Source)
		if err != nil {
			return relocate.FileSet{}, err
		}
		relative, err := moduleRelativePath(set.Packages[i].Source, ev.StagingDir)
		if err != nil {
			return relocate.FileSet{}, err
		}
		set.Packages[i].Source = relative
	}
	for i := range set.Files {
		ev, err := evidenceForStagingPath(evidence, set.Files[i].SourcePackage)
		if err != nil {
			return relocate.FileSet{}, err
		}
		sourcePackage, err := moduleRelativePath(set.Files[i].SourcePackage, ev.StagingDir)
		if err != nil {
			return relocate.FileSet{}, err
		}
		source, err := moduleRelativePath(set.Files[i].Source, ev.StagingDir)
		if err != nil {
			return relocate.FileSet{}, err
		}
		set.Files[i].SourcePackage = sourcePackage
		set.Files[i].Source = source
	}
	return set, nil
}

func moduleRelativePath(name, stagingDir string) (string, error) {
	if name == stagingDir {
		return ".", nil
	}
	relative, ok := strings.CutPrefix(name, stagingDir+"/")
	if !ok || relative == "" {
		return "", fmt.Errorf("path %q is not beneath staging module %q", name, stagingDir)
	}
	return relative, nil
}

// readCandidatePackage reads exactly the Go and other files the loaded graph
// reported for one staging package from the candidate's resolved directory.
//
// The file list comes from the type checked graph rather than from a directory
// listing, which is what makes the copy deterministic: a file the toolchain did
// not build is not copied, so a stale test helper, an editor backup, or a file
// the build constraints exclude cannot reach the generated module.
//
// Portability is fail-closed. When the loader reports IgnoredFiles for this
// package, the package has Go files that do not match the current GOOS/GOARCH.
// A copy that omits them compiles on the build host and fails elsewhere, so the
// function refuses rather than producing a host-specific tree. A pure leaf
// utility like policy_comparator.go has no build constraints and no ignored
// files, so this check costs nothing for the expected shape.
func readCandidatePackage(ctx context.Context, stagingPath string, pkg *deppolicy.Package) ([]relocate.PlanFile, error) {
	if err := config.ValidateRelPath(stagingPath); err != nil {
		return nil, fmt.Errorf("staging path: %w", err)
	}
	if pkg == nil {
		return nil, fmt.Errorf("candidate has no loaded package")
	}
	if pkg.Dir == "" {
		return nil, fmt.Errorf("candidate has no resolved directory")
	}

	// Fail closed on build-constrained files. A copy that skips them would
	// compile only on this host.
	if len(pkg.IgnoredFiles) > 0 {
		return nil, fmt.Errorf("%w: %d ignored files: %s",
			ErrCopyNotPortable, len(pkg.IgnoredFiles), strings.Join(pkg.IgnoredFiles, ", "))
	}

	var allFiles []string
	allFiles = append(allFiles, pkg.GoFiles...)
	allFiles = append(allFiles, pkg.OtherFiles...)
	if len(allFiles) == 0 {
		return nil, fmt.Errorf("the candidate reports no files")
	}

	// Validate and deduplicate file names. The adapter normally supplies
	// base names, but the materializer is a security boundary and tests
	// construct graphs directly.
	if err := validateBaseNames(allFiles); err != nil {
		return nil, err
	}
	slices.Sort(allFiles)

	// Open the resolved package directory as a root for contained reads.
	root, err := os.OpenRoot(pkg.Dir)
	if err != nil {
		return nil, fmt.Errorf("open package directory: %w", err)
	}
	defer root.Close()

	planFiles := make([]relocate.PlanFile, 0, len(allFiles))
	for _, baseName := range allFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// The file lives directly in pkg.Dir; the plan path uses the full
		// staging path so relocation preserves the upstream directory structure.
		info, err := root.Lstat(baseName)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", baseName, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("inspect %s: unsupported mode %v, only regular files are accepted", baseName, info.Mode())
		}
		mode, err := relocate.ModeOf(info.Mode())
		if err != nil {
			return nil, fmt.Errorf("mode %s: %w", baseName, err)
		}
		contents, err := root.ReadFile(baseName)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", baseName, err)
		}

		generated := false
		if strings.HasSuffix(baseName, ".go") {
			generated = rewrite.Generated(contents)
		}

		relPath := stagingPath + "/" + baseName
		planFiles = append(planFiles, relocate.PlanFile{
			Path:      relPath,
			Package:   stagingPath,
			Mode:      mode,
			Contents:  contents,
			Generated: generated,
		})
	}
	return planFiles, nil
}

// rewriteStagingImports rewrites imports of copied staging modules in a file
// set. This must be applied to BOTH the copy files AND the retained extracted
// files, because retained code that imports the staging module needs to point
// at the relocated copy for the requirement to actually drop.
//
// The noNotice parameter controls whether the rewrite inserts a modification
// notice. Retained extracted files already carry a notice from the extraction
// rewrite and must not receive a second one (noNotice=true). Copied files have
// no prior notice and should receive one when their imports change
// (noNotice=false).
//
// The returned changes map records every import rewrite the pass made, keyed by
// destination path. The caller merges these into provenance so the per-package
// record accounts for every modification the engine applied.
//
// The mapping for each staging module with copied packages is:
//
//	k8s.io/component-helpers/auth/rbac/validation
//	    becomes
//	<module>/internal/kk/staging/src/k8s.io/component-helpers/auth/rbac/validation
func (r *run) rewriteStagingImports(ctx context.Context, set relocate.FileSet, copiedPaths []string, noNotice bool) (stagingRewriteResult, error) {
	// Require every copied path maps to exactly one measured evidence/module.
	stagingModules, err := r.copiedStagingModules(copiedPaths)
	if err != nil {
		return stagingRewriteResult{}, err
	}
	if len(stagingModules) == 0 {
		return stagingRewriteResult{files: set}, nil
	}

	allChanges := map[string][]rewrite.Change{}

	// Apply one rewrite pass per staging module. Each pass rewrites only the
	// imports rooted at that module's path; others pass through unchanged.
	for _, sm := range stagingModules {
		var changes map[string][]rewrite.Change
		var err error
		set, changes, err = rewriteForStagingModule(ctx, set, r.cfg, sm, noNotice)
		if err != nil {
			return stagingRewriteResult{}, err
		}
		for path, ch := range changes {
			allChanges[path] = append(allChanges[path], ch...)
		}
	}
	return stagingRewriteResult{files: set, changes: allChanges}, nil
}

// stagingModuleInfo identifies one staging module that has copied packages,
// together with its measured Origin for notice stamping.
type stagingModuleInfo struct {
	modulePath string // e.g., k8s.io/component-helpers
	dir        string // e.g., staging/src/k8s.io/component-helpers
	originURL  string // validated Git repository URL
	originHash string // validated Git commit hash
}

// copiedStagingModules identifies the distinct staging modules that own at
// least one copied package. Every copied path must map to exactly one measured
// evidence record. A path with no match is refused because it would produce a
// copy with no provenance.
func (r *run) copiedStagingModules(copiedPaths []string) ([]stagingModuleInfo, error) {
	if len(copiedPaths) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var modules []stagingModuleInfo
	for _, copied := range copiedPaths {
		found := false
		for _, ev := range r.copyEvidence {
			if copied == ev.StagingDir || strings.HasPrefix(copied, ev.StagingDir+"/") {
				if !seen[ev.ModulePath] {
					seen[ev.ModulePath] = true
					modules = append(modules, stagingModuleInfo{
						modulePath: ev.ModulePath,
						dir:        ev.StagingDir,
						originURL:  ev.OriginURL,
						originHash: ev.OriginHash,
					})
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("copied staging path %q has no measured evidence record", copied)
		}
	}
	return modules, nil
}

// rewriteForStagingModule rewrites imports of one staging module into their
// relocated form across all Go files in the set.
//
// For copied files (noNotice=false), the modification notice uses the measured
// Origin URL and commit from the staging module info so the notice and
// provenance agree. For retained files (noNotice=true), no notice is added.
func rewriteForStagingModule(ctx context.Context, set relocate.FileSet, cfg *config.Config, sm stagingModuleInfo, noNotice bool) (relocate.FileSet, map[string][]rewrite.Change, error) {
	internalPrefix := cfg.Destination.InternalPrefix + "/" + sm.dir

	opts := rewrite.Options{
		SourcePrefix:      sm.modulePath,
		DestinationModule: cfg.Destination.Module,
		InternalPrefix:    internalPrefix,
		LineEndings:       rewrite.LineEndingReject,
		NoNotice:          noNotice,
		SourceRepository:  sm.originURL,
		SourceSHA:         sm.originHash,
	}

	changes := map[string][]rewrite.Change{}
	rewritten := make([]relocate.File, 0, len(set.Files))
	for _, file := range set.Files {
		if err := ctx.Err(); err != nil {
			return relocate.FileSet{}, nil, err
		}
		if !strings.HasSuffix(file.Path, ".go") {
			rewritten = append(rewritten, file)
			continue
		}
		result, err := rewrite.GoFile(ctx, rewrite.File{
			Path:       file.Path,
			SourcePath: file.Source,
			Contents:   file.Contents,
			Generated:  file.Generated,
		}, opts)
		if err != nil {
			return relocate.FileSet{}, nil, fmt.Errorf("%s: %w", file.Path, err)
		}
		file.Contents = result.Contents
		rewritten = append(rewritten, file)
		if len(result.Changes) > 0 {
			changes[file.Path] = result.Changes
		}
	}

	return relocate.FileSet{Files: rewritten, Packages: set.Packages}, changes, nil
}

// validateBaseNames checks that every file name in the list is a valid base
// name: no slashes, no dot/dotdot traversal, no flag-like prefixes, and no
// duplicates. The adapter normally supplies clean base names from go/packages,
// but the materializer is a security boundary that must not trust upstream
// shapes.
func validateBaseNames(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" {
			return fmt.Errorf("file list contains an empty name")
		}
		if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
			return fmt.Errorf("file name %q contains a path separator", name)
		}
		if name == "." || name == ".." {
			return fmt.Errorf("file name %q is a traversal", name)
		}
		if strings.HasPrefix(name, ".") {
			return fmt.Errorf("file name %q starts with a dot", name)
		}
		if strings.HasPrefix(name, "-") {
			return fmt.Errorf("file name %q starts with a dash", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate file name %q", name)
		}
		seen[name] = true
	}
	return nil
}
