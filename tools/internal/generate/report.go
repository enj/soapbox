package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/facade"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

// Report is the deterministic record of one generation.
//
// It carries no absolute path, no proxy URL, no credential, no source remote
// override, and no timestamp. That is not tidiness: the report is compared byte
// for byte between two runs over different directory layouts, it is attached to
// CI artifacts, and it is the evidence a reviewer reads before approving an
// outward action. A path from the machine that produced it would break the first
// use and leak into the second.
//
// Every list is sorted and non-nil, so the encoding depends on the generation
// alone and never on map iteration order or on whether a list happened to be
// empty.
type Report struct {
	Schema       int              `json:"schema"`
	Engine       EngineReport     `json:"engine"`
	Source       SourceReport     `json:"source"`
	Extract      ExtractReport    `json:"extract"`
	Staging      StagingReport    `json:"staging"`
	Module       ModuleReport     `json:"module"`
	Facade       FacadeReport     `json:"facade"`
	Types        TypesReport      `json:"types"`
	Dependencies DependencyReport `json:"dependencies"`
	Provenance   ProvenanceReport `json:"provenance"`
	Output       OutputReport     `json:"output"`
	// Failure records why the generation refused, nil when it did not. A report
	// is produced for a refusal precisely so it is reviewable without rerunning
	// the pipeline.
	Failure *FailureReport `json:"failure"`
	// Notices are advisory findings from every phase, sorted and deduplicated.
	// They never stop a generation on their own; -strict is what turns them into
	// a refusal, and it does so before any output is written.
	Notices []string `json:"notices"`

	// scrub holds the absolute directories this run used, so a rendered failure
	// can have them replaced by stable placeholders. It is unexported and never
	// encoded: it exists to keep paths out of the report, not to record them.
	scrub    []scrubRule
	redactor *gitcli.Redactor
}

// EngineReport identifies what produced the generation.
type EngineReport struct {
	// Version is the engine version.
	Version string `json:"version"`
	// Toolchain is the Go toolchain the profile pins for deterministic
	// formatting.
	Toolchain string `json:"toolchain"`
	// ProfileHash is the digest of the output affecting subset of the profile.
	// It is taken from the post-prune extraction rather than recomputed, so the
	// generation and the plan it contains can never disagree about which profile
	// produced them.
	ProfileHash string `json:"profileHash"`
}

// SourceReport records the upstream release the generation covers.
//
// The remote is absent on purpose, including when it was overridden. Only the
// fact of an override is recorded: its value is frequently a path on the machine
// that ran the generation, and this report is compared byte for byte between two
// runs over different layouts.
type SourceReport struct {
	RefKind string `json:"refKind"`
	RefName string `json:"refName"`
	// Commit is the upstream commit both extraction passes read.
	Commit string `json:"commit"`
	// ReleaseTag is the generated module's tag for an upstream release. It is
	// empty for an intermediate exact commit, which publishes no module tag.
	ReleaseTag string `json:"releaseTag"`
	// Fetched, Offline, and RemoteOverridden report how the source was obtained.
	Fetched          bool `json:"fetched"`
	Offline          bool `json:"offline"`
	RemoteOverridden bool `json:"remoteOverridden"`
}

// ExtractReport records both extraction passes.
type ExtractReport struct {
	// Pre is the unpruned baseline, which exists only to make the facade
	// comparison and the type policy possible.
	Pre PassReport `json:"pre"`
	// Post is the pass that produced the module being generated.
	Post PassReport `json:"post"`
}

// PassReport is one extraction pass, digested and summarized.
//
// The whole plan report is digested rather than embedded, because a generation
// report that contained two complete plan reports would be dominated by them
// while answering a different question. The digest is what proves two
// generations ran the same plan; the sections beside it are the ones a reviewer
// of a generation actually reads.
type PassReport struct {
	// ReportHash digests the pass's complete plan report.
	ReportHash string `json:"reportHash"`
	// ManifestHash digests the relocated tree the pass produced.
	ManifestHash string `json:"manifestHash"`
	Files        int    `json:"files"`
	Packages     int    `json:"packages"`
	// ClosurePackages are the post-prune package import paths of this pass,
	// sorted.
	ClosurePackages []string `json:"closurePackages"`
	// PrunedFiles and DeniedImports are what the pass asserted, sorted. They are
	// empty for the baseline by construction, which is what makes the pair
	// readable as a description of what pruning did.
	PrunedFiles   []string `json:"prunedFiles"`
	DeniedImports []string `json:"deniedImports"`
}

