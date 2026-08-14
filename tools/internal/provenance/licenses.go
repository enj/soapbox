package provenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/relocate"
)

// licenseStems are the file name stems a grant, an attribution notice, or a
// patent grant is published under.
//
// Both spellings of licence are here because both occur in the wild and a
// collection that missed one would report a package as having no grant, which
// fails the copy closed for a reason that is not true.
var licenseStems = []string{"COPYING", "LICENCE", "LICENSE", "NOTICE", "PATENTS"}

// licenseExtensions are the suffixes those stems appear with.
//
// A repository publishes its grant as a bare name, as Markdown, or as plain
// text, and all three are the same document.
var licenseExtensions = []string{"", ".md", ".txt"}

// grantStems are the stems that state a grant, as opposed to travelling with
// one. A notice carries attribution and a patent file carries a separate
// promise; neither states the permission or the conditions, so neither can
// establish which licence a copy is under.
var grantStems = []string{"COPYING", "LICENCE", "LICENSE"}

// LicenseNames are the file names a licence collection looks for.
//
// Names are matched exactly rather than by pattern. A glob would sweep up
// LICENSES.md, license_test.go, and a vendored directory listing and copy them
// as though they were grants, and a file is not a licence because of what it is
// called: the identifier a copy records is checked against the text.
var LicenseNames = licenseNames()

// licenseNames expands the stems over the extensions, sorted, so the collection
// order is fixed and the two tables cannot drift apart.
func licenseNames() []string {
	names := make([]string, 0, len(licenseStems)*len(licenseExtensions))
	for _, stem := range licenseStems {
		for _, extension := range licenseExtensions {
			names = append(names, stem+extension)
		}
	}
	slices.Sort(names)
	return names
}

// StatesGrant reports whether a collected file is the one that states the
// licence, which is the only kind whose text can verify an identifier.
func StatesGrant(name string) bool {
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".md"), ".txt")
	return slices.Contains(grantStems, stem)
}

// Licence identifiers this engine can verify.
//
// The set is deliberately small. A provenance file that names a licence is
// making a legal claim on the operator's behalf, so it may only name one whose
// text this engine has actually checked. An identifier that is not here is
// refused rather than repeated, because repeating an unverified identifier is
// how a NOTICE ends up asserting a grant nobody confirmed.
const (
	// Apache20 is the Apache License 2.0.
	Apache20 = "Apache-2.0"
	// BSD3Clause is the three clause BSD licence.
	BSD3Clause = "BSD-3-Clause"
	// MIT is the MIT licence.
	MIT = "MIT"
	// ISC is the ISC licence.
	ISC = "ISC"
)

// licenseMarkers are the phrases each verifiable licence text must contain.
//
// They are matched rather than parsed because a licence file is prose with a
// stable body: every Apache 2.0 file carries its title and version line, and
// every BSD 3 clause file carries its redistribution clause and its
// endorsement clause. Matching the body catches the mistake that matters, which
// is a record labelling one licence while carrying another.
var licenseMarkers = map[string][]string{
	Apache20:   {"Apache License", "Version 2.0"},
	BSD3Clause: {"Redistribution and use in source and binary forms", "Neither the name of"},
	MIT:        {"Permission is hereby granted, free of charge"},
	ISC:        {"Permission to use, copy, modify, and/or distribute this software"},
}

// VerifyLicense checks that a licence text is the licence an identifier names.
//
// It is exported because the same check has to hold wherever a licence
// identifier is recorded: the root licence of the generated module and the
// grant beside every copied dependency package are the same kind of claim.
func VerifyLicense(id string, contents []byte) error {
	markers, known := licenseMarkers[id]
	if !known {
		return fmt.Errorf("%w: licence %q is not one this engine can verify, and an unverified identifier may not be published: %s",
			ErrOptions, id, strings.Join(verifiableLicenses(), ", "))
	}
	text := string(contents)
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			return fmt.Errorf("%w: the licence text does not contain %q, so it is not %s", ErrLicense, marker, id)
		}
	}
	return nil
}

