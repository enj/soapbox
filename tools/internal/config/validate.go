package config

import (
	"fmt"
	"go/token"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Supported policy values. Unknown values are rejected rather than ignored so a
// profile written for a newer engine cannot silently lose meaning.
const (
	TypePolicyPreferExternal = "prefer-external"
	TypePolicyKeepInternal   = "keep-internal"

	DependencyPolicyExternal     = "external"
	DependencyPolicyCopyApproved = "copy-approved"

	AuthorPolicyPreserveUpstream = "preserve-upstream"

	// PublicationModeManual requires an explicit workflow dispatch to publish.
	PublicationModeManual = "manual"
	// PublicationModeAutomatic adds -unattended to the scheduled sync so
	// publication proceeds without further human intervention.
	PublicationModeAutomatic = "automatic"

	// CompatibilityApiserverExternal means the generated module depends only on
	// published staging modules.
	CompatibilityApiserverExternal = "external"
	// CompatibilityApiserverLocal means the module may use in-tree apiserver
	// helpers that are not published as staging modules.
	CompatibilityApiserverLocal = "local"
)

// facadeKinds are the symbol kinds the facade generator can forward. Exported
// variables are deliberately absent because forwarding them would create a
// second mutable global.
var facadeKinds = []string{"func", "type", "interface", "const"}

// licenseIdentifiers are the SPDX identifiers a profile may name.
//
// The list is a copy of the set the provenance verifier can check the text of,
// and it is a copy on purpose: the verifier reaches this package through the
// relocation and rewriting layers, so importing it here would close a cycle.
// TestLicenseIdentifiersMatchVerifier keeps the two in step, which is the same
// arrangement the gate names use.
//
// The set is deliberately small. Naming a licence in a generated file makes a
// legal claim on the operator's behalf, so a profile may only name one whose
// text the engine actually reads and checks before publishing anything.
var licenseIdentifiers = []string{"Apache-2.0", "BSD-3-Clause", "ISC", "MIT"}

// LicenseIdentifiers returns the SPDX identifiers a profile may name.
//
// It is exported so the package that verifies licence text can be tested against
// the profile schema's actual vocabulary rather than against a literal copied
// into a test, which would keep passing while the two drifted apart.
func LicenseIdentifiers() []string { return slices.Clone(licenseIdentifiers) }

// costGates may be relaxed by an override. correctnessGates never may.
//
// The names are the ones the gate evaluator reports, so an operator reading a
// refusal and then editing dependencies.overrides does not have to translate.
// minimumLeverage is one name for the three minimum removal ceilings, because a
// copy either delivers the benefit the profile asked for or it does not, and
// relaxing one of the three alone would approve a copy on a benefit nobody
// stated.
var (
	costGates       = []string{"maxCopiedLines", "maxCopiedPackages", "maxDistinctLicenses", "maxGeneratedFiles", "maxModuleZipBytes", "maxReleasesPerMinor", "minimumLeverage", "nativeCode", "securityCritical"}
	correctnessGate = []string{"diamond", "globalState", "interoperability"}
)

// CostGateNames returns the gate names an override may relax.
//
// It is exported so the package that evaluates these gates can be tested
// against the profile schema's actual vocabulary rather than against a copy of
// it. The two lists live in different packages on purpose, and a copied literal
// in a test would keep passing while they drifted apart.
func CostGateNames() []string { return slices.Clone(costGates) }

// CorrectnessGateNames returns the gate names no override may relax.
func CorrectnessGateNames() []string { return slices.Clone(correctnessGate) }

// ValidationError reports every problem found in one profile.
type ValidationError struct {
	Problems []string
}

// Error renders each problem on its own line.
func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid profile: %d problems", len(e.Problems))
	for _, problem := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(problem)
	}
	return b.String()
}

// problems accumulates validation failures in a deterministic order.
type problems struct {
	items []string
}

func (p *problems) addf(format string, args ...any) {
	p.items = append(p.items, fmt.Sprintf(format, args...))
}

// check records err, when present, under a field prefix.
func (p *problems) check(field string, err error) {
	if err != nil {
		p.addf("%s: %v", field, err)
	}
}

func (p *problems) err() error {
	if len(p.items) == 0 {
		return nil
	}
	return &ValidationError{Problems: p.items}
}