// StagingReport records how the staging modules were pinned.
type StagingReport struct {
	// SourceModule is the module path the upstream commit declares.
	SourceModule string `json:"sourceModule"`
	// GoDirective is the source module's language version, which the generated
	// module inherits so the extracted code compiles under the semantics
	// upstream compiled it under.
	GoDirective string `json:"goDirective"`
	// Cached reports that the version index already held this source commit, so
	// no version was resolved over the network.
	Cached bool `json:"cached"`
	// Modules are the pinned staging modules, sorted by path.
	Modules []ModulePin `json:"modules"`
}

// ModulePin is one staging module resolved to a published version.
type ModulePin struct {
	// Path is the published module path.
	Path string `json:"path"`
	// Version is the resolved version.
	Version string `json:"version"`
	// Commit is the staging commit that version names, which is the evidence a
	// later run has that the tag still names what it named before.
	Commit string `json:"commit"`
	// Directory is the upstream staging directory the version corresponds to,
	// repository relative.
	Directory string `json:"directory"`
}

// ModuleReport records the module metadata the toolchain settled on.
type ModuleReport struct {
	// GoModHash and GoSumHash digest the published metadata. The bytes
	// themselves are the tree's rather than the report's, and the tree is
	// already digested by Output.ManifestHash.
	GoModHash string `json:"goModHash"`
	GoSumHash string `json:"goSumHash"`
	// Kept lists the requirements that survived tidying, sorted by path.
	Kept []RequirementReport `json:"kept"`
	// Added lists transitive requirements introduced by an allowed compatibility
	// re-tidy, sorted by path.
	Added []RequirementReport `json:"added,omitempty"`
	// Dropped lists the module paths tidying removed, sorted. A large set is the
	// normal outcome of extracting a few packages out of Kubernetes.
	Dropped []string `json:"dropped"`
	// Reclassified lists requirements tidying kept at the pinned version but
	// marked differently than the source module did, sorted by path.
	Reclassified []ReclassificationReport `json:"reclassified"`
	// BaselineGoModHash digests the unpruned module's metadata. It is recorded
	// because the baseline is what the facade comparison was made against, and a
	// reviewer checking that comparison needs to know which module produced it.
	BaselineGoModHash string `json:"baselineGoModHash"`
}

// RequirementReport is one surviving requirement.
type RequirementReport struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect"`
}

// ReclassificationReport is one requirement whose directness tidying changed.
type ReclassificationReport struct {
	Path string `json:"path"`
	// Indirect is what tidying decided, and therefore what the generated module
	// carries. What the source module said is its negation.
	Indirect bool `json:"indirect"`
}

// FacadeReport records the published API and the comparison that gated it.
type FacadeReport struct {
	// Package is the generated root package name.
	Package string `json:"package"`
	// PreManifestHash and PostManifestHash digest the rendered manifests. Equal
	// digests are the proof that pruning changed no published API.
	PreManifestHash  string `json:"preManifestHash"`
	PostManifestHash string `json:"postManifestHash"`
	// Differences are the rendered manifest differences, sorted. A generation
	// that completed has none, because any difference at all refuses.
	Differences []string `json:"differences"`
	// Entries are the published names, sorted.
	Entries []string `json:"entries"`
	// Files are the generated facade files, sorted by module relative path.
	Files []string `json:"files"`
}

// TypesReport records the type policy analysis.
//
// It is this package's own shape rather than the analyzer's, and the reason is
// the same one that governs the whole report. The analyzer's evidence quotes
// source positions, and a position names the scratch work tree the analysis ran
// in, which is a directory on the machine that produced the report. Restating
// the analysis here with those positions rewritten to the tree relative form is
// what lets two runs over different layouts compare byte for byte while keeping
// the evidence a reviewer needs.
type TypesReport struct {
	// Policy is the configured policy.
	Policy string `json:"policy"`
	// Pairs reports every pairing, sorted by internal package path.
	Pairs []TypePairReport `json:"pairs"`
}

// TypePairReport is one pairing's verdict.
type TypePairReport struct {
	Internal string `json:"internal"`
	External string `json:"external"`
	// Action is the decision, such as prune-internal or blocked.
	Action string `json:"action"`
	// Analyses are the individual proofs, in the order the analyzer ran them.
	Analyses []AnalysisReport `json:"analyses"`
	// Evidence and Blockers are every proof's findings, sorted.
	Evidence []string `json:"evidence"`
	Blockers []string `json:"blockers"`
	// BehaviorChanges are the import time effects the change removes.
	BehaviorChanges []TypeBehaviorChange `json:"behaviorChanges"`
	// ExternalAlreadyUsed lists the retained packages that already import the
	// external package, sorted. It is the proof that the substitution has in
	// effect already happened upstream and the internal package is simply dead.
	ExternalAlreadyUsed []string `json:"externalAlreadyUsed"`
}

