package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

// copyEvidence records the measured identity and provenance of one staging
// module whose packages are proposed for copying.
//
// It is built once by measureCandidateModuleFacts and consumed by
// materializeCopies, addCopyProvenance, and the rewrite notice stamping,
// so every downstream step uses the same measured source rather than
// reconstructing it from unordered maps.
type copyEvidence struct {
	// StagingDir is the Kubernetes-root-relative staging directory, such as
	// staging/src/k8s.io/component-helpers.
	StagingDir string
	// ModulePath is the Go module path, such as k8s.io/component-helpers.
	ModulePath string
	// Version is the resolved version, such as v0.36.1.
	Version string
	// Commit is the staging commit the version names.
	Commit string
	// OriginURL is the validated Git repository URL from the module's Origin.
	OriginURL string
	// OriginHash is the validated Git commit hash from the module's Origin.
	OriginHash string
	// ModuleDir is the absolute extracted module directory in the cache.
	ModuleDir string
	// DestinationPrefix is where this staging module lives in the generated
	// module, including the configured internal prefix and staging directory.
	DestinationPrefix string
	// Licenses are the collected licence files, verified against the
	// configured identifier.
	Licenses []provenance.LicenseFile
}

// addCopyProvenance builds provenance records for the copied packages, renders
// them as SOAPBOX_PROVENANCE.txt files beside each package, and composes them
// into r.copyFiles so the provenance cross-check accounts for every file in
// the tree.
//
// Each copied package gets a record naming the staging module it was read from,
// at the version and commit the staging resolution pinned. This is the honest
// source: the file bytes came from the module cache at that version, not from
// the Kubernetes root tree.
func (r *run) addCopyProvenance(evidence map[string]copyEvidence, changes map[string][]rewrite.Change) error {
	var provenanceFiles []relocate.File
	for _, pkg := range r.copyFiles.Packages {
		// Find the evidence for the staging module this package belongs to.
		// The destination prefix determines which evidence record applies:
		// a package at internal/kk/staging/src/k8s.io/helpers/text/policy
		// belongs to the evidence whose StagingDir is
		// staging/src/k8s.io/helpers.
		ev, err := evidenceForPackage(evidence, pkg)
		if err != nil {
			return fmt.Errorf("copy provenance %s: %w", pkg.Path, err)
		}

		record := rewrite.NewPackageProvenance(pkg.Path, pkg.Source, rewrite.Options{
			SourceRepository: ev.OriginURL,
			SourceSHA:        ev.OriginHash,
		})
		for _, filePath := range pkg.Files {
			file, ok := r.copyFiles.Lookup(filePath)
			if !ok {
				continue
			}
			record.AddFile(rewrite.File{
				Path:       file.Path,
				SourcePath: file.Source,
				Generated:  file.Generated,
			}, rewrite.Result{
				Contents: file.Contents,
				Changes:  changes[file.Path],
			})
		}
		r.post.Provenance = append(r.post.Provenance, record)

		// Render the record as a file beside the copied package.
		provenanceFiles = append(provenanceFiles, relocate.File{
			Path:     path.Join(pkg.Path, rewrite.ProvenanceFileName),
			Mode:     relocate.ModeRegular,
			Contents: []byte(record.Render()),
		})
	}

	// Compose per-package provenance into the copy set. Licence, notice, and
	// patent documents are composed later from the actual CopiedPackage list so
	// evidence for a proposed-but-refused candidate cannot enter the tree.
	if len(provenanceFiles) > 0 {
		composed, err := r.copyFiles.With(provenanceFiles...)
		if err != nil {
			return fmt.Errorf("copy provenance: %w", err)
		}
		r.copyFiles = composed
	}
	return nil
}

