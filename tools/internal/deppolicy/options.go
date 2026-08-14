package deppolicy

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"path"
	"slices"
	"strings"
)

// Policy values. Unknown values are rejected rather than ignored, so a profile
// written for a newer engine cannot silently lose meaning.
const (
	// PolicyExternal keeps every staging dependency external. It is the default
	// and the only policy that needs no evidence.
	PolicyExternal = "external"
	// PolicyCopyApproved admits the proposals a profile lists, still subject to
	// every gate.
	PolicyCopyApproved = "copy-approved"
)

// Gate names.
//
// The first three are correctness gates and can never be overridden. The rest
// are cost gates: they bound how much code a copy takes ownership of and can be
// relaxed with a justification, an approver, and an expiry.
//
// The names are the profile's names on purpose. An operator reading a refusal
// and then editing dependencies.gates should not have to translate.
const (
	GateInteroperability = "interoperability"
	GateGlobalState      = "globalState"
	GateDiamond          = "diamond"

	GateCopiedPackages   = "maxCopiedPackages"
	GateUpstreamCadence  = "maxReleasesPerMinor"
	GateMinimumLeverage  = "minimumLeverage"
	GateCopiedLines      = "maxCopiedLines"
	GateGeneratedFiles   = "maxGeneratedFiles"
	GateDistinctLicenses = "maxDistinctLicenses"
	GateModuleZipBytes   = "maxModuleZipBytes"
	GateSecurityCritical = "securityCritical"
	GateNativeCode       = "nativeCode"
	GateClosureComplete  = "closureCompleteness"
)

// correctnessGates are the gates no override may relax, in report order.
//
// Closure completeness is here rather than among the cost gates because an
// incomplete closure is not expensive, it is broken: the relocated copy would
// reference a package that did not move with it, so the generated module would
// not compile. There is no justification, approver, or expiry that makes that
// acceptable.
var correctnessGates = []string{GateInteroperability, GateGlobalState, GateDiamond, GateClosureComplete}

// costGates are the gates an override may relax, in report order.
//
// The first five are the sized gates the profile schema exposes today. The last
// two are boolean cost gates this package measures and refuses on: they have no
// configured ceiling because the only defensible ceiling is zero. They are
// listed as cost rather than correctness because an operator with a
// justification, an approver, and an expiry may accept them, which is exactly
// what separates cost from correctness here.
var costGates = []string{
	GateCopiedPackages,
	GateCopiedLines,
	GateGeneratedFiles,
	GateDistinctLicenses,
	GateModuleZipBytes,
	GateUpstreamCadence,
	GateSecurityCritical,
	GateNativeCode,
	GateMinimumLeverage,
}

// CorrectnessGates returns the gate names no override may relax.
func CorrectnessGates() []string { return slices.Clone(correctnessGates) }

// CostGates returns the gate names an override may relax.
func CostGates() []string { return slices.Clone(costGates) }

// Options configures one dependency policy decision.
//
// The struct is deliberately independent of internal/config. Policy is the
// lower layer: a profile is one way to describe a decision, and a caller
// translates a profile into Options at the boundary. That also keeps the engine
// able to judge a module no profile describes yet, which is what the fixtures
// do.
type Options struct {
	// ModulePath is the generated module's path, for example
	// monis.app/kk/rbac_authorizer. Packages under it are the generated
	// module's own and are never candidates.
	ModulePath string
	// InternalPrefix is the module relative directory relocated packages live
	// under, for example internal/kk.
	InternalPrefix string
	// SourceMinor is the Kubernetes minor series being transformed, 36 for
	// v1.36.1. Overrides expire relative to it, so a relaxation granted for one
	// release cannot outlive the reason it was granted.
	SourceMinor int
	// Policy is the default action. PolicyExternal admits no copy at all, which
	// is why an external profile with proposals is rejected rather than
	// quietly emptied.
	Policy string
	// Proposals are the staging/src relative package paths a profile proposes
	// to copy. A proposal the graph does not contain fails the run.
	Proposals []string
	// Gates holds the enabled correctness gates and the configured cost
	// ceilings.
	Gates Gates
	// Overrides relax cost gates only.
	Overrides []Override
	// IdentityRequired lists fully qualified type names whose real upstream
	// identity the generated module must keep, for example
	// k8s.io/apiserver/pkg/authorization/authorizer.Authorizer. They come from
	// the facade's interface assertions. A candidate owning one of them can
	// never be copied, and their modules can never leave the build, which is
	// also what makes the diamond gate fire.
	IdentityRequired []string
}