// Analysis reports one named proof of this pairing.
func (p TypePairReport) Analysis(name string) (AnalysisReport, bool) {
	for _, analysis := range p.Analyses {
		if analysis.Name == name {
			return analysis, true
		}
	}
	return AnalysisReport{}, false
}

// TypeBehaviorChange is one import time effect the substitution removes.
//
// The analyzer's source position is deliberately dropped. It names the scratch
// tree the analysis ran in, and the effect is already identified by the symbol
// that performed it, which is a fact about the upstream code rather than about
// where a copy of it happened to be checked out.
type TypeBehaviorChange struct {
	Kind   string `json:"kind"`
	Symbol string `json:"symbol"`
	Detail string `json:"detail"`
	// Observable reports whether the effect can be reached through the
	// generated public API.
	Observable bool `json:"observable"`
}

// AnalysisReport is one proof's outcome.
type AnalysisReport struct {
	Name     string   `json:"name"`
	Passed   bool     `json:"passed"`
	Evidence []string `json:"evidence"`
	Blockers []string `json:"blockers"`
}

// DependencyReport records the dependency decision.
//
// It is this package's own shape rather than the decider's, because the
// decider's records carry an absolute module directory for provenance and this
// report may not.
type DependencyReport struct {
	// Policy is the configured default action.
	Policy string `json:"policy"`
	// Candidates reports every candidate the graph offered, sorted by import
	// path, whether or not the profile proposed it.
	Candidates []CandidateReport `json:"candidates"`
	// Copy lists the staging paths the decision approves, sorted. For a profile
	// whose answer is external it is empty, and that emptiness is the decision
	// rather than the absence of one.
	Copy []string `json:"copy"`
	// Candidates, Copied, and Refused summarize the decision.
	Totals TotalsReport `json:"totals"`
}

// CandidateReport is one candidate's verdict.
type CandidateReport struct {
	ImportPath  string `json:"importPath"`
	StagingPath string `json:"stagingPath"`
	Module      string `json:"module"`
	Proposed    bool   `json:"proposed"`
	Action      string `json:"action"`
	// FailedGates are the gates that refused it, sorted.
	FailedGates []string `json:"failedGates"`
}

// TotalsReport summarizes one decision.
type TotalsReport struct {
	Candidates int `json:"candidates"`
	Copied     int `json:"copied"`
	Refused    int `json:"refused"`
}

// ProvenanceReport records the root evidence.
type ProvenanceReport struct {
	// LicenseID is the SPDX identifier the profile states and this run verified
	// against the upstream text.
	LicenseID string `json:"licenseId"`
	// LicenseHash digests the upstream licence reproduced at the root.
	LicenseHash string `json:"licenseHash"`
	// UpstreamNotice reports whether the upstream commit carried a NOTICE, and
	// NoticeHash digests it when it did. Both are recorded because embedding an
	// upstream NOTICE that exists is a licence obligation, so its absence is a
	// claim rather than a detail.
	UpstreamNotice bool   `json:"upstreamNotice"`
	NoticeHash     string `json:"noticeHash"`
	// Files are the generated root files, sorted by module relative path.
	Files []string `json:"files"`
	// BehaviorChanges are the documented differences from upstream, sorted by
	// summary.
	BehaviorChanges []BehaviorChangeReport `json:"behaviorChanges"`
	// PublicAPI are the names the README states the module publishes, sorted.
	PublicAPI []string `json:"publicApi"`
}

// BehaviorChangeReport is one documented difference from upstream.
type BehaviorChangeReport struct {
	Summary string `json:"summary"`
	Cause   string `json:"cause"`
}

// OutputReport records the tree the generation produced.
type OutputReport struct {
	// Module is the destination module path.
	Module string `json:"module"`
	// Files is how many files the complete tree holds, root evidence and module
	// metadata included.
	Files int `json:"files"`
	// Packages is how many Go packages it holds.
	Packages int `json:"packages"`
	// ManifestHash digests the complete tree: every destination path, its mode,
	// and its content. Two generations that agree on it produced the same
	// module.
	ManifestHash string `json:"manifestHash"`
	// Materialized reports that the tree was written to a disk. A generation
	// computes the same tree either way, so the hash above does not depend on
	// it.
	Materialized bool `json:"materialized"`
}