// validate reports every schema, safety, and consistency problem at once.
func (c *Config) validate() error {
	p := &problems{}

	if c.Version != SchemaVersion {
		p.addf("version: unsupported schema version %d, want %d", c.Version, SchemaVersion)
	}
	c.validateSource(p)
	c.validateDestination(p)
	c.validatePackages(p)
	c.validatePrune(p)
	c.validateDeny(p)
	c.validateClosure(p)
	c.validateTypes(p)
	c.validateDependencies(p)
	c.validatePatches(p)
	c.validateFacade(p)
	c.validateRelease(p)
	c.validateCommit(p)
	c.validateVanity(p)
	c.validatePublication(p)
	c.validateCompatibility(p)
	c.validateDeterminism(p)

	return p.err()
}

func (c *Config) validateSource(p *problems) {
	p.check("source.repository", validateURL(c.Source.Repository, urlRule{allowedHosts: []string{gitHost}, suffix: ".git"}))
	p.check("source.importPrefix", ValidateModulePath(c.Source.ImportPrefix))
	p.check("source.project", validateProse("project name", c.Source.Project))
	if !slices.Contains(licenseIdentifiers, c.Source.License) {
		p.addf("source.license: unsupported value %q, want one of %s", c.Source.License, strings.Join(licenseIdentifiers, ", "))
	}

	if _, err := ParseSemver(c.Source.Refs.MinimumRelease); err != nil {
		p.check("source.refs.minimumRelease", err)
	}
	if len(c.Source.Refs.Branches) == 0 {
		p.addf("source.refs.branches: at least one tracked branch is required")
	}
	for _, branch := range c.Source.Refs.Branches {
		p.check("source.refs.branches", gitcli.ValidateBranchName(branch))
	}
	for _, dup := range duplicates(c.Source.Refs.Branches) {
		p.addf("source.refs.branches: duplicate branch %q", dup)
	}
	if c.Source.Refs.AnchorCommit != "" {
		p.check("source.refs.anchorCommit", ValidateHexSHA(c.Source.Refs.AnchorCommit))
	}
}

func (c *Config) validateDestination(p *problems) {
	d := c.Destination
	p.check("destination.module", ValidateModulePath(d.Module))
	p.check("destination.repository", ValidateRepositorySlug(d.Repository))
	p.check("destination.remote", validateURL(d.Remote, urlRule{allowedHosts: []string{gitHost}, suffix: ".git"}))
	if want := "https://" + gitHost + "/" + d.Repository + ".git"; d.Remote != "" && d.Remote != want {
		p.addf("destination.remote: %q does not match destination.repository, want %q", safeURL(d.Remote), want)
	}
	p.check("destination.branch", gitcli.ValidateBranchName(d.Branch))
	p.check("destination.stateRef", gitcli.ValidateRefName(d.StateRef))
	if d.StateRef != "" && !strings.HasPrefix(d.StateRef, "refs/heads/") {
		p.addf("destination.stateRef: %q must live under refs/heads/", d.StateRef)
	}
	if d.StateRef == "refs/heads/"+d.Branch {
		p.addf("destination.stateRef: %q must differ from the consumer branch", d.StateRef)
	}
	switch {
	case d.ProgressRefPrefix == "":
		p.addf("destination.progressRefPrefix: a progress ref namespace is required")
	case !strings.HasSuffix(d.ProgressRefPrefix, "/"):
		p.addf("destination.progressRefPrefix: %q must end with a slash", d.ProgressRefPrefix)
	case strings.HasPrefix(d.ProgressRefPrefix, "refs/heads/"), strings.HasPrefix(d.ProgressRefPrefix, "refs/tags/"):
		p.addf("destination.progressRefPrefix: %q must not shadow branches or tags", d.ProgressRefPrefix)
	case !strings.HasPrefix(d.ProgressRefPrefix, "refs/"):
		p.addf("destination.progressRefPrefix: %q must start with refs/", d.ProgressRefPrefix)
	default:
		p.check("destination.progressRefPrefix", gitcli.ValidateRefName(strings.TrimSuffix(d.ProgressRefPrefix, "/")))
	}
	p.check("destination.rootPackage", validatePackageName(d.RootPackage))
	p.check("destination.internalPrefix", ValidatePackagePath(d.InternalPrefix))
	if d.InternalPrefix != "" && !slices.Contains(strings.Split(d.InternalPrefix, "/"), "internal") {
		p.addf("destination.internalPrefix: %q must contain an internal element so relocated packages stay unimportable", d.InternalPrefix)
	}
	c.validateSummary(p)
}