// Gates holds the enabled correctness gates and the configured cost ceilings.
type Gates struct {
	// Interoperability, GlobalState, and Diamond enable the correctness gates.
	//
	// They are configurable only so a profile can state that it runs them, and
	// a profile that proposes a copy with one of them disabled is rejected.
	// Disabling a correctness gate is therefore not a way to pass it.
	Interoperability bool
	GlobalState      bool
	Diamond          bool
	// Cost bounds the size of an approved copy. A zero ceiling admits nothing,
	// which is the correct default: the profile that copies nothing states
	// zero, and a profile that forgot to state a ceiling should not thereby
	// gain an unbounded one.
	Cost CostCeilings
}

// CostCeilings bound the size of an approved staging copy and the benefit it
// must deliver.
//
// The maxima are measured across the whole accepted copy rather than per
// candidate, because that is what a ceiling means: a profile allowing a thousand
// copied lines is describing the generated module, not each package in it.
type CostCeilings struct {
	MaxCopiedPackages   int
	MaxCopiedLines      int
	MaxGeneratedFiles   int
	MaxDistinctLicenses int
	MaxModuleZipBytes   int64
	// MaxReleasesPerMinor bounds how fast the copied code moves upstream. A
	// dependency that ships nine times in a minor series is nine merges the
	// generated module performs itself once it owns the code, and that cost
	// recurs for as long as the copy exists.
	MaxReleasesPerMinor int
	// MinModulesRemoved, MinPackagesRemoved, and MinLinesRemoved are the
	// benefit a copy has to deliver to be worth owning.
	//
	// They are minima rather than maxima, and they are the gate that asks what
	// the copy is for. The usual outcome of copying some packages of a module
	// that stays in the build for the others is that nothing leaves at all: the
	// consumer downloads the same module, compiles the same packages, and now
	// compiles the copy as well. A profile states the benefit it expects and a
	// copy that does not deliver it is refused however cheap it looks.
	MinModulesRemoved  int
	MinPackagesRemoved int
	MinLinesRemoved    int
}

// Override relaxes exactly one cost gate for one candidate package.
//
// An override is a dated promise, not a permanent exemption. It names who made
// the promise, why, and the Kubernetes minor after which it stops being
// believed. An expired override fails the run rather than reverting to the
// unrelaxed gate, because reverting would turn a forgotten promise into a
// silent policy change.
type Override struct {
	// StagingPath is the candidate the override applies to.
	StagingPath string
	// Gate is the cost gate being relaxed. A correctness gate here is a
	// configuration error, not a stronger override.
	Gate string
	// Justification records why the cost is acceptable.
	Justification string
	// Approver records who accepted it.
	Approver string
	// ExpiresAfterMinor is the last Kubernetes minor series the override is
	// believed through, 38 for v1.38. It is good through that minor and
	// expires once the source moves past it, so an override written for v1.38
	// still applies while transforming v1.38 and stops applying at v1.39. The
	// caller parses the profile's minor series string; this package compares
	// numbers so it needs no semver parser of its own.
	ExpiresAfterMinor int
}

// Graph is the loaded, type checked view of one provisional generated module.
//
// The caller loads it, which is what keeps this package free of go/packages, of
// a module resolver, and of any ambient environment. Everything here is a fact
// about one already resolved build.
type Graph struct {
	// Fset positions every file in Boundary and Candidates. Evidence carries
	// positions, so a shared file set is what makes evidence locatable.
	Fset *token.FileSet
	// Boundary are the packages whose exported surface forms the generated
	// module's public boundary. For a curated facade that is the facade package
	// itself; before the facade exists it is the relocated root packages.
	Boundary []*Package
	// Candidates are the staging packages under consideration, whether or not a
	// profile proposes them. Judging unproposed candidates is what lets a
	// report answer "why not copy this" without an operator having to propose
	// it first.
	Candidates []Candidate
	// Build is the resolved consumer build as the Go toolchain reports it, one
	// entry per package. It is what the diamond gate reasons over.
	Build []BuildPackage
	// Modules holds the module facts the toolchain and Git know and this
	// package cannot compute: zip size and upstream cadence.
	Modules []Module
}

// Candidate is one staging package a copy would take ownership of.
type Candidate struct {
	// StagingPath is the upstream repository relative path, for example
	// staging/src/k8s.io/apiserver/pkg/authorization/authorizer. Copied files
	// keep this complete path below the internal prefix, which is what
	// preserves nested Go internal visibility.
	StagingPath string
	// Package is the loaded package at that path.
	Package *Package
}