// FailureReport records one refused generation.
type FailureReport struct {
	// Stage names the phase that refused, matching PolicyError.Stage.
	Stage string `json:"stage"`
	// Message is the rendered failure with every directory this run owns
	// replaced by a stable placeholder, so two runs over different layouts
	// produce the same text.
	Message string `json:"message"`
	// Policy reports whether the refusal was a finding about the profile rather
	// than an engine or environment failure, which is the distinction CI acts
	// on.
	Policy bool `json:"policy"`
	// Unsupported reports a run shape this engine refuses rather than
	// approximates, which is neither a bad profile nor a broken engine.
	Unsupported bool `json:"unsupported"`
}

// scrubRule replaces one absolute directory with a stable placeholder.
type scrubRule struct {
	dir         string
	placeholder string
}

// init seeds the report with everything known before the first phase runs.
func (r *Report) init(opts Options) {
	r.Schema = ReportSchema
	if opts.Go != nil {
		r.redactor = opts.Go.Redactor()
	} else {
		r.redactor = gitcli.NewRedactor()
	}
	r.Engine = EngineReport{Version: buildinfo.Version, Toolchain: opts.Config.Determinism.Toolchain}
	r.Source = SourceReport{
		RefKind: string(opts.Ref.Kind),
		RefName: opts.Ref.Name,
		Offline: opts.Offline,
		Fetched: opts.Fetch,
	}
	r.Output.Module = opts.Config.Destination.Module
	r.Facade.Package = opts.Config.Facade.Package
	// The order matters: the longest directories are replaced first, so a work
	// root nested inside a cache root does not have its prefix rewritten by the
	// shorter rule before its own rule is reached.
	r.scrub = []scrubRule{
		{filepath.Join(opts.WorkRoot, workDirName), "<work>"},
		{opts.WorkRoot, "<work>"},
		{opts.CacheRoot, "<cache>"},
		{opts.OutputRoot, "<output>"},
		{opts.ProfileDir, "<profile>"},
		{opts.StorePath, "<store>"},
	}
	if opts.SourceRemote != "" {
		r.scrub = append(r.scrub, scrubRule{opts.SourceRemote, "<source-remote>"})
		if parsed, err := url.Parse(opts.SourceRemote); err == nil && parsed.Scheme == "file" && parsed.Path != "" {
			r.scrub = append(r.scrub, scrubRule{filepath.Clean(parsed.Path), "<source-remote>"})
		}
	}
	slices.SortFunc(r.scrub, func(a, b scrubRule) int { return len(b.dir) - len(a.dir) })
}

// addLoaderEnvironment adds operational Go paths and the proxy to the report's
// scrub set. These values are absent from the report itself, but Go diagnostics
// quote them on failures and would otherwise make two refused runs differ by
// machine or disclose a proxy endpoint.
func (r *Report) addLoaderEnvironment(entries []string) {
	pathNames := map[string]bool{
		"HOME": true, "TMPDIR": true, "TEMP": true, "TMP": true,
		"GOCACHE": true, "GOMODCACHE": true, "GOPATH": true,
		"GOTMPDIR": true, "XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true,
	}
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		if name == "GOPROXY" {
			for proxy := range strings.SplitSeq(value, ",") {
				if proxy == "" || proxy == "off" || proxy == "direct" {
					continue
				}
				r.scrub = append(r.scrub, scrubRule{proxy, "<proxy>"})
				if parsed, err := url.Parse(proxy); err == nil && parsed.Scheme == "file" && parsed.Path != "" {
					r.scrub = append(r.scrub, scrubRule{filepath.Clean(parsed.Path), "<proxy>"})
				}
			}
			continue
		}
		if !pathNames[name] {
			continue
		}
		for _, location := range filepath.SplitList(value) {
			if filepath.IsAbs(location) {
				r.scrub = append(r.scrub, scrubRule{filepath.Clean(location), "<go-" + strings.ToLower(name) + ">"})
			}
		}
	}
	slices.SortFunc(r.scrub, func(a, b scrubRule) int { return len(b.dir) - len(a.dir) })
}