// evidenceForPackage finds the exactly one evidence record whose generated
// destination prefix contains a copied package. Source paths are deliberately
// module-relative by this stage, so the destination—not Source—is the stable
// link back to the Kubernetes staging directory.
func evidenceForPackage(evidence map[string]copyEvidence, pkg relocate.Package) (copyEvidence, error) {
	var matches []copyEvidence
	for _, e := range evidence {
		if pkg.Path == e.DestinationPrefix || strings.HasPrefix(pkg.Path, e.DestinationPrefix+"/") {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return copyEvidence{}, fmt.Errorf("no measured evidence matches destination %q", pkg.Path)
	case 1:
		return matches[0], nil
	default:
		paths := make([]string, len(matches))
		for i, m := range matches {
			paths[i] = m.ModulePath
		}
		return copyEvidence{}, fmt.Errorf("ambiguous evidence: destination %q matches %s", pkg.Path, strings.Join(paths, ", "))
	}
}

// evidenceForStagingPath finds the exactly one evidence record whose
// StagingDir contains the given staging path.
func evidenceForStagingPath(evidence map[string]copyEvidence, stagingPath string) (copyEvidence, error) {
	var matches []copyEvidence
	for _, e := range evidence {
		if stagingPath == e.StagingDir || strings.HasPrefix(stagingPath, e.StagingDir+"/") {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return copyEvidence{}, fmt.Errorf("no measured evidence for staging path %q", stagingPath)
	case 1:
		return matches[0], nil
	default:
		return copyEvidence{}, fmt.Errorf("ambiguous evidence for staging path %q", stagingPath)
	}
}

// mergeRetainedChanges appends the staging rewrite changes to the existing
// extraction provenance records for retained files and re-renders the
// per-package SOAPBOX_PROVENANCE.txt files in r.post.Files so the committed
// text matches the updated records.
//
// The extraction phase already recorded each file's Changes (the
// k8s.io/kubernetes -> <module>/internal/kk rewrites). The staging rewrite
// adds more changes (the k8s.io/component-helpers -> <module>/internal/kk/
// staging/src/k8s.io/component-helpers rewrites). Both sets belong in the
// provenance record because they are both modifications the engine applied.
func (r *run) mergeRetainedChanges(changes map[string][]rewrite.Change) {
	if len(changes) == 0 {
		return
	}
	// Track which package records were updated so we re-render only those.
	updatedPkgs := map[string]bool{}
	for _, record := range r.post.Provenance {
		for i, fileProv := range record.Files {
			if ch, ok := changes[fileProv.Path]; ok {
				record.Files[i].Changes = append(record.Files[i].Changes, ch...)
				updatedPkgs[record.Package] = true
			}
		}
	}

	// Re-render the SOAPBOX_PROVENANCE.txt for each updated package in the
	// extracted file set so the text the tree commits reflects the merged
	// changes.
	if len(updatedPkgs) == 0 {
		return
	}
	for _, record := range r.post.Provenance {
		if !updatedPkgs[record.Package] {
			continue
		}
		provPath := path.Join(record.Package, rewrite.ProvenanceFileName)
		for i, file := range r.post.Files.Files {
			if file.Path == provPath {
				r.post.Files.Files[i].Contents = []byte(record.Render())
				break
			}
		}
	}
}

// measureCandidateModuleFacts downloads the staging modules that contain
// proposed copy candidates and measures the facts the cost gates need.
//
// It returns both the deppolicy facts (for the cost gates) and a map of
// per-module evidence (for provenance, notices, and licence composition).
// The evidence map is keyed by module path so every downstream consumer
// can look up the measured source identity without reconstructing it.
//
// Every fact is measured rather than assumed. Zip bytes are the stat of the
// downloaded archive. Release cadence is the count of v0.<sourceMinor>.*
// versions the proxy lists. Licence identity is verified by running
// provenance.Collect over the downloaded module directory and calling
// provenance.VerifyLicense against the configured identifier.
//
// Measurement failures are runtime errors, not evidence that a fact is
// unknown. A network timeout, a filesystem error, or a parse failure stops the
// run so the operator can fix the condition.
func (r *run) measureCandidateModuleFacts(ctx context.Context, proposals []string) ([]deppolicy.Module, map[string]copyEvidence, error) {
	if len(proposals) == 0 {
		return nil, nil, nil
	}

	// Identify the distinct staging modules that own at least one proposal.
	type staged struct {
		modulePath string
		version    string
		dir        string // staging dir, e.g. staging/src/k8s.io/component-helpers
		packages   []string
	}
	seen := map[string]*staged{}
	var modules []*staged
	for _, sm := range r.root.Staging {
		for _, proposal := range proposals {
			if proposal == sm.Dir || strings.HasPrefix(proposal, sm.Dir+"/") {
				s, ok := seen[sm.Path]
				if !ok {
					version := ""
					for _, mv := range r.staging {
						if mv.Path == sm.Path {
							version = mv.Version
							break
						}
					}
					if version == "" {
						return nil, nil, fmt.Errorf("measure module facts: no resolved version for staging module %s", sm.Path)
					}
					s = &staged{modulePath: sm.Path, version: version, dir: sm.Dir}
					seen[sm.Path] = s
					modules = append(modules, s)
				}
				s.packages = append(s.packages, proposal)
				break
			}
		}
	}
	if len(modules) == 0 {
		return nil, nil, nil
	}

	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return nil, nil, fmt.Errorf("measure module facts: %w", err)
	}
	queries := make([]string, len(modules))
	for i, m := range modules {
		queries[i] = m.modulePath + "@" + m.version
	}
	downloads, err := runner.Download(ctx, queries...)
	if err != nil {
		return nil, nil, fmt.Errorf("measure module facts: download: %w", err)
	}
	// Index by path@version, rejecting duplicates.
	downloadIndex := make(map[string]gocli.DownloadedModule, len(downloads))
	for _, d := range downloads {
		if d.Error != "" {
			return nil, nil, fmt.Errorf("measure module facts: download %s: %s", d.Path, d.Error)
		}
		key := d.Path + "@" + d.Version
		if _, dup := downloadIndex[key]; dup {
			return nil, nil, fmt.Errorf("measure module facts: duplicate download record for %s", key)
		}
		downloadIndex[key] = d
	}

	// Query module metadata to get Origin for each downloaded module.
	// Each query must return exactly one record matching the queried
	// path@version. Duplicates and extras are refused.
	originIndex := make(map[string]*gocli.ModuleOrigin, len(modules))
	listed, err := runner.ListModules(ctx, queries...)
	if err != nil {
		return nil, nil, fmt.Errorf("measure module facts: list modules for origin: %w", err)
	}
	for _, lm := range listed {
		if lm.Error != nil {
			return nil, nil, fmt.Errorf("measure module facts: list module %s: %s", lm.Path, lm.Error.Err)
		}
		key := lm.Path + "@" + lm.Version
		if _, dup := originIndex[key]; dup {
			return nil, nil, fmt.Errorf("measure module facts: duplicate module record for %s", key)
		}
		originIndex[key] = lm.Origin
	}
	// Every queried module must have returned exactly one record.
	for _, q := range queries {
		if _, ok := originIndex[q]; !ok {
			return nil, nil, fmt.Errorf("measure module facts: list modules did not return a record for %s", q)
		}
	}
	if len(originIndex) != len(queries) {
		return nil, nil, fmt.Errorf("measure module facts: list modules returned %d records for %d queries", len(originIndex), len(queries))
	}

	minor, err := sourceMinor(r.opts.sourceRelease())
	if err != nil {
		return nil, nil, fmt.Errorf("measure module facts: %w", err)
	}
	cadenceCutoff, err := config.MapReleaseTag(r.cfg.Release.Policy, r.opts.sourceRelease())
	if err != nil {
		return nil, nil, fmt.Errorf("measure module facts: cadence cutoff: %w", err)
	}

	var facts []deppolicy.Module
	evidence := make(map[string]copyEvidence, len(modules))
	for _, m := range modules {
		key := m.modulePath + "@" + m.version
		d, ok := downloadIndex[key]
		if !ok {
			return nil, nil, fmt.Errorf("measure module facts: download did not return %s", key)
		}
		if d.Path != m.modulePath || d.Version != m.version {
			return nil, nil, fmt.Errorf("measure module facts: download returned %s@%s for query %s",
				d.Path, d.Version, key)
		}
		if d.Dir == "" {
			return nil, nil, fmt.Errorf("measure module facts: download %s has no extracted directory", key)
		}
		if d.Zip == "" {
			return nil, nil, fmt.Errorf("measure module facts: download %s has no zip path", key)
		}

		// Zip: Lstat the archive, require a regular file, then take its size.
		zipInfo, err := os.Lstat(d.Zip)
		if err != nil {
			return nil, nil, fmt.Errorf("measure module facts: stat zip %s: %w", d.Zip, err)
		}
		if !zipInfo.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("measure module facts: zip %s is not a regular file (mode %v)", d.Zip, zipInfo.Mode())
		}

		// Cadence: count versions by semantic parse, not string prefix.
		versionResults, err := runner.ListModuleVersions(ctx, m.modulePath+"@"+m.version)
		if err != nil {
			return nil, nil, fmt.Errorf("measure module facts: list versions %s: %w", m.modulePath, err)
		}
		cadence, err := countMinorCadence(versionResults, m.modulePath, m.version, minor, cadenceCutoff)
		if err != nil {
			return nil, nil, fmt.Errorf("measure module facts: %w", err)
		}

		// Look up the staging commit from the version index.
		commit := ""
		for _, mv := range r.staging {
			if mv.Path == m.modulePath {
				commit = mv.Commit
				break
			}
		}

		// Origin: validate the module's Git origin against the pinned commit.
		origin := originIndex[key]
		originURL, originHash, err := validateStagingOrigin(origin, m.modulePath, m.version, commit, r.cfg.Source.Repository)
		if err != nil {
			return nil, nil, fmt.Errorf("measure module facts: %w", err)
		}

		// Licence: use provenance.Collect over the downloaded module directory
		// with os.OpenRoot for containment.
		relPackages, err := relativeStagingPackages(m.packages, m.dir)
		if err != nil {
			return nil, nil, fmt.Errorf("measure module facts: %w", err)
		}
		root, err := os.OpenRoot(d.Dir)
		if err != nil {
			return nil, nil, fmt.Errorf("measure module facts: open module root %s: %w", d.Dir, err)
		}
		licenseFiles, collectErr := provenance.Collect(ctx, provenance.CollectOptions{
			FS:             root.FS(),
			ModuleRoot:     ".",
			Packages:       relPackages,
			InternalPrefix: r.cfg.Destination.InternalPrefix + "/" + m.dir,
		})
		closeErr := root.Close()
		if collectErr != nil || closeErr != nil {
			var wrapped []error
			if collectErr != nil {
				wrapped = append(wrapped, fmt.Errorf("measure module facts: collect licences %s: %w", m.modulePath, collectErr))
			}
			if closeErr != nil {
				wrapped = append(wrapped, fmt.Errorf("measure module facts: close module root %s: %w", d.Dir, closeErr))
			}
			return nil, nil, errors.Join(wrapped...)
		}
		var licenses []deppolicy.License
		licensesVerified := false
		var grantFiles []string
		for _, lf := range licenseFiles {
			if provenance.StatesGrant(lf.Name) {
				if err := provenance.VerifyLicense(r.cfg.Source.License, lf.Contents); err != nil {
					return nil, nil, fmt.Errorf("measure module facts: verify licence %s in %s: %w", lf.SourcePath, m.modulePath, err)
				}
				grantFiles = append(grantFiles, lf.SourcePath)
				licensesVerified = true
			}
		}
		if licensesVerified {
			licenses = []deppolicy.License{{
				Identifier: r.cfg.Source.License,
				Files:      grantFiles,
			}}
		}

		facts = append(facts, deppolicy.Module{
			Path:             m.modulePath,
			Version:          m.version,
			Dir:              d.Dir,
			ZipBytes:         zipInfo.Size(),
			ZipBytesKnown:    true,
			ReleasesPerMinor: cadence,
			CadenceKnown:     true,
			Licenses:         licenses,
			LicensesVerified: licensesVerified,
		})
		evidence[m.modulePath] = copyEvidence{
			StagingDir:        m.dir,
			ModulePath:        m.modulePath,
			Version:           m.version,
			Commit:            commit,
			OriginURL:         originURL,
			OriginHash:        originHash,
			ModuleDir:         d.Dir,
			DestinationPrefix: r.cfg.Destination.InternalPrefix + "/" + m.dir,
			Licenses:          licenseFiles,
		}
	}
	return facts, evidence, nil
}