// validateSummary checks the sentence the generated root doc comment is built
// from.
//
// The summary is rendered as "Package <rootPackage> provides <summary>" with
// nothing added, and as a standalone paragraph in the README and the NOTICE. The
// shape rules follow from that single rendering: a summary that began with a
// capital would read as a second sentence spliced into the first, and one
// without a final stop would leave every one of those three files ending a
// paragraph mid-sentence.
func (c *Config) validateSummary(p *problems) {
	summary := c.Destination.Summary
	if err := validateProse("summary", summary); err != nil {
		p.check("destination.summary", err)
		return
	}
	if first := []rune(summary)[0]; !unicode.IsLower(first) {
		p.addf("destination.summary: %q must start with a lower case letter because it completes \"Package %s provides ...\"",
			summary, c.Destination.RootPackage)
	}
	if !strings.HasSuffix(summary, ".") {
		p.addf("destination.summary: %q must end with a full stop because it is rendered as a complete sentence", summary)
	}
}

func (c *Config) validatePackages(p *problems) {
	if len(c.Packages.Roots) == 0 {
		p.addf("packages.roots: at least one package root is required")
	}
	for _, root := range c.Packages.Roots {
		p.check("packages.roots", ValidatePackagePath(root))
	}
	for _, dup := range duplicates(c.Packages.Roots) {
		p.addf("packages.roots: duplicate root %q", dup)
	}
	if c.Packages.Recursive {
		for i, outer := range c.Packages.Roots {
			for j, inner := range c.Packages.Roots {
				if i != j && strings.HasPrefix(inner, outer+"/") {
					p.addf("packages.roots: %q is already covered by recursive root %q", inner, outer)
				}
			}
		}
	}
	for _, glob := range c.Packages.AssetGlobs {
		p.check("packages.assetGlobs", ValidateGlob(glob))
	}
	for _, dup := range duplicates(c.Packages.AssetGlobs) {
		p.addf("packages.assetGlobs: duplicate pattern %q", dup)
	}
}

func (c *Config) validatePrune(p *problems) {
	for _, file := range c.Prune.Files {
		p.check("prune.files", validatePruneEntry(file))
	}
	for _, dup := range duplicates(c.Prune.Files) {
		p.addf("prune.files: duplicate entry %q", dup)
	}
	for _, file := range c.Prune.Required {
		p.check("prune.required", validatePruneEntry(file))
	}
	for _, dup := range duplicates(c.Prune.Required) {
		p.addf("prune.required: duplicate entry %q", dup)
	}
	for _, file := range c.Prune.Required {
		if slices.Contains(c.Prune.Files, file) {
			p.addf("prune: %q is both pruned and required", file)
		}
	}
}

func (c *Config) validateDeny(p *problems) {
	for _, imp := range c.Deny.Imports {
		if err := ValidateImportPath(imp); err != nil {
			p.check("deny.imports", err)
			continue
		}
		if imp == c.Source.ImportPrefix {
			p.addf("deny.imports: %q denies the whole source module", imp)
		}
		for _, root := range c.Packages.Roots {
			rooted := c.Source.ImportPrefix + "/" + root
			switch {
			case imp == rooted:
				p.addf("deny.imports: %q denies configured package root %q", imp, root)
			case c.Packages.Recursive && strings.HasPrefix(imp, rooted+"/"):
				p.addf("deny.imports: %q denies a package below recursive root %q", imp, root)
			}
		}
	}
	for _, dup := range duplicates(c.Deny.Imports) {
		p.addf("deny.imports: duplicate import %q", dup)
	}
}

