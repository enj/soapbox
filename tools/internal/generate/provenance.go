package generate

import (
	"context"
	"errors"
	"fmt"
	"go/format"
	"path"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// runProvenance renders the module's root evidence and composes the tree it
// describes.
//
// The generated module is a derivative work: somebody else's code, modified,
// under somebody else's licence, published from a repository that has nothing to
// do with the project the code came from. The root LICENSE, NOTICE, README, and
// doc comment are where that is discharged, and they are generated from the same
// records the rest of the run produced rather than written by hand, so the
// evidence cannot disagree with the module.
//
// The composition and the cross-check are one phase for a reason. Rendering can
// only see the records it was handed, so on its own it cannot tell complete
// evidence from evidence that is merely internally consistent. Cross-checking
// compares it against the tree that will actually be published, which is the
// only comparison that catches a package the evidence forgot or a record
// describing a package the tree does not contain.
func (r *run) runProvenance(ctx context.Context) error {
	license, notice, err := r.readGrants(ctx)
	if err != nil {
		return err
	}

	mappings, err := r.moduleMappings()
	if err != nil {
		return policyError(stageProvenance, err)
	}
	options, err := r.provenanceOptions(license, notice, mappings)
	if err != nil {
		return policyError(stageProvenance, err)
	}

	// The behaviour disclosure is checked before anything is composed. A profile
	// that ran the type policy, acted on its decision, and then rendered a
	// NOTICE without the effects that decision removed would publish a module
	// whose documented behaviour is not its behaviour.
	if err := options.CheckBehaviorChanges(r.types); err != nil {
		return classify(stageProvenance, err, provenanceSemantic...)
	}

	files, err := options.Files()
	if err != nil {
		return classify(stageProvenance, err, provenanceSemantic...)
	}
	copiedGrants, err := copiedLicenseFiles(options.Copied)
	if err != nil {
		return policyError(stageProvenance, err)
	}
	files = append(files, copiedGrants...)
	slices.SortFunc(files, func(a, b relocate.File) int { return strings.Compare(a.Path, b.Path) })

	// Composition validates the tree rather than writing it, so a rejection is a
	// statement about the files the phases produced.
	set, err := r.compose(files)
	if err != nil {
		return policyError(stageProvenance, err)
	}
	set, err = formatComposedGoFiles(set)
	if err != nil {
		return runtimeError(stageProvenance, err)
	}
	if err := options.CrossCheck(set); err != nil {
		return classify(stageProvenance, err, provenanceSemantic...)
	}

	r.files = set
	r.report.recordProvenance(options, files)
	return nil
}

// formatComposedGoFiles applies the pinned toolchain's gofmt rules only after
// every rewrite and generated compatibility file has reached the final tree.
// Formatting earlier would let a later import rewrite publish an unformatted
// result; go/format preserves intentional import groups and does not apply
// goimports-style regrouping.
func formatComposedGoFiles(set relocate.FileSet) (relocate.FileSet, error) {
	formatted := set
	formatted.Files = slices.Clone(set.Files)
	for i := range formatted.Files {
		file := &formatted.Files[i]
		if file.Mode == relocate.ModeSymlink || path.Ext(file.Path) != ".go" {
			continue
		}
		contents, err := format.Source(file.Contents)
		if err != nil {
			return relocate.FileSet{}, fmt.Errorf("format composed Go file %s: %w", file.Path, err)
		}
		file.Contents = contents
	}
	return formatted, nil
}

// readGrants reads the upstream licence and optional notice at the source
// commit.
//
// They are read from the cache at the exact commit rather than from a work tree,
// because the licence that travels with the code is the one that governed it at
// that commit, and a work tree is a sparse view whose contents depend on which
// patterns a closure happened to need.
//
// The runner is anonymous, so this read cannot carry a publishing credential to
// the source host. A blobless cache has to fetch the blob to answer, which is
// why an offline run that does not already hold it refuses: the alternative is
// publishing a module whose licence file came from somewhere other than the
// commit it claims.
func (r *run) readGrants(ctx context.Context) (license, notice []byte, err error) {
	cache, err := r.cacheRunner()
	if err != nil {
		return nil, nil, runtimeError(stageProvenance, err)
	}
	commit := r.post.Report.Source.Commit

	license, err = cache.ReadBlob(ctx, gitcli.BlobOptions{
		Revision:       commit,
		Path:           provenance.LicenseFileName,
		AllowLazyFetch: !r.opts.Offline,
	})
	if err != nil {
		// Reading the grant is a cache and transport operation, so a blob the
		// cache does not hold or an offline run that may not fetch it is a
		// condition to repair rather than a finding about the profile.
		return nil, nil, runtimeError(stageProvenance, fmt.Errorf("upstream %s at %s: %w", provenance.LicenseFileName, commit, err))
	}
	// The identifier the profile states is checked against the text rather than
	// trusted. The generated files quote the obligations of a particular licence
	// by section number, so a record naming the wrong one would misstate what
	// this module owes.
	if err := provenance.VerifyLicense(r.cfg.Source.License, license); err != nil {
		// The identifier is the profile's claim about the text, so a mismatch is
		// exactly the finding this check exists to produce.
		return nil, nil, classify(stageProvenance, err, provenanceSemantic...)
	}

	entries, err := cache.ListTree(ctx, commit)
	if err != nil {
		return nil, nil, runtimeError(stageProvenance, fmt.Errorf("inspect upstream NOTICE at %s: %w", commit, err))
	}
	hasNotice := false
	for _, entry := range entries {
		if entry.Path == provenance.NoticeFileName {
			hasNotice = true
			break
		}
	}
	if hasNotice {
		notice, err = cache.ReadBlob(ctx, gitcli.BlobOptions{
			Revision:       commit,
			Path:           provenance.NoticeFileName,
			AllowLazyFetch: !r.opts.Offline,
		})
		if err != nil {
			// The tree proves the file exists. ErrObjectNotFound can also mean a
			// promised blob a partial clone has not downloaded, so it is a cold
			// cache failure here, never evidence that NOTICE is optional.
			return nil, nil, runtimeError(stageProvenance, fmt.Errorf("upstream %s at %s: %w", provenance.NoticeFileName, commit, err))
		}
	}
	return license, notice, nil
}

// provenanceOptions assembles the root evidence from what the run proved.
//
// Every field is taken from a phase that measured it rather than restated here.
// The package records come from the rewriting step, so the root NOTICE cannot
// disagree with the record committed beside each package; the module mappings
// come from the staging resolution, so the versions it names are the ones go.mod
// requires; the behaviour changes come from the type policy analysis, so a
// disclosure cannot be forgotten; and the public API comes from the facade
// manifest, so the README cannot describe an API the module does not have.
func (r *run) provenanceOptions(license, notice []byte, mappings []provenance.ModuleMapping) (provenance.Options, error) {
	copied, err := r.copiedPackages()
	if err != nil {
		return provenance.Options{}, fmt.Errorf("provenance: %w", err)
	}
	behaviorChanges := provenance.BehaviorChangesFrom(r.types)
	behaviorChanges = append(behaviorChanges, r.compatibilityChanges...)
	return provenance.Options{
		Module:          r.cfg.Destination.Module,
		RootPackage:     r.cfg.Destination.RootPackage,
		Repository:      r.cfg.Destination.Remote,
		InternalPrefix:  r.cfg.Destination.InternalPrefix,
		Summary:         r.cfg.Destination.Summary,
		Source:          r.provenanceSource(),
		License:         license,
		LicenseID:       r.cfg.Source.License,
		UpstreamNotice:  notice,
		Packages:        r.post.Provenance,
		Modules:         slices.Clone(mappings),
		Copied:          copied,
		BehaviorChanges: behaviorChanges,
		PublicAPI:       r.publicAPI(),
	}, nil
}

// provenanceSource identifies the upstream commit the module was extracted from.
func (r *run) provenanceSource() provenance.Source {
	source := r.post.Report.Source
	packages := make([]string, 0, len(r.post.Report.Relocation.Packages))
	for _, pkg := range r.post.Report.Relocation.Packages {
		packages = append(packages, pkg.SourcePackage)
	}
	slices.Sort(packages)
	tag := ""
	if source.RefKind == string(extract.RefTag) {
		tag = source.RefName
	}
	return provenance.Source{
		Repository: r.cfg.Source.Repository,
		Module:     r.cfg.Source.ImportPrefix,
		Project:    r.cfg.Source.Project,
		SHA:        source.Commit,
		Tag:        tag,
		Packages:   slices.Compact(packages),
	}
}

// moduleMappings records which upstream staging directory each pinned version
// corresponds to.
//
// A consumer reading go.mod sees a published version and nothing else. Only this
// record says which directory of which upstream commit that version is meant to
// hold, which is what makes a claim about a staging module checkable rather than
// merely stated.
func (r *run) moduleMappings() ([]provenance.ModuleMapping, error) {
	if r.moduleReport == nil {
		return nil, errors.New("module mappings: the verification pass produced no report")
	}
	resolved := make(map[string]gomodmap.ModuleVersion, len(r.staging))
	for _, module := range r.staging {
		if _, exists := resolved[module.Path]; exists {
			return nil, fmt.Errorf("module mappings: staging module %s was resolved more than once", module.Path)
		}
		resolved[module.Path] = module
	}

	var mappings []provenance.ModuleMapping
	for _, requirement := range r.moduleReport.Kept {
		staged, isStaging := r.root.StagingModuleOf(requirement.Path)
		if !isStaging {
			continue
		}
		module, ok := resolved[requirement.Path]
		if !ok {
			return nil, fmt.Errorf("module mappings: kept staging requirement %s %s has no resolved source mapping", requirement.Path, requirement.Version)
		}
		if module.Version != requirement.Version {
			return nil, fmt.Errorf("module mappings: kept staging requirement %s is %s but its resolved mapping is %s", requirement.Path, requirement.Version, module.Version)
		}
		mappings = append(mappings, provenance.ModuleMapping{
			Source:  staged.Dir,
			Module:  module.Path,
			Version: module.Version,
		})
	}
	slices.SortFunc(mappings, func(a, b provenance.ModuleMapping) int {
		return cmpString(a.Module, b.Module)
	})
	return mappings, nil
}

// publicAPI lists the names the curated facade publishes, for the README.
func (r *run) publicAPI() []string {
	names := make([]string, 0, len(r.postFacade.Manifest.Entries))
	for _, entry := range r.postFacade.Manifest.Entries {
		names = append(names, entry.Name)
	}
	slices.Sort(names)
	return names
}

// copiedPackages builds the CopiedPackage entries the NOTICE renders.
//
// Each copied package records the staging module it came from, the validated
// Origin URL and commit, the licence files collected for it, and its
// destination in the generated tree. CopiedPackage.Package is the import path
// (module path + module-relative package), not the staging or destination path.
func (r *run) copiedPackages() ([]provenance.CopiedPackage, error) {
	if len(r.copyEvidence) == 0 {
		return nil, nil
	}
	var copied []provenance.CopiedPackage
	for _, pkg := range r.copyFiles.Packages {
		ev, err := evidenceForPackage(r.copyEvidence, pkg)
		if err != nil {
			return nil, fmt.Errorf("copied package %s: %w", pkg.Path, err)
		}
		// Derive the import path from the destination prefix. evidenceForPackage
		// already proved the destination is either the module root or beneath it.
		moduleRelPkg := ""
		if pkg.Path != ev.DestinationPrefix {
			var ok bool
			moduleRelPkg, ok = strings.CutPrefix(pkg.Path, ev.DestinationPrefix+"/")
			if !ok || moduleRelPkg == "" {
				return nil, fmt.Errorf("copied package %s is outside destination prefix %s", pkg.Path, ev.DestinationPrefix)
			}
		}
		expectedSource := moduleRelPkg
		if expectedSource == "" {
			expectedSource = "."
		}
		if pkg.Source != expectedSource {
			return nil, fmt.Errorf("copied package %s records source %q, want module-relative %q", pkg.Path, pkg.Source, expectedSource)
		}
		importPath := ev.ModulePath
		if moduleRelPkg != "" {
			importPath = ev.ModulePath + "/" + moduleRelPkg
		}
		// Deep-clone the license records so a caller cannot mutate evidence.
		licenses := make([]provenance.LicenseFile, len(ev.Licenses))
		for i, lf := range ev.Licenses {
			licenses[i] = provenance.LicenseFile{
				Name:        lf.Name,
				SourcePath:  lf.SourcePath,
				Destination: lf.Destination,
				Contents:    slices.Clone(lf.Contents),
				SHA256:      lf.SHA256,
			}
		}
		copied = append(copied, provenance.CopiedPackage{
			Module:           ev.ModulePath,
			Version:          ev.Version,
			Package:          importPath,
			Destination:      pkg.Path,
			SourceRepository: ev.OriginURL,
			SourceSHA:        ev.OriginHash,
			LicenseID:        r.cfg.Source.License,
			Licenses:         licenses,
		})
	}
	return copied, nil
}

// copiedLicenseFiles returns the licence, notice, and patent documents that
// travel with copied dependency packages. Multiple copied packages in one
// module commonly name the same module-root grant, so identical destinations
// are deduplicated; contradictory records fail instead of making composition
// order decide which legal text is published.
func copiedLicenseFiles(copied []provenance.CopiedPackage) ([]relocate.File, error) {
	byDestination := make(map[string]provenance.LicenseFile)
	for _, pkg := range copied {
		for _, license := range pkg.Licenses {
			previous, exists := byDestination[license.Destination]
			if exists {
				if previous.Name != license.Name ||
					previous.SourcePath != license.SourcePath ||
					previous.SHA256 != license.SHA256 ||
					!slices.Equal(previous.Contents, license.Contents) {
					return nil, fmt.Errorf("copied licence destination %q has contradictory records", license.Destination)
				}
				continue
			}
			byDestination[license.Destination] = license
		}
	}

	paths := make([]string, 0, len(byDestination))
	for destination := range byDestination {
		paths = append(paths, destination)
	}
	slices.Sort(paths)
	files := make([]relocate.File, 0, len(paths))
	for _, destination := range paths {
		files = append(files, byDestination[destination].File())
	}
	return files, nil
}

// compose assembles the complete generated module.
//
// Four things go in, and they go in through one write boundary rather than four:
// the relocated upstream code, the generated facade, the module metadata the
// toolchain settled on, and the root evidence. Composing them into a single file
// set is what lets every remaining gate run against the exact tree that will be
// written, and what lets the tree be written in one atomic operation at the end.
func (r *run) compose(provenanceFiles []relocate.File) (relocate.FileSet, error) {
	set, err := r.post.Files.With(r.facadeFiles()...)
	if err != nil {
		return relocate.FileSet{}, fmt.Errorf("facade: %w", err)
	}
	if len(r.copyFiles.Files) > 0 {
		set, err = set.With(r.copyFiles.Files...)
		if err != nil {
			return relocate.FileSet{}, fmt.Errorf("staging copies: %w", err)
		}
	}
	set, err = set.With(r.metadataFiles()...)
	if err != nil {
		return relocate.FileSet{}, fmt.Errorf("module metadata: %w", err)
	}
	set, err = set.With(provenanceFiles...)
	if err != nil {
		return relocate.FileSet{}, fmt.Errorf("root provenance: %w", err)
	}
	return set, nil
}

// metadataFiles renders the tidied module metadata as files.
//
// The bytes are the ones the toolchain produced during verification rather than
// the ones this engine generated before it. Minimal version selection can raise
// a requirement, and go.sum exists only after tidying has computed it, so
// publishing what was generated rather than what was verified would publish a
// module nobody proved.
func (r *run) metadataFiles() []relocate.File {
	report := r.moduleReport
	files := []relocate.File{
		{Path: goModName, Mode: relocate.ModeRegular, Contents: slices.Clone(report.GoMod)},
	}
	// A module requiring nothing outside the standard library needs no
	// checksums, and writing an empty go.sum would state that it has none of
	// something rather than none at all.
	if len(report.GoSum) > 0 {
		files = append(files, relocate.File{Path: goSumName, Mode: relocate.ModeRegular, Contents: slices.Clone(report.GoSum)})
	}
	return files
}

// module metadata file names, which are the go command's rather than this
// engine's choice.
const (
	goModName = "go.mod"
	goSumName = "go.sum"
)

// cmpString orders two strings, which slices.SortFunc needs as a comparison
// rather than a predicate.
func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