// recordExtract summarizes both extraction passes.
func (r *Report) recordExtract(pre, post *extract.Result) {
	r.Engine.ProfileHash = post.Report.Engine.ProfileHash
	r.Source.Commit = post.Report.Source.Commit
	r.Source.RemoteOverridden = post.Report.Source.RemoteOverridden
	r.Extract = ExtractReport{Pre: passReport(pre), Post: passReport(post)}
	// The work trees are only named once a pass has created them, and the type
	// policy's evidence quotes positions inside one of them. Registering them as
	// scrub rules here is what turns those positions into tree relative text
	// before they reach the report.
	r.addScrub(pre.Paths.Worktree, "<pre-worktree>")
	r.addScrub(post.Paths.Worktree, "<post-worktree>")
	// Only the post-prune tree can become the generated module. The baseline is
	// deliberately larger and contains generator markers and files the profile
	// removes; promoting its notices to the generation would make strict mode
	// reject findings that cannot exist in the published output.
	r.addNotices(post.Report.Notices...)
}

// addScrub registers one more directory to replace, keeping the longest rules
// first so a nested directory is rewritten before its parent.
func (r *Report) addScrub(dir, placeholder string) {
	if dir == "" {
		return
	}
	r.scrub = append(r.scrub, scrubRule{dir: dir, placeholder: placeholder})
	slices.SortFunc(r.scrub, func(a, b scrubRule) int { return len(b.dir) - len(a.dir) })
}

// passReport digests and summarizes one plan.
func passReport(result *extract.Result) PassReport {
	report := PassReport{
		ReportHash:      reportDigest(result.Report),
		ManifestHash:    result.Report.Output.ManifestHash,
		Files:           result.Report.Output.Files,
		Packages:        result.Report.Output.Packages,
		ClosurePackages: slices.Clone(result.Report.Closure.Report.Exact.Packages),
		PrunedFiles:     slices.Clone(result.Report.Closure.Report.Exact.PrunedFiles),
		DeniedImports:   slices.Clone(result.Report.Closure.Report.Exact.DeniedImports),
	}
	slices.Sort(report.ClosurePackages)
	slices.Sort(report.PrunedFiles)
	slices.Sort(report.DeniedImports)
	return report
}

// reportDigest hashes a plan report through its own canonical encoding.
//
// The plan's encoder is used rather than a fresh one so the digest is over the
// bytes the plan itself would emit, which is what a reviewer comparing two runs
// would have on disk.
func reportDigest(report extract.Report) string {
	data, err := report.JSON()
	if err != nil {
		// A report that cannot encode is a defect in the plan rather than a
		// finding about the profile, and it has already been surfaced by the
		// phase that produced it. Recording the failure keeps the digest field
		// from silently reading as a real hash.
		return "unencodable"
	}
	return contentDigest(data)
}

// recordStaging summarizes the staging pins.
func (r *Report) recordStaging(root *gomodmap.RootModule, staging []gomodmap.ModuleVersion, cached bool) {
	report := StagingReport{
		SourceModule: root.Path,
		GoDirective:  root.Go,
		Cached:       cached,
		Modules:      make([]ModulePin, 0, len(staging)),
	}
	for _, module := range staging {
		pin := ModulePin{Path: module.Path, Version: module.Version, Commit: module.Commit}
		if staged, ok := root.StagingModuleOf(module.Path); ok {
			pin.Directory = staged.Dir
		}
		report.Modules = append(report.Modules, pin)
	}
	slices.SortFunc(report.Modules, func(a, b ModulePin) int { return cmpString(a.Path, b.Path) })
	r.Staging = report
}

// recordModule summarizes the verified module metadata.
func (r *Report) recordModule(post, pre *modgen.Report) {
	report := renderModuleReport(post)
	report.BaselineGoModHash = contentDigest(pre.GoMod)
	r.Module = report
}

// refreshModule replaces the generated-module facts after an approved copy
// changes the tidied module while retaining the pre-prune baseline identity.
func (r *Report) refreshModule(post *modgen.Report) {
	baseline := r.Module.BaselineGoModHash
	r.Module = renderModuleReport(post)
	r.Module.BaselineGoModHash = baseline
}