func (c *Config) validateClosure(p *problems) {
	limits := map[string]int{
		"maxPackages":      c.Closure.Limits.MaxPackages,
		"maxFiles":         c.Closure.Limits.MaxFiles,
		"maxNonTestLines":  c.Closure.Limits.MaxNonTestLines,
		"maxPackageGrowth": c.Closure.Limits.MaxPackageGrowth,
	}
	for _, name := range slices.Sorted(maps.Keys(limits)) {
		if limits[name] <= 0 {
			p.addf("closure.limits.%s: must be greater than zero, got %d", name, limits[name])
		}
	}
	if n := len(c.Packages.Roots); c.Closure.Limits.MaxPackages > 0 && c.Closure.Limits.MaxPackages < n {
		p.addf("closure.limits.maxPackages: %d cannot hold %d configured package roots", c.Closure.Limits.MaxPackages, n)
	}
	p.check("closure.golden", ValidateRelPath(c.Closure.Golden))
}

func (c *Config) validateTypes(p *problems) {
	switch c.Types.Policy {
	case TypePolicyPreferExternal:
	case TypePolicyKeepInternal:
		if len(c.Types.Pairs) > 0 {
			p.addf("types: policy %q cannot declare type pairs", c.Types.Policy)
		}
	default:
		p.addf("types.policy: unsupported value %q, want one of %s, %s", c.Types.Policy, TypePolicyPreferExternal, TypePolicyKeepInternal)
	}
	internals := make([]string, 0, len(c.Types.Pairs))
	for _, pair := range c.Types.Pairs {
		internals = append(internals, pair.Internal)
		if err := ValidateImportPath(pair.Internal); err != nil {
			p.check("types.pairs.internal", err)
		} else if !underPrefix(pair.Internal, c.Source.ImportPrefix) {
			p.addf("types.pairs.internal: %q is not part of source.importPrefix %q", pair.Internal, c.Source.ImportPrefix)
		}
		if err := ValidateImportPath(pair.External); err != nil {
			p.check("types.pairs.external", err)
		} else if underPrefix(pair.External, c.Source.ImportPrefix) {
			p.addf("types.pairs.external: %q must be an external module path", pair.External)
		}
	}
	for _, dup := range duplicates(internals) {
		p.addf("types.pairs: duplicate internal package %q", dup)
	}
}