// countMinorCadence counts versions matching major=0/minor=sourceMinor through
// the release-bounded cutoff. Later releases cannot change the evidence for an
// already generated source commit.
//
// It requires exactly one successful ModuleWithVersions record that matches
// the queried module path and contains the resolved version.
func countMinorCadence(results []gocli.ModuleWithVersions, modulePath, version string, sourceMinor int, cutoff ...string) (int, error) {
	if len(results) == 0 {
		return 0, fmt.Errorf("cadence %s: version query returned no records", modulePath)
	}
	if len(results) > 1 {
		return 0, fmt.Errorf("cadence %s: version query returned %d records, want exactly 1", modulePath, len(results))
	}
	vm := results[0]
	if vm.Error != nil {
		return 0, fmt.Errorf("cadence %s: %s", modulePath, vm.Error.Err)
	}
	if vm.Path != modulePath {
		return 0, fmt.Errorf("cadence: version list returned %s, want %s", vm.Path, modulePath)
	}
	// Tagged resolutions must appear in the release list. A pseudo-version is
	// identified by its exact Origin hash instead; proxies list semantic tags,
	// not every commit-derived pseudo-version.
	if !module.IsPseudoVersion(version) && !slices.Contains(vm.Versions, version) {
		return 0, fmt.Errorf("cadence %s: version list does not contain resolved version %s", modulePath, version)
	}
	through := "v999999.0.0"
	if len(cutoff) > 1 {
		return 0, fmt.Errorf("cadence %s: got %d cutoffs, want at most one", modulePath, len(cutoff))
	}
	if len(cutoff) == 1 {
		through = cutoff[0]
	}
	if !semver.IsValid(through) {
		return 0, fmt.Errorf("cadence %s: cutoff %q is not semantic version", modulePath, through)
	}
	cadence := 0
	for _, v := range vm.Versions {
		sv, err := config.ParseSemver(v)
		if err != nil {
			// go list -m -versions returns valid semantic versions. A
			// malformed entry is protocol corruption, not a version to skip:
			// silently dropping it would lower the measured cadence.
			return 0, fmt.Errorf("cadence %s: malformed version %q in version list: %w", modulePath, v, err)
		}
		if semver.Compare(v, through) <= 0 && sv.Major == 0 && sv.Minor == sourceMinor {
			cadence++
		}
	}
	return cadence, nil
}