// verifiableLicenses reports the identifiers this engine can verify, sorted.
func verifiableLicenses() []string {
	ids := make([]string, 0, len(licenseMarkers))
	for id := range licenseMarkers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// LicenseFile is one licence, notice, or patent grant copied with a dependency.
type LicenseFile struct {
	// Name is the file's base name, one of LicenseNames.
	Name string
	// SourcePath is the upstream repository relative path it was read from.
	SourcePath string
	// Destination is the module relative path it is copied to, which preserves
	// the full upstream path below the internal prefix exactly as the copied
	// source does.
	Destination string
	// Contents are the file bytes, copied unchanged.
	Contents []byte
	// SHA256 is the hex digest of the contents. It is recorded so the NOTICE
	// can state which grant was copied in a form that a reader can check
	// against the upstream repository without trusting this record.
	SHA256 string
}

// File renders the licence as a relocated file, so it reaches the tree through
// the same write boundary as the code it belongs to.
func (l LicenseFile) File() relocate.File {
	// No package is recorded. A licence is not Go source, the directory it sits
	// in need not be a package at all, and claiming one would put a package
	// into the relocated tree's record that no relocation produced.
	return relocate.File{
		Source:   l.SourcePath,
		Path:     l.Destination,
		Mode:     relocate.ModeRegular,
		Contents: slices.Clone(l.Contents),
	}
}

// CopiedPackage records one dependency package copied into the module.
//
// Copying a dependency is the decision the dependency policy exists to make
// rarely and deliberately, so the record is complete enough to review after the
// fact: which module the code belongs to, at which version and commit, where it
// landed, who approved it, and every grant that came with it.
type CopiedPackage struct {
	// Module is the dependency's module path, such as k8s.io/apiserver.
	Module string
	// Version is the version the copy corresponds to, such as v0.36.1.
	Version string
	// Package is the copied package's import path.
	Package string
	// Destination is the module relative directory it was copied to.
	Destination string
	// SourceRepository and SourceSHA identify the commit the copy was taken at.
	SourceRepository string
	SourceSHA        string
	// Override is the identifier of the approval that permitted the copy, when
	// the profile required one.
	Override string
	// LicenseID is the SPDX identifier of the grant the copied code is under.
	// It is required and is checked against the copied licence text, because
	// the NOTICE states it and a record that named the wrong licence would make
	// this module distribute code under terms nobody agreed to.
	LicenseID string
	// Licenses are the grants copied with the package, sorted by destination.
	Licenses []LicenseFile
}

// validate refuses a record that would publish an unusable or unsafe claim.
func (c CopiedPackage) validate() error {
	for _, required := range []struct{ name, value string }{
		{"copied module path", c.Module},
		{"copied module version", c.Version},
		{"copied package path", c.Package},
	} {
		if required.value == "" {
			return fmt.Errorf("%w: a %s is required", ErrOptions, required.name)
		}
	}
	if err := checkRelative(c.Destination, "copied package destination"); err != nil {
		return err
	}
	if err := checkURL(c.SourceRepository, "copied package repository"); err != nil {
		return err
	}
	if !shaPattern.MatchString(c.SourceSHA) {
		return fmt.Errorf("%w: copied package %s commit %q is not a Git object name", ErrOptions, c.Package, c.SourceSHA)
	}
	if len(c.Licenses) == 0 {
		// A copied package with no grant beside it is either a collection bug
		// or a package whose licence this module cannot state, and both have to
		// stop the run rather than produce a NOTICE that claims nothing.
		return fmt.Errorf("%w: copied package %s carries no licence file", ErrOptions, c.Package)
	}
	granted := false
	for _, license := range c.Licenses {
		if err := checkRelative(license.Destination, "copied licence destination"); err != nil {
			return err
		}
		if err := checkRelative(license.SourcePath, "copied licence source"); err != nil {
			return err
		}
		if !slices.Contains(LicenseNames, license.Name) {
			return fmt.Errorf("%w: %q is not a licence file name", ErrOptions, license.Name)
		}
		// The grant itself is what the identifier describes. A NOTICE and a
		// PATENTS file travel with it but state neither the permission nor the
		// conditions, so verifying the identifier against one of them would
		// prove nothing.
		if !StatesGrant(license.Name) {
			continue
		}
		if err := VerifyLicense(c.LicenseID, license.Contents); err != nil {
			return fmt.Errorf("copied package %s: %s: %w", c.Package, license.SourcePath, err)
		}
		granted = true
	}
	if !granted {
		return fmt.Errorf("%w: copied package %s carries no file stating a grant, so its %q identifier is unverified: one of %s is required",
			ErrOptions, c.Package, c.LicenseID, strings.Join(grantStems, ", "))
	}
	return nil
}

// CollectOptions describes a licence collection over one upstream worktree.
type CollectOptions struct {
	// FS is the upstream worktree, rooted at the repository root.
	FS fs.FS
	// ModuleRoot is the repository relative directory of the copied module's
	// root, such as staging/src/k8s.io/apiserver. The walk stops there rather
	// than continuing to the repository root, because a module's own licence is
	// the one that governs its packages.
	ModuleRoot string
	// Packages are the repository relative package directories being copied.
	Packages []string
	// InternalPrefix is the module relative directory copied files land below.
	InternalPrefix string
}

// Collect gathers the licence files that govern a set of copied packages.
//
// The walk goes from each copied package directory upwards to the module root,
// collecting every licence file on the way, because a repository may put its
// grant at the module root, beside a subtree that is licensed differently, or
// both. A file found twice through two packages is collected once.
//
// Nothing is inferred. If a copied package has no grant above it, this returns
// what it found and the caller's validation refuses the copy: guessing that the
// module root's licence must apply is exactly the sort of assumption that
// belongs to a human reviewing a copy, not to a generator.
//
// The result is empty for a profile that copies nothing, which is the case this
// engine is expected to be in: keeping dependencies external is what preserves
// the type identity the rest of the ecosystem uses.
func Collect(ctx context.Context, opts CollectOptions) ([]LicenseFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("licence collection: %w", err)
	}
	if len(opts.Packages) == 0 {
		return nil, nil
	}
	if opts.FS == nil {
		return nil, fmt.Errorf("licence collection: %w: an upstream worktree is required", ErrOptions)
	}
	if err := checkRelative(opts.ModuleRoot, "module root"); err != nil {
		return nil, fmt.Errorf("licence collection: %w", err)
	}
	if err := checkRelative(opts.InternalPrefix, "internal prefix"); err != nil {
		return nil, fmt.Errorf("licence collection: %w", err)
	}

	collected := make(map[string]LicenseFile)
	for _, pkg := range opts.Packages {
		if err := checkRelative(pkg, "copied package"); err != nil {
			return nil, fmt.Errorf("licence collection: %w", err)
		}
		if pkg != opts.ModuleRoot && opts.ModuleRoot != "." && !strings.HasPrefix(pkg, opts.ModuleRoot+"/") {
			return nil, fmt.Errorf("licence collection: package %q is not inside module root %q: %w", pkg, opts.ModuleRoot, ErrOptions)
		}
		if err := collectFrom(opts, pkg, collected); err != nil {
			return nil, fmt.Errorf("licence collection: %w", err)
		}
	}

	licenses := make([]LicenseFile, 0, len(collected))
	for _, license := range collected {
		licenses = append(licenses, license)
	}
	slices.SortFunc(licenses, func(a, b LicenseFile) int { return strings.Compare(a.Destination, b.Destination) })
	return licenses, nil
}