func (c *Config) validateDependencies(p *problems) {
	d := c.Dependencies
	switch d.Policy {
	case DependencyPolicyExternal:
		if len(d.CopyPackages) > 0 {
			p.addf("dependencies: policy %q cannot copy staging packages", d.Policy)
		}
	case DependencyPolicyCopyApproved:
		if len(d.CopyPackages) == 0 {
			p.addf("dependencies: policy %q requires at least one copy package", d.Policy)
		}
		switch {
		case d.Gates.Cost.MaxCopiedPackages == 0:
			p.addf("dependencies.gates.cost.maxCopiedPackages: policy %q requires a non zero cap", d.Policy)
		case d.Gates.Cost.MaxCopiedPackages < len(d.CopyPackages):
			p.addf("dependencies.gates.cost.maxCopiedPackages: %d cannot admit %d copy packages",
				d.Gates.Cost.MaxCopiedPackages, len(d.CopyPackages))
		}
		// A zero ceiling admits nothing, so a profile that proposes a copy and
		// leaves the cadence ceiling at zero has described a copy that can never
		// be approved.
		if d.Gates.Cost.MaxReleasesPerMinor == 0 {
			p.addf("dependencies.gates.cost.maxReleasesPerMinor: policy %q requires a non zero cap", d.Policy)
		}
		// The minima are the gate that asks what the copy is for. Leaving all
		// three at zero demands no benefit at all, which is how a copy that
		// removes nothing from the consumer build gets approved for being cheap.
		if d.Gates.Cost.MinModulesRemoved == 0 && d.Gates.Cost.MinPackagesRemoved == 0 && d.Gates.Cost.MinLinesRemoved == 0 {
			p.addf("dependencies.gates.cost: policy %q requires a non zero minModulesRemoved, minPackagesRemoved, or minLinesRemoved so the copy states the benefit it delivers", d.Policy)
		}
	default:
		p.addf("dependencies.policy: unsupported value %q, want one of %s, %s", d.Policy, DependencyPolicyExternal, DependencyPolicyCopyApproved)
	}
	for _, pkg := range d.CopyPackages {
		if err := ValidatePackagePath(pkg); err != nil {
			p.check("dependencies.copyPackages", err)
			continue
		}
		if !strings.HasPrefix(pkg, "staging/src/") {
			p.addf("dependencies.copyPackages: %q must be a staging/src path so nested internal visibility is preserved", pkg)
		}
	}
	for _, dup := range duplicates(d.CopyPackages) {
		p.addf("dependencies.copyPackages: duplicate package %q", dup)
	}

	for _, mod := range d.ForbiddenModules {
		p.check("dependencies.forbiddenModules", ValidateModulePath(mod))
	}
	for _, dup := range duplicates(d.ForbiddenModules) {
		p.addf("dependencies.forbiddenModules: duplicate module %q", dup)
	}

	if !d.Gates.Interoperability {
		p.addf("dependencies.gates.interoperability: correctness gate cannot be disabled")
	}
	if !d.Gates.GlobalState {
		p.addf("dependencies.gates.globalState: correctness gate cannot be disabled")
	}
	if !d.Gates.Diamond {
		p.addf("dependencies.gates.diamond: correctness gate cannot be disabled")
	}
	costs := map[string]int64{
		"maxCopiedPackages":   int64(d.Gates.Cost.MaxCopiedPackages),
		"maxCopiedLines":      int64(d.Gates.Cost.MaxCopiedLines),
		"maxGeneratedFiles":   int64(d.Gates.Cost.MaxGeneratedFiles),
		"maxDistinctLicenses": int64(d.Gates.Cost.MaxDistinctLicenses),
		"maxModuleZipBytes":   d.Gates.Cost.MaxModuleZipBytes,
		"maxReleasesPerMinor": int64(d.Gates.Cost.MaxReleasesPerMinor),
		"minModulesRemoved":   int64(d.Gates.Cost.MinModulesRemoved),
		"minPackagesRemoved":  int64(d.Gates.Cost.MinPackagesRemoved),
		"minLinesRemoved":     int64(d.Gates.Cost.MinLinesRemoved),
	}
	for _, name := range slices.Sorted(maps.Keys(costs)) {
		if costs[name] < 0 {
			p.addf("dependencies.gates.cost.%s: must not be negative, got %d", name, costs[name])
		}
	}

	seen := make([]string, 0, len(d.Overrides))
	_, minimumMinor, minimumOK := minorOf(c.Source.Refs.MinimumRelease)
	for _, override := range d.Overrides {
		seen = append(seen, override.Package+" "+override.Gate)
		if !slices.Contains(d.CopyPackages, override.Package) {
			p.addf("dependencies.overrides: %q is not a copy package", override.Package)
		}
		switch {
		case slices.Contains(correctnessGate, override.Gate):
			p.addf("dependencies.overrides: correctness gate %q cannot be overridden", override.Gate)
		case !slices.Contains(costGates, override.Gate):
			p.addf("dependencies.overrides: unsupported gate %q, want one of %s", override.Gate, strings.Join(costGates, ", "))
		}
		if strings.TrimSpace(override.Justification) == "" {
			p.addf("dependencies.overrides: %q needs a justification", override.Package)
		}
		if strings.TrimSpace(override.Approver) == "" {
			p.addf("dependencies.overrides: %q needs an approver", override.Package)
		}
		major, minor, err := ParseMinorSeries(override.ExpiresAfter)
		switch {
		case err != nil:
			p.check("dependencies.overrides.expiresAfter", err)
		case major != 1:
			p.addf("dependencies.overrides.expiresAfter: %q must name a Kubernetes v1 minor", override.ExpiresAfter)
		case minimumOK && minor < minimumMinor:
			p.addf("dependencies.overrides.expiresAfter: %q already expired at source.refs.minimumRelease %q", override.ExpiresAfter, c.Source.Refs.MinimumRelease)
		}
	}
	for _, dup := range duplicates(seen) {
		p.addf("dependencies.overrides: duplicate override %q", dup)
	}
}