// Package is one loaded, type checked package.
//
// The shape mirrors what a go/packages load produces, so the adapter at the
// caller's boundary is a field copy and nothing is reinterpreted on the way in.
type Package struct {
	// ImportPath is the package's import path.
	ImportPath string
	// Module is the path of the module providing the package.
	Module string
	// Dir is the absolute directory holding the package's files. This package
	// opens it as a root and reads only the named files inside it.
	Dir string
	// Types is the type checked package.
	Types *types.Package
	// Syntax are the parsed files, in the loader's order.
	Syntax []*ast.File
	// Info carries the type information for Syntax. Uses is what lets the
	// global state scan resolve a selector to a real object instead of
	// guessing from its spelling.
	Info *types.Info
	// GoFiles are the base names of the package's Go build inputs.
	GoFiles []string
	// OtherFiles are the base names of its non-Go build inputs: assembly, C,
	// and anything else the go tool would compile.
	OtherFiles []string
	// IgnoredFiles are the base names of source files that belong to the
	// package directory but were excluded by the current build constraints.
	// A non-empty list means the loader saw files it did not build, so a
	// copy that reads only GoFiles and OtherFiles would be host-specific.
	IgnoredFiles []string
	// Imports are the import paths this package imports.
	Imports []string
}

// BuildPackage is one package in the resolved consumer build.
type BuildPackage struct {
	// ImportPath is the package's import path.
	ImportPath string
	// Module is the path of the module providing it. It is empty for standard
	// library packages.
	Module string
	// Imports are the import paths it imports.
	Imports []string
	// Lines is how many non-test lines the toolchain compiles for this
	// package. It is supplied rather than computed because the dependency's
	// sources are not part of the graph this package reads, and it feeds only
	// the benefit side of the score. Zero means unmeasured, which reports as
	// no measured benefit rather than as a benefit of zero lines.
	Lines int
}

// Module carries the module facts this package cannot measure itself.
//
// Every fact here is paired with a flag saying whether it was actually
// measured. That pairing is the point: a zero zip size and an unmeasured zip
// size are the same number and opposite meanings, and a policy that could not
// tell them apart would approve a copy on missing evidence.
type Module struct {
	// Path is the module path.
	Path string
	// Version is the resolved version.
	Version string
	// Dir is the absolute module root, recorded for provenance.
	Dir string
	// ZipBytes is the module zip size the toolchain reports. It is supplied
	// rather than computed because only the toolchain knows it.
	ZipBytes int64
	// ZipBytesKnown reports whether ZipBytes was measured.
	ZipBytesKnown bool
	// ReleasesPerMinor is how many upstream releases the module published
	// during the source minor series. It is a Git fact the caller measures. A
	// fast moving dependency is expensive to own because every release is a
	// merge the generated module must perform itself.
	ReleasesPerMinor int
	// CadenceKnown reports whether ReleasesPerMinor was measured.
	CadenceKnown bool
	// Licenses are the licence identities the caller verified for this module.
	Licenses []License
	// LicensesVerified reports whether the caller actually inspected the
	// module's licensing documents. A false value refuses the licence gate
	// rather than reporting no licences.
	LicensesVerified bool
}

// License is one licence identity the caller verified, with the documents that
// evidence it.
//
// The identifier is supplied rather than inferred, because a file name is not a
// licence: a file called LICENSE can contain anything, and a module can carry
// its real terms in COPYING. This package therefore never opens these files and
// never claims which licence a module is under.
//
// The identity is recorded once with its documents rather than once per
// document, because only the grant states a licence. A NOTICE carries
// attribution and a PATENTS file carries a separate promise; both travel with
// the grant and neither states permission or conditions, so attaching an
// identifier to them would assert something they do not say. The files are
// evidence for the identity rather than claims of their own, so they are
// recorded and not counted.
type License struct {
	// Identifier is the licence identity the caller established by reading the
	// grant, such as an SPDX identifier. An empty identifier is not counted.
	Identifier string
	// Files are the licensing documents that travel with it, sorted.
	//
	// They are repository relative paths rather than base names. Two
	// directories can each carry a file called LICENSE, so a base name would
	// collapse two distinct obligations into one entry and leave provenance
	// unable to say which document it copied.
	Files []string
}

// clone returns a deep copy so a Decider cannot observe a caller mutating the
// slices it was constructed with.
func (o Options) clone() Options {
	out := o
	out.Proposals = slices.Clone(o.Proposals)
	out.Overrides = slices.Clone(o.Overrides)
	out.IdentityRequired = slices.Clone(o.IdentityRequired)
	return out
}