// validateStagingOrigin checks that a module's Origin proves it came from the
// canonical Kubernetes staging repository at the pinned commit.
//
// For a release version the checks are:
//   - VCS must be "git".
//   - Hash must equal the pinned staging commit from the version index.
//   - Ref must be "refs/tags/<version>", proving the version came from a tag.
//   - Subdir must be empty, because each staging module is its own repository.
//   - URL must be the canonical staging repo: https://github.com/kubernetes/<basename>
//     where <basename> is the last element of the module path, with optional .git suffix.
//
// For a pseudo-version, the same URL and exact hash checks apply. Origin.Ref
// may be empty or a validated refs/heads/* name, but never a tag claim: the
// immutable identity is the mapped staging commit recorded in the index.
//
// A nil or empty Origin is refused because it means the proxy provided no
// provenance at all, and publishing a copy with no evidence of where the bytes
// came from is exactly what this gate exists to prevent.
func validateStagingOrigin(origin *gocli.ModuleOrigin, modulePath, version, pinnedCommit, sourceRepo string) (originURL, originHash string, err error) {
	if origin == nil {
		return "", "", fmt.Errorf("origin %s@%s: the proxy reported no Origin, so the copy has no provenance", modulePath, version)
	}
	if origin.VCS != "git" {
		return "", "", fmt.Errorf("origin %s@%s: VCS is %q, want git", modulePath, version, origin.VCS)
	}
	if origin.Hash == "" {
		return "", "", fmt.Errorf("origin %s@%s: Origin has no commit hash", modulePath, version)
	}
	if origin.Hash != pinnedCommit {
		return "", "", fmt.Errorf("origin %s@%s: Origin hash %s does not match pinned staging commit %s", modulePath, version, origin.Hash, pinnedCommit)
	}
	if module.IsPseudoVersion(version) {
		// A pseudo-version is proved by its exact Origin hash. If Go also reports
		// the branch used to discover it, keep that evidence constrained to the
		// heads namespace; a pseudo-version claiming a tag ref is contradictory.
		if origin.Ref != "" {
			if err := gitcli.ValidateRefName(origin.Ref); err != nil {
				return "", "", fmt.Errorf("origin %s@%s: Ref: %w", modulePath, version, err)
			}
			if !strings.HasPrefix(origin.Ref, "refs/heads/") {
				return "", "", fmt.Errorf("origin %s@%s: pseudo-version Ref is %q, want empty or refs/heads/*", modulePath, version, origin.Ref)
			}
		}
	} else {
		expectedRef := "refs/tags/" + version
		if origin.Ref != expectedRef {
			return "", "", fmt.Errorf("origin %s@%s: Ref is %q, want %q", modulePath, version, origin.Ref, expectedRef)
		}
	}
	if origin.Subdir != "" {
		return "", "", fmt.Errorf("origin %s@%s: Subdir is %q, want empty for a separate staging repository", modulePath, version, origin.Subdir)
	}

	// Validate URL is the canonical Kubernetes staging repo.
	basename := modulePath
	if idx := strings.LastIndex(modulePath, "/"); idx >= 0 {
		basename = modulePath[idx+1:]
	}
	normalized := strings.TrimSuffix(sourceRepo, ".git")
	orgIdx := strings.LastIndex(normalized, "/")
	if orgIdx < 0 {
		return "", "", fmt.Errorf("origin %s@%s: source repository %q has no path separator, cannot derive staging org", modulePath, version, sourceRepo)
	}
	orgURL := normalized[:orgIdx+1]
	expectedURL := orgURL + basename
	originNormalized := strings.TrimSuffix(origin.URL, ".git")
	if originNormalized != expectedURL {
		return "", "", fmt.Errorf("origin %s@%s: URL %q does not match expected canonical staging repo %q", modulePath, version, origin.URL, expectedURL)
	}

	return origin.URL, origin.Hash, nil
}

// relativeStagingPackages converts staging paths to paths relative to the
// module root directory for provenance.Collect.
//
// Every path must be equal to or beneath moduleDir. A path that is not is
// refused rather than silently mapped to ".", because it would measure the
// wrong module's licence tree.
func relativeStagingPackages(stagingPaths []string, moduleDir string) ([]string, error) {
	result := make([]string, 0, len(stagingPaths))
	for _, sp := range stagingPaths {
		if sp == moduleDir {
			result = append(result, ".")
			continue
		}
		if !strings.HasPrefix(sp, moduleDir+"/") {
			return nil, fmt.Errorf("staging package %q is not inside module directory %q", sp, moduleDir)
		}
		result = append(result, strings.TrimPrefix(sp, moduleDir+"/"))
	}
	return result, nil
}