func (c *Config) validatePatches(p *problems) {
	files := make([]string, 0, len(c.Patches))
	for _, patch := range c.Patches {
		files = append(files, patch.File)
		if err := ValidateRelPath(patch.File); err != nil {
			p.check("patches.file", err)
		} else if !strings.HasPrefix(patch.File, "patches/") {
			p.addf("patches.file: %q must live under patches/", patch.File)
		} else if !strings.HasSuffix(patch.File, ".patch") && !strings.HasSuffix(patch.File, ".diff") {
			p.addf("patches.file: %q must be a .patch or .diff file", patch.File)
		}
		for _, field := range []struct {
			name  string
			value string
		}{{"since", patch.Since}, {"until", patch.Until}} {
			if field.value == "" {
				continue
			}
			// Ancestry selectors may be object names, short tags such as
			// v1.36.1, or fully qualified refs.
			if ValidateHexSHA(field.value) != nil &&
				gitcli.ValidateBranchName(field.value) != nil &&
				gitcli.ValidateRefName(field.value) != nil {
				p.addf("patches.%s: %q is neither an object name nor a ref name", field.name, field.value)
			}
		}
		if patch.Since != "" && patch.Since == patch.Until {
			p.addf("patches: %q selects an empty range because since equals until", patch.File)
		}
		for _, branch := range patch.Branches {
			if err := gitcli.ValidateBranchName(branch); err != nil {
				p.check("patches.branches", err)
				continue
			}
			if !slices.Contains(c.Source.Refs.Branches, branch) {
				p.addf("patches.branches: %q is not a tracked source branch", branch)
			}
		}
		for _, dup := range duplicates(patch.Branches) {
			p.addf("patches.branches: duplicate branch %q", dup)
		}
	}
	for _, dup := range duplicates(files) {
		p.addf("patches: duplicate patch file %q", dup)
	}
}

func (c *Config) validateFacade(p *problems) {
	f := c.Facade
	p.check("facade.package", validatePackageName(f.Package))
	if f.Package != "" && c.Destination.RootPackage != "" && f.Package != c.Destination.RootPackage {
		p.addf("facade.package: %q must match destination.rootPackage %q", f.Package, c.Destination.RootPackage)
	}
	for _, file := range []struct {
		name  string
		value string
	}{{"facade.file", f.File}, {"facade.assertionsFile", f.AssertionsFile}} {
		if err := ValidateRelPath(file.value); err != nil {
			p.check(file.name, err)
			continue
		}
		if !strings.HasSuffix(file.value, ".go") {
			p.addf("%s: %q must be a Go file", file.name, file.value)
		}
		if strings.Contains(file.value, "/") {
			p.addf("%s: %q must live in the module root package", file.name, file.value)
		}
	}
	if f.File != "" && f.File == f.AssertionsFile {
		p.addf("facade: file and assertionsFile must differ")
	}

	names := make([]string, 0, len(f.Exports)+len(f.Aliases))
	sources := make([]string, 0, len(f.Exports)+len(f.Aliases))
	// concrete holds the types that can implement an interface; named holds every
	// exported type or interface that an assertion may refer to.
	concrete := make(map[string]bool, len(f.Exports)+len(f.Aliases))
	named := make(map[string]bool, len(f.Exports)+len(f.Aliases))

	record := func(field, name, kind, source string, wantSameName bool) {
		names = append(names, name)
		sources = append(sources, source)
		if err := ValidateExportedIdent(name); err != nil {
			p.check(field+".name", err)
		}
		if !slices.Contains(facadeKinds, kind) {
			p.addf("%s.kind: unsupported value %q, want one of %s", field, kind, strings.Join(facadeKinds, ", "))
		}
		switch kind {
		case "type":
			concrete[name] = true
			named[name] = true
		case "interface":
			named[name] = true
		}
		pkgPath, symbol, err := ParseSymbolRef(source)
		if err != nil {
			p.check(field+".source", err)
			return
		}
		if !underPrefix(pkgPath, c.Source.ImportPrefix) {
			p.addf("%s.source: %q must be a relocated source package, external types keep their upstream identity", field, source)
		}
		switch {
		case wantSameName && name != symbol:
			p.addf("%s: export %q must keep upstream name %q, use an alias to rename it", field, name, symbol)
		case !wantSameName && name == symbol:
			p.addf("%s: alias %q does not rename upstream symbol %q", field, name, symbol)
		}
	}
	for _, export := range f.Exports {
		record("facade.exports", export.Name, export.Kind, export.Source, true)
	}
	for _, alias := range f.Aliases {
		record("facade.aliases", alias.Name, alias.Kind, alias.Source, false)
	}
	for _, dup := range duplicates(names) {
		p.addf("facade: duplicate public name %q", dup)
	}
	for _, dup := range duplicates(sources) {
		p.addf("facade: duplicate source symbol %q", dup)
	}

	assertions := make([]string, 0, len(f.InterfaceAssertions))
	for _, assertion := range f.InterfaceAssertions {
		assertions = append(assertions, assertion.Type+" "+assertion.Interface)
		switch err := ValidateExportedIdent(assertion.Type); {
		case err != nil:
			p.check("facade.interfaceAssertions.type", err)
		case named[assertion.Type] && !concrete[assertion.Type]:
			p.addf("facade.interfaceAssertions.type: %q is an interface, an assertion needs the concrete type that implements one", assertion.Type)
		case !concrete[assertion.Type]:
			p.addf("facade.interfaceAssertions.type: %q is not an exported facade type", assertion.Type)
		}
		// The asserted interface is always a qualified symbol in a module this
		// one does not own. An assertion exists to prove the facade type
		// satisfies the real upstream interface that consumers pass it to;
		// asserting against a name this facade republishes would compare the
		// type to a copy and prove nothing about the interface anyone uses. The
		// generator refuses an unqualified name outright, so a profile carrying
		// one is refused here rather than validating and then failing to
		// generate.
		pkgPath, _, err := ParseSymbolRef(assertion.Interface)
		switch {
		case err != nil:
			p.check("facade.interfaceAssertions.interface", err)
		case ValidateModulePath(pkgPath) != nil:
			p.addf("facade.interfaceAssertions.interface: %q must name a package qualified interface, such as k8s.io/apiserver/pkg/authorization/authorizer.Authorizer", assertion.Interface)
		case underPrefix(pkgPath, c.Destination.Module):
			p.addf("facade.interfaceAssertions.interface: %q is inside destination.module %q, and an assertion against a copied interface proves nothing about the real one",
				assertion.Interface, c.Destination.Module)
		}
	}
	for _, dup := range duplicates(assertions) {
		p.addf("facade.interfaceAssertions: duplicate assertion %q", dup)
	}
}