// renderModuleReport converts the toolchain report without inventing a second
// source of truth for requirement ordering or directness.
func renderModuleReport(post *modgen.Report) ModuleReport {
	report := ModuleReport{
		GoModHash: contentDigest(post.GoMod),
		Dropped:   slices.Clone(post.Dropped),
	}
	if len(post.GoSum) > 0 {
		report.GoSumHash = contentDigest(post.GoSum)
	}
	for _, requirement := range post.Kept {
		report.Kept = append(report.Kept, RequirementReport{
			Path:     requirement.Path,
			Version:  requirement.Version,
			Indirect: requirement.Indirect,
		})
	}
	for _, requirement := range post.Added {
		report.Added = append(report.Added, RequirementReport{
			Path: requirement.Path, Version: requirement.Version, Indirect: requirement.Indirect,
		})
	}
	for _, reclassified := range post.Reclassified {
		report.Reclassified = append(report.Reclassified, ReclassificationReport{
			Path:     reclassified.Path,
			Indirect: reclassified.Indirect,
		})
	}
	slices.SortFunc(report.Kept, func(a, b RequirementReport) int { return cmpString(a.Path, b.Path) })
	slices.SortFunc(report.Added, func(a, b RequirementReport) int { return cmpString(a.Path, b.Path) })
	slices.SortFunc(report.Reclassified, func(a, b ReclassificationReport) int { return cmpString(a.Path, b.Path) })
	slices.Sort(report.Dropped)
	return report
}

// recordFacade summarizes both manifests and the comparison between them.
func (r *Report) recordFacade(pre, post facade.Result, differences []facade.Difference) {
	report := r.Facade
	report.PreManifestHash = contentDigest([]byte(pre.Manifest.Render()))
	report.PostManifestHash = contentDigest([]byte(post.Manifest.Render()))
	report.Differences = renderDifferences(differences)
	for _, entry := range post.Manifest.Entries {
		report.Entries = append(report.Entries, entry.Name)
	}
	for _, file := range post.Files {
		report.Files = append(report.Files, file.Path)
	}
	slices.Sort(report.Differences)
	slices.Sort(report.Entries)
	slices.Sort(report.Files)
	r.Facade = report
}

// recordTypes summarizes the type policy analysis with every source position
// rewritten to a form that does not name this machine.
func (r *Report) recordTypes(result *typeswap.Result) {
	report := TypesReport{Policy: result.Policy}
	for _, pair := range result.Pairs {
		entry := TypePairReport{
			Internal:            pair.Internal,
			External:            pair.External,
			Action:              string(pair.Action),
			Evidence:            r.scrubAll(pair.Evidence),
			Blockers:            r.scrubAll(pair.Blockers),
			ExternalAlreadyUsed: slices.Clone(pair.ExternalAlreadyUsed),
		}
		for _, analysis := range pair.Analyses {
			entry.Analyses = append(entry.Analyses, AnalysisReport{
				Name:     analysis.Name,
				Passed:   analysis.Passed,
				Evidence: r.scrubAll(analysis.Evidence),
				Blockers: r.scrubAll(analysis.Blockers),
			})
		}
		for _, change := range pair.BehaviorChanges {
			entry.BehaviorChanges = append(entry.BehaviorChanges, TypeBehaviorChange{
				Kind:       change.Kind,
				Symbol:     change.Symbol,
				Detail:     change.Detail,
				Observable: change.Observable,
			})
		}
		slices.Sort(entry.ExternalAlreadyUsed)
		slices.SortFunc(entry.BehaviorChanges, func(a, b TypeBehaviorChange) int {
			if order := cmpString(a.Kind, b.Kind); order != 0 {
				return order
			}
			return cmpString(a.Symbol, b.Symbol)
		})
		report.Pairs = append(report.Pairs, entry)
	}
	slices.SortFunc(report.Pairs, func(a, b TypePairReport) int { return cmpString(a.Internal, b.Internal) })
	r.Types = report
}

// scrubAll rewrites a list of findings, keeping it sorted and non-nil.
func (r *Report) scrubAll(values []string) []string {
	scrubbed := make([]string, 0, len(values))
	for _, value := range values {
		scrubbed = append(scrubbed, r.scrubPaths(value))
	}
	slices.Sort(scrubbed)
	return scrubbed
}

// recordDependencies summarizes the dependency decision.
func (r *Report) recordDependencies(result *deppolicy.Result) {
	report := DependencyReport{
		Policy: result.Policy,
		Copy:   slices.Clone(result.Copy),
		Totals: TotalsReport{
			Candidates: result.Totals.Candidates,
			Copied:     result.Totals.Copied,
			Refused:    result.Totals.Refused,
		},
	}
	for _, candidate := range result.Candidates {
		failed := candidate.FailedGates()
		slices.Sort(failed)
		report.Candidates = append(report.Candidates, CandidateReport{
			ImportPath:  candidate.ImportPath,
			StagingPath: candidate.StagingPath,
			Module:      candidate.Module,
			Proposed:    candidate.Proposed,
			Action:      string(candidate.Action),
			FailedGates: failed,
		})
	}
	slices.SortFunc(report.Candidates, func(a, b CandidateReport) int { return cmpString(a.ImportPath, b.ImportPath) })
	slices.Sort(report.Copy)
	r.Dependencies = report
}