// collectFrom walks one package directory up to the module root.
func collectFrom(opts CollectOptions, pkg string, collected map[string]LicenseFile) error {
	for dir := pkg; ; dir = path.Dir(dir) {
		for _, name := range LicenseNames {
			source := path.Join(dir, name)
			if _, seen := collected[source]; seen {
				continue
			}
			contents, err := fs.ReadFile(opts.FS, source)
			switch {
			case err != nil:
				// A missing licence at one level is the normal case: a grant
				// usually sits at the module root and nowhere else. Anything
				// other than absence is a real read failure and is reported,
				// including a name that turned out to be a directory, which
				// would otherwise be silently skipped.
				if !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("read %s: %w", source, err)
				}
				continue
			case len(contents) == 0:
				return fmt.Errorf("%s is empty, which states no grant: %w", source, ErrOptions)
			}
			digest := sha256.Sum256(contents)
			collected[source] = LicenseFile{
				Name:       name,
				SourcePath: source,
				// The destination preserves the full upstream path below the
				// internal prefix, which is the same rule the copied code
				// follows, so a grant always sits at the same relative position
				// to the files it governs as it did upstream.
				Destination: path.Join(opts.InternalPrefix, source),
				Contents:    contents,
				SHA256:      hex.EncodeToString(digest[:]),
			}
		}
		if dir == opts.ModuleRoot {
			return nil
		}
	}
}

// sortedChanges orders behaviour changes deterministically.
func sortedChanges(changes []BehaviorChange) []BehaviorChange {
	ordered := slices.Clone(changes)
	slices.SortFunc(ordered, func(a, b BehaviorChange) int {
		if order := strings.Compare(a.Summary, b.Summary); order != 0 {
			return order
		}
		return strings.Compare(a.Cause, b.Cause)
	})
	return ordered
}

// sortedMappings orders staging module mappings by published module path.
func sortedMappings(mappings []ModuleMapping) []ModuleMapping {
	ordered := slices.Clone(mappings)
	slices.SortFunc(ordered, func(a, b ModuleMapping) int {
		if order := strings.Compare(a.Module, b.Module); order != 0 {
			return order
		}
		return strings.Compare(a.Source, b.Source)
	})
	return ordered
}

// sortedCopied orders copied packages by import path.
func sortedCopied(copied []CopiedPackage) []CopiedPackage {
	ordered := slices.Clone(copied)
	slices.SortFunc(ordered, func(a, b CopiedPackage) int { return strings.Compare(a.Package, b.Package) })
	return ordered
}