func (c *Config) validateRelease(p *problems) {
	if c.Release.Policy != ReleasePolicyV1ToV0 {
		p.addf("release.policy: unsupported value %q, want %s", c.Release.Policy, ReleasePolicyV1ToV0)
		return
	}
	if _, err := ParseSemver(c.Release.FirstTag); err != nil {
		p.check("release.firstTag", err)
		return
	}
	mapped, err := MapReleaseTag(c.Release.Policy, c.Source.Refs.MinimumRelease)
	if err != nil {
		p.check("release", err)
		return
	}
	if mapped != c.Release.FirstTag {
		p.addf("release.firstTag: %q does not match source.refs.minimumRelease %q under policy %s, want %q",
			c.Release.FirstTag, c.Source.Refs.MinimumRelease, c.Release.Policy, mapped)
	}
}

func (c *Config) validateCommit(p *problems) {
	if c.Commit.AuthorPolicy != AuthorPolicyPreserveUpstream {
		p.addf("commit.authorPolicy: unsupported value %q, want %s", c.Commit.AuthorPolicy, AuthorPolicyPreserveUpstream)
	}
	p.check("commit.committer.name", ValidateIdentityName(c.Commit.Committer.Name))
	p.check("commit.committer.email", ValidateEmail(c.Commit.Committer.Email))
	p.check("commit.trailerKey", validateTrailerKey(c.Commit.TrailerKey))
	if c.Commit.Sign {
		p.addf("commit.sign: generated commits are never signed")
	}
}

func (c *Config) validateVanity(p *problems) {
	v := c.Vanity
	p.check("vanity.repository", ValidateRepositorySlug(v.Repository))
	if err := ValidateRelPath(v.Path); err != nil {
		p.check("vanity.path", err)
	} else if !strings.HasSuffix(v.Path, "/index.html") {
		p.addf("vanity.path: %q must end with /index.html", v.Path)
	}
	p.check("vanity.importPath", ValidateModulePath(v.ImportPath))
	if v.ImportPath != c.Destination.Module {
		p.addf("vanity.importPath: %q must match destination.module %q", v.ImportPath, c.Destination.Module)
	}
	p.check("vanity.repositoryURL", validateURL(v.RepositoryURL, urlRule{allowedHosts: []string{gitHost}}))
	if want := "https://" + gitHost + "/" + c.Destination.Repository; v.RepositoryURL != "" && v.RepositoryURL != want {
		p.addf("vanity.repositoryURL: %q does not match destination.repository, want %q", safeURL(v.RepositoryURL), want)
	}
	domain := moduleDomain(c.Destination.Module)
	p.check("vanity.probeURL", validateURL(v.ProbeURL, urlRule{allowedHosts: []string{domain}, query: "go-get=1"}))
	if want := "https://" + c.Destination.Module + "?go-get=1"; v.ProbeURL != "" && v.ProbeURL != want {
		p.addf("vanity.probeURL: %q does not match destination.module, want %q", safeURL(v.ProbeURL), want)
	}
}