// recordProvenance summarizes the root evidence.
func (r *Report) recordProvenance(options provenance.Options, files []relocate.File) {
	report := ProvenanceReport{
		LicenseID:      options.LicenseID,
		LicenseHash:    contentDigest(options.License),
		UpstreamNotice: len(options.UpstreamNotice) > 0,
		PublicAPI:      slices.Clone(options.PublicAPI),
	}
	if report.UpstreamNotice {
		report.NoticeHash = contentDigest(options.UpstreamNotice)
	}
	for _, file := range files {
		report.Files = append(report.Files, file.Path)
	}
	for _, change := range options.BehaviorChanges {
		report.BehaviorChanges = append(report.BehaviorChanges, BehaviorChangeReport{
			Summary: change.Summary,
			Cause:   change.Cause,
		})
	}
	slices.Sort(report.Files)
	slices.Sort(report.PublicAPI)
	slices.SortFunc(report.BehaviorChanges, func(a, b BehaviorChangeReport) int {
		if order := cmpString(a.Summary, b.Summary); order != 0 {
			return order
		}
		return cmpString(a.Cause, b.Cause)
	})
	r.Provenance = report
}

// recordOutput digests the composed tree.
func (r *Report) recordOutput(set relocate.FileSet, materialized bool) {
	r.Output.Files = len(set.Files)
	// FileSet.Packages records relocated upstream packages. The generated facade
	// is the module-root package and intentionally carries no relocation package
	// attribution, so it contributes one additional Go package.
	r.Output.Packages = len(set.Packages) + 1
	r.Output.ManifestHash = manifestHash(set)
	r.Output.Materialized = materialized
}

// fail records the stage that refused and why.
func (r *Report) fail(stage string, err error) {
	var policy *PolicyError
	r.Failure = &FailureReport{
		Stage:       stage,
		Message:     r.scrubPaths(err.Error()),
		Policy:      errors.As(err, &policy),
		Unsupported: errors.Is(err, ErrUnsupported),
	}
}

// addNotices merges advisory findings, keeping them sorted and unique.
func (r *Report) addNotices(notices ...string) {
	r.Notices = append(r.Notices, notices...)
	slices.Sort(r.Notices)
	r.Notices = slices.Compact(r.Notices)
}

// normalize makes every list non-nil so the encoding depends on the generation
// rather than on whether a phase happened to append anything.
func (r *Report) normalize() {
	r.Notices = nonNil(r.Notices)
	r.Extract.Pre.ClosurePackages = nonNil(r.Extract.Pre.ClosurePackages)
	r.Extract.Pre.PrunedFiles = nonNil(r.Extract.Pre.PrunedFiles)
	r.Extract.Pre.DeniedImports = nonNil(r.Extract.Pre.DeniedImports)
	r.Extract.Post.ClosurePackages = nonNil(r.Extract.Post.ClosurePackages)
	r.Extract.Post.PrunedFiles = nonNil(r.Extract.Post.PrunedFiles)
	r.Extract.Post.DeniedImports = nonNil(r.Extract.Post.DeniedImports)
	r.Staging.Modules = nonNilOf(r.Staging.Modules)
	r.Module.Kept = nonNilOf(r.Module.Kept)
	r.Module.Dropped = nonNil(r.Module.Dropped)
	r.Module.Reclassified = nonNilOf(r.Module.Reclassified)
	r.Facade.Differences = nonNil(r.Facade.Differences)
	r.Facade.Entries = nonNil(r.Facade.Entries)
	r.Facade.Files = nonNil(r.Facade.Files)
	r.Types.Pairs = nonNilOf(r.Types.Pairs)
	for i := range r.Types.Pairs {
		pair := &r.Types.Pairs[i]
		pair.Evidence = nonNil(pair.Evidence)
		pair.Blockers = nonNil(pair.Blockers)
		pair.ExternalAlreadyUsed = nonNil(pair.ExternalAlreadyUsed)
		pair.BehaviorChanges = nonNilOf(pair.BehaviorChanges)
		pair.Analyses = nonNilOf(pair.Analyses)
		for j := range pair.Analyses {
			pair.Analyses[j].Evidence = nonNil(pair.Analyses[j].Evidence)
			pair.Analyses[j].Blockers = nonNil(pair.Analyses[j].Blockers)
		}
	}
	r.Dependencies.Candidates = nonNilOf(r.Dependencies.Candidates)
	r.Dependencies.Copy = nonNil(r.Dependencies.Copy)
	r.Provenance.Files = nonNil(r.Provenance.Files)
	r.Provenance.PublicAPI = nonNil(r.Provenance.PublicAPI)
	r.Provenance.BehaviorChanges = nonNilOf(r.Provenance.BehaviorChanges)
}