// normalize sorts the set-like inputs so two equal option sets produce byte
// identical reports and identical failure ordering.
func (o *Options) normalize() {
	o.ModulePath = strings.TrimSpace(o.ModulePath)
	o.InternalPrefix = path.Clean(strings.TrimSpace(o.InternalPrefix))
	if o.InternalPrefix == "." {
		o.InternalPrefix = ""
	}
	o.Policy = strings.TrimSpace(o.Policy)
	slices.Sort(o.Proposals)
	slices.Sort(o.IdentityRequired)
	slices.SortFunc(o.Overrides, func(a, b Override) int {
		if c := compareStrings(a.StagingPath, b.StagingPath); c != 0 {
			return c
		}
		return compareStrings(a.Gate, b.Gate)
	})
}

// validate reports every structural problem in one pass.
func (o *Options) validate() error {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if o.ModulePath == "" {
		addf("modulePath: the generated module path is required")
	}
	if strings.HasPrefix(o.InternalPrefix, "/") || strings.HasPrefix(o.InternalPrefix, "../") {
		addf("internalPrefix: %q must be a module relative directory", o.InternalPrefix)
	}
	if o.SourceMinor < 0 {
		addf("sourceMinor: %d cannot be negative", o.SourceMinor)
	}
	// An override expires relative to the source minor, so an unset source
	// minor would make every override look valid forever. That is the one
	// direction this must not fail in, so it is rejected rather than defaulted.
	if o.SourceMinor == 0 && len(o.Overrides) > 0 {
		addf("sourceMinor: an override expires relative to the source minor, so it must be set when overrides are configured")
	}

	switch o.Policy {
	case PolicyExternal:
		if len(o.Proposals) > 0 {
			addf("policy: %q cannot propose %d staging copies", o.Policy, len(o.Proposals))
		}
	case PolicyCopyApproved:
		if len(o.Proposals) == 0 {
			addf("policy: %q requires at least one proposal", o.Policy)
		}
		// A correctness gate is not optional for a profile that actually
		// proposes a copy. The flags are assertions that the gates run, not
		// switches that turn them off: the gates are evaluated unconditionally
		// either way, and a profile that claims to have disabled one is
		// rejected here rather than quietly ignored. The slice is iterated in
		// declaration order rather than ranged over a map so two runs of the
		// same invalid profile report the same problems in the same order.
		declared := []struct {
			name    string
			enabled bool
		}{
			{GateInteroperability, o.Gates.Interoperability},
			{GateGlobalState, o.Gates.GlobalState},
			{GateDiamond, o.Gates.Diamond},
		}
		for _, gate := range declared {
			if !gate.enabled {
				addf("gates.%s: a profile proposing a copy cannot disable a correctness gate", gate.name)
			}
		}
	default:
		addf("policy: unsupported value %q, want one of %s, %s", o.Policy, PolicyExternal, PolicyCopyApproved)
	}

	for _, proposal := range o.Proposals {
		if err := validateStagingPath(proposal); err != nil {
			addf("proposals: %v", err)
		}
	}
	for _, duplicate := range duplicates(o.Proposals) {
		addf("proposals: duplicate proposal %q", duplicate)
	}

	seen := make([]string, 0, len(o.Overrides))
	for _, override := range o.Overrides {
		seen = append(seen, override.StagingPath+" "+override.Gate)
		if err := validateStagingPath(override.StagingPath); err != nil {
			addf("overrides: %v", err)
		}
		switch {
		case slices.Contains(correctnessGates, override.Gate):
			addf("overrides: correctness gate %q cannot be overridden", override.Gate)
		case !slices.Contains(costGates, override.Gate):
			addf("overrides: unsupported gate %q, want one of %s", override.Gate, strings.Join(costGates, ", "))
		}
		if strings.TrimSpace(override.Justification) == "" {
			addf("overrides: %q needs a justification", override.StagingPath)
		}
		if strings.TrimSpace(override.Approver) == "" {
			addf("overrides: %q needs an approver", override.StagingPath)
		}
		if override.ExpiresAfterMinor <= 0 {
			addf("overrides: %q needs a Kubernetes minor expiry", override.StagingPath)
		}
	}
	for _, duplicate := range duplicates(seen) {
		addf("overrides: duplicate override %q", duplicate)
	}

	for _, required := range o.IdentityRequired {
		if !strings.Contains(required, ".") {
			addf("identityRequired: %q must be a fully qualified type name", required)
		}
	}

	if len(problems) > 0 {
		return &OptionsError{Problems: problems}
	}
	return nil
}