func (c *Config) validatePublication(p *problems) {
	switch c.Publication.Mode {
	case PublicationModeManual, PublicationModeAutomatic:
	default:
		p.addf("publication.mode: unsupported value %q, want one of %s, %s", c.Publication.Mode, PublicationModeManual, PublicationModeAutomatic)
	}
}

func (c *Config) validateCompatibility(p *problems) {
	switch c.Compatibility.Apiserver {
	case CompatibilityApiserverExternal, CompatibilityApiserverLocal:
	default:
		p.addf("compatibility.apiserver: unsupported value %q, want one of %s, %s", c.Compatibility.Apiserver, CompatibilityApiserverExternal, CompatibilityApiserverLocal)
	}
}

func (c *Config) validateDeterminism(p *problems) {
	p.check("determinism.toolchain", validateToolchain(c.Determinism.Toolchain))
	if c.Determinism.ChunkSize <= 0 {
		p.addf("determinism.chunkSize: must be greater than zero, got %d", c.Determinism.ChunkSize)
	}
}

// validatePruneEntry checks one exact prune or required entry. Prune rules name
// files, never directories or patterns, so an upstream rename fails closed.
func validatePruneEntry(entry string) error {
	if err := ValidateRelPath(entry); err != nil {
		return err
	}
	if strings.ContainsAny(entry, "*?[]") {
		return fmt.Errorf("entry %q must be an exact file path, not a pattern", entry)
	}
	if !strings.Contains(entry, "/") {
		return fmt.Errorf("entry %q must be a repository relative file path", entry)
	}
	return nil
}

// validatePackageName checks a Go package name that will appear in generated
// source. Only lower case letters and digits keep import lines readable, and a
// keyword would not compile.
func validatePackageName(name string) error {
	if name == "" {
		return fmt.Errorf("package name must not be empty")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("package name %q must be lower case letters and digits", name)
		}
	}
	if token.IsKeyword(name) {
		return fmt.Errorf("package name %q is a Go keyword", name)
	}
	return nil
}

// validateTrailerKey checks the generated provenance trailer key.
func validateTrailerKey(key string) error {
	if key == "" {
		return fmt.Errorf("trailer key must not be empty")
	}
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case (r >= '0' && r <= '9' || r == '-') && i > 0:
		default:
			return fmt.Errorf("trailer key %q must be a Git trailer token", key)
		}
	}
	return nil
}

// validateToolchain checks the pinned exact Go toolchain.
func validateToolchain(toolchain string) error {
	rest, ok := strings.CutPrefix(toolchain, "go")
	if !ok {
		return fmt.Errorf("toolchain %q must start with go", toolchain)
	}
	fields := strings.Split(rest, ".")
	if len(fields) != 3 {
		return fmt.Errorf("toolchain %q must pin an exact patch release such as go1.26.5", toolchain)
	}
	for _, field := range fields {
		if _, err := parseNumericID(field); err != nil {
			return fmt.Errorf("toolchain %q: %w", toolchain, err)
		}
	}
	return nil
}

// underPrefix reports whether importPath is prefix itself or below it.
func underPrefix(importPath, prefix string) bool {
	return importPath == prefix || strings.HasPrefix(importPath, prefix+"/")
}

// minorOf reports the minor version of a release tag.
func minorOf(tag string) (major, minor int, ok bool) {
	version, err := ParseSemver(tag)
	if err != nil {
		return 0, 0, false
	}
	return version.Major, version.Minor, true
}

// duplicates returns the sorted distinct values that appear more than once.
func duplicates(values []string) []string {
	counts := make(map[string]int, len(values))
	for _, value := range values {
		counts[value]++
	}
	var repeated []string
	for value, count := range counts {
		if count > 1 {
			repeated = append(repeated, value)
		}
	}
	slices.Sort(repeated)
	return repeated
}