// scrubPaths replaces every directory this run owns with a stable placeholder.
//
// A failure message is the one part of the report assembled from text this
// package did not compose, because it may come from a Git or Go subprocess that
// names the directory it was working in. Replacing those directories is what
// keeps a refusal comparable between two runs over different layouts, which is
// the property the determinism check depends on.
func (r *Report) scrubPaths(message string) string {
	if r.redactor != nil {
		message = r.redactor.String(message)
	}
	for _, rule := range r.scrub {
		if rule.dir == "" {
			continue
		}
		message = strings.ReplaceAll(message, rule.dir, rule.placeholder)
	}
	return message
}

// JSON renders the report canonically.
func (r Report) JSON() ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		return nil, fmt.Errorf("generation report: %w", err)
	}
	return out.Bytes(), nil
}

// Summary renders the generation for a person.
//
// It is the one rendering allowed to name absolute directories, because an
// operator who just ran the command needs to know where the module went, and
// nothing compares this text byte for byte.
func (r *Result) Summary() string {
	var b strings.Builder
	report := r.Report

	fmt.Fprintf(&b, "soapbox generate for %s %s at %s\n", report.Source.RefKind, report.Source.RefName, short(report.Source.Commit))
	fmt.Fprintf(&b, "  engine        %s, toolchain %s\n", report.Engine.Version, report.Engine.Toolchain)
	fmt.Fprintf(&b, "  profile       %s\n", report.Engine.ProfileHash)
	fmt.Fprintf(&b, "  module        %s\n", report.Output.Module)
	fmt.Fprintf(&b, "  staging       %d modules pinned%s\n", len(report.Staging.Modules), cachedSuffix(report.Staging.Cached))
	fmt.Fprintf(&b, "  requirements  %d kept, %d added, %d dropped, %d reclassified\n",
		len(report.Module.Kept), len(report.Module.Added), len(report.Module.Dropped), len(report.Module.Reclassified))
	fmt.Fprintf(&b, "  facade        %d entries, %d differences from the unpruned baseline\n",
		len(report.Facade.Entries), len(report.Facade.Differences))
	fmt.Fprintf(&b, "  dependencies  %s, %d candidates, %d copied\n",
		report.Dependencies.Policy, report.Dependencies.Totals.Candidates, report.Dependencies.Totals.Copied)
	fmt.Fprintf(&b, "  provenance    %s, %d behaviour changes\n",
		report.Provenance.LicenseID, len(report.Provenance.BehaviorChanges))
	fmt.Fprintf(&b, "  output        %d files, %d packages\n", report.Output.Files, report.Output.Packages)
	fmt.Fprintf(&b, "  manifest      %s\n", report.Output.ManifestHash)

	for _, notice := range report.Notices {
		fmt.Fprintf(&b, "  notice        %s\n", notice)
	}
	if report.Failure != nil {
		fmt.Fprintf(&b, "  REFUSED       %s: %s\n", report.Failure.Stage, report.Failure.Message)
	}
	if report.Output.Materialized {
		fmt.Fprintf(&b, "  wrote         %s\n", r.Paths.Output)
	}
	return b.String()
}

// cachedSuffix says whether the staging pins came from the index.
func cachedSuffix(cached bool) string {
	if cached {
		return " from the index"
	}
	return ""
}

// short renders the leading twelve characters of a commit, which is what a
// person reads.
func short(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// manifestHash digests a composed tree: every destination path, its mode, and
// its content.
//
// The encoding matches the plan's, so a generation's tree hash and a plan's tree
// hash are the same kind of value and a reviewer comparing them is comparing
// like with like.
func manifestHash(set relocate.FileSet) string {
	files := slices.Clone(set.Files)
	slices.SortFunc(files, func(a, b relocate.File) int { return cmpString(a.Path, b.Path) })

	digest := sha256.New()
	for _, file := range files {
		fmt.Fprintf(digest, "%s\x00%s\x00%s\n", file.Path, file.Mode, contentDigest(file.Contents))
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// contentDigest renders the digest of one file's bytes.
func contentDigest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

// nonNil returns an empty slice rather than a nil one.
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// nonNilOf is nonNil for any element type.
func nonNilOf[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