// validateStagingPath enforces the staging/src shape.
//
// The prefix is not decoration. Copied files keep their complete upstream
// relative path so that a nested internal element still enforces Go visibility
// after relocation, and a path that does not start at staging/src cannot do
// that.
func validateStagingPath(p string) error {
	switch {
	case strings.TrimSpace(p) == "":
		return fmt.Errorf("%w: staging path is empty", ErrStagingPathMalformed)
	case p != path.Clean(p):
		return fmt.Errorf("%w: %q is not a clean relative path", ErrStagingPathMalformed, p)
	case !strings.HasPrefix(p, "staging/src/"):
		return fmt.Errorf("%w: %q must start at staging/src so nested internal visibility survives relocation", ErrStagingPathMalformed, p)
	}
	return nil
}

// ImportPathOf returns the import path a staging path provides.
//
// The staging tree maps directly onto module paths: staging/src/k8s.io/apiserver
// is the root of k8s.io/apiserver, so the import path is the remainder.
func ImportPathOf(stagingPath string) string {
	return strings.TrimPrefix(stagingPath, "staging/src/")
}

// validate rejects a graph that cannot support a decision.
func (g *Graph) validate() error {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if g.Fset == nil {
		addf("fset: a shared file set is required so evidence can carry positions")
	}
	if len(g.Boundary) == 0 {
		addf("boundary: at least one boundary package is required")
	}
	for i, pkg := range g.Boundary {
		if pkg == nil {
			addf("boundary[%d]: package is nil", i)
			continue
		}
		if pkg.Types == nil {
			addf("boundary[%d]: package %q is not type checked", i, pkg.ImportPath)
		}
	}
	seen := make(map[string]bool, len(g.Candidates))
	for i, candidate := range g.Candidates {
		if err := validateStagingPath(candidate.StagingPath); err != nil {
			addf("candidates[%d]: %v", i, err)
			continue
		}
		if seen[candidate.StagingPath] {
			addf("candidates[%d]: duplicate candidate %q", i, candidate.StagingPath)
		}
		seen[candidate.StagingPath] = true
		switch {
		case candidate.Package == nil:
			addf("candidates[%d]: package for %q is nil", i, candidate.StagingPath)
		case candidate.Package.Types == nil:
			addf("candidates[%d]: package %q is not type checked", i, candidate.Package.ImportPath)
		case candidate.Package.ImportPath != ImportPathOf(candidate.StagingPath):
			addf("candidates[%d]: staging path %q does not provide import path %q",
				i, candidate.StagingPath, candidate.Package.ImportPath)
		case len(candidate.Package.Syntax) == 0:
			// The global state scan reads syntax. A candidate loaded without it
			// would be scanned, found clean, and approved, so an incomplete
			// load is refused rather than silently passing the gate it cannot
			// run.
			addf("candidates[%d]: package %q carries no syntax, so the global state scan cannot run",
				i, candidate.Package.ImportPath)
		case candidate.Package.Info == nil:
			addf("candidates[%d]: package %q carries no type information, so registrations cannot be resolved",
				i, candidate.Package.ImportPath)
		}
	}

	// The diamond gate reasons entirely over the resolved build. An empty build
	// graph would let it find no retained reacher and pass every candidate,
	// which is the most expensive possible way to be wrong.
	if len(g.Candidates) > 0 && len(g.Build) == 0 {
		addf("build: the diamond gate needs the resolved consumer build, and an empty one would pass every candidate")
	}
	for i, pkg := range g.Build {
		if pkg.ImportPath == "" {
			addf("build[%d]: package has no import path", i)
		}
	}

	if len(problems) > 0 {
		return &OptionsError{Problems: problems}
	}
	return nil
}

// module returns the recorded facts for one module path.
func (g *Graph) module(modulePath string) (Module, bool) {
	for _, module := range g.Modules {
		if module.Path == modulePath {
			return module, true
		}
	}
	return Module{}, false
}

// duplicates returns each value that appears more than once, sorted and itself
// deduplicated.
func duplicates(values []string) []string {
	counts := make(map[string]int, len(values))
	for _, value := range values {
		counts[value]++
	}
	var repeated []string
	for value, count := range maps.All(counts) {
		if count > 1 {
			repeated = append(repeated, value)
		}
	}
	slices.Sort(repeated)
	return repeated
}

// compareStrings orders two strings, and exists so every sort in this package
// reads the same way.
func compareStrings(a, b string) int { return strings.Compare(a, b) }
