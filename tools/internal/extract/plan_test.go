package extract_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/patchset"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// wantDestinations is the tree the fixture profile produces.
var wantDestinations = []string{
	"internal/kk/pkg/apis/rbac/v1/SOAPBOX_PROVENANCE.txt",
	"internal/kk/pkg/apis/rbac/v1/doc.go",
	"internal/kk/pkg/apis/rbac/v1/evaluation_helpers.go",
	"internal/kk/pkg/registry/rbac/validation/SOAPBOX_PROVENANCE.txt",
	"internal/kk/pkg/registry/rbac/validation/rule.go",
	"internal/kk/pkg/registry/rbac/validation/zz_generated.deepcopy.go",
	"internal/kk/plugin/pkg/auth/authorizer/rbac/SOAPBOX_PROVENANCE.txt",
	"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
	"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac_amd64.s",
}

// TestPlanExtractsTheRBACShape is the end to end proof that the composed
// pipeline produces the tree the plan describes.
func TestPlanExtractsTheRBACShape(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))
	report := result.Report

	if report.Schema != extract.ReportSchema {
		t.Errorf("schema = %d, want %d", report.Schema, extract.ReportSchema)
	}
	if report.Source.Commit != up.commit {
		t.Errorf("source commit = %s, want %s", report.Source.Commit, up.commit)
	}
	if !report.Source.Annotated {
		t.Error("the fixture tag is annotated, so the report must say so")
	}
	if !report.Source.CacheCreated || !report.Source.Fetched {
		t.Errorf("first run created=%t fetched=%t, want both", report.Source.CacheCreated, report.Source.Fetched)
	}

	// Four packages before pruning and three after. The internal API package
	// disappears because its only importer was pruned, not because any of its
	// own files were removed.
	if got, want := report.Closure.Report.Observed.PrePrune.Packages, 4; got != want {
		t.Errorf("pre-prune packages = %d, want %d", got, want)
	}
	assertEqual(t, "post-prune packages", report.Closure.Report.Exact.Packages, []string{
		"k8s.io/kubernetes/pkg/apis/rbac/v1",
		"k8s.io/kubernetes/pkg/registry/rbac/validation",
		"k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac",
	})
	assertEqual(t, "removed files", report.Closure.RemovedFiles, []string{
		"pkg/apis/rbac/v1/zz_generated.conversion.go",
		"pkg/apis/rbac/v1/zz_generated.deepcopy.go",
		"pkg/registry/rbac/validation/internal_version_adapter.go",
	})
	assertEqual(t, "external imports", report.Closure.Report.Exact.ExternalPackages, []string{"k8s.io/api/rbac/v1"})
	assertEqual(t, "standard imports", report.Closure.Report.Exact.StandardPackages, []string{"fmt"})

	assertEqual(t, "destinations", destinations(result), wantDestinations)
	assertEqual(t, "provenance files", report.Output.ProvenanceFiles, []string{
		"internal/kk/pkg/apis/rbac/v1/SOAPBOX_PROVENANCE.txt",
		"internal/kk/pkg/registry/rbac/validation/SOAPBOX_PROVENANCE.txt",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/SOAPBOX_PROVENANCE.txt",
	})

	if len(report.Rewrite.Unformatted) > 0 {
		t.Errorf("the pinned gofmt would reformat %v", report.Rewrite.Unformatted)
	}
	if len(report.Rewrite.Unparsed) > 0 {
		t.Errorf("relocation produced unparsable files %v", report.Rewrite.Unparsed)
	}
	if !strings.HasPrefix(report.Output.ManifestHash, "sha256:") {
		t.Errorf("manifest hash %q is not a digest", report.Output.ManifestHash)
	}
	if report.Output.Materialized {
		t.Error("a plan without -materialize must not report a written tree")
	}
}

// TestPlanWidensOntoNestedPackages proves the pattern set grows to reach every
// package the closure discovers, including a pair where one is a subdirectory of
// the other, and that widening never pulls in a sibling subpackage.
func TestPlanWidensOntoNestedPackages(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	opts.KeepWorktree = true
	result := mustPlan(ctx, t, opts)

	// One round per discovered package, plus the round that finally succeeded.
	if got, want := result.Report.Closure.Rounds, 4; got != want {
		t.Errorf("closure rounds = %d, want %d", got, want)
	}
	assertEqual(t, "widened packages", result.Report.Worktree.WidenedPackages, []string{
		"pkg/apis/rbac",
		"pkg/apis/rbac/v1",
		"pkg/registry/rbac/validation",
	})
	// The ancestor's exclusion precedes the nested root's include, which is what
	// lets both materialize while their other subdirectories stay out.
	assertEqual(t, "sparse patterns", result.Report.Worktree.SparsePatterns, []string{
		"/pkg/apis/rbac/*",
		"!/pkg/apis/rbac/*/",
		"/pkg/apis/rbac/v1/*",
		"!/pkg/apis/rbac/v1/*/",
		"/pkg/registry/rbac/validation/*",
		"!/pkg/registry/rbac/validation/*/",
		"/plugin/pkg/auth/authorizer/rbac/*",
		"!/plugin/pkg/auth/authorizer/rbac/*/",
	})

	materialized := worktreePaths(t, result.Paths.Worktree)
	for _, absent := range []string{
		"pkg/apis/rbac/v1beta1/types.go",
		"plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go",
	} {
		if slices.Contains(materialized, absent) {
			t.Errorf("%s is a sibling subpackage and must not be materialized", absent)
		}
	}
	for _, present := range []string{"pkg/apis/rbac/types.go", "pkg/apis/rbac/v1/doc.go"} {
		if !slices.Contains(materialized, present) {
			t.Errorf("%s is a selected package file and must be materialized, got %v", present, materialized)
		}
	}
}

// TestPlanIsDeterministic runs the same plan twice under different directory
// layouts and requires byte identical results.
//
// This is the check the whole report shape is built for. A report that carried
// an absolute path, a wall clock date, or a map iteration order would pass every
// other test in this file and fail here.
func TestPlanIsDeterministic(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	first := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))
	second := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	if first.Paths.Cache == second.Paths.Cache {
		t.Fatal("the two runs shared a cache, so the check proves nothing")
	}
	firstJSON, err := first.Report.JSON()
	if err != nil {
		t.Fatalf("encode first report: %v", err)
	}
	secondJSON, err := second.Report.JSON()
	if err != nil {
		t.Fatalf("encode second report: %v", err)
	}
	if !slices.Equal(firstJSON, secondJSON) {
		t.Errorf("two runs produced different reports:\n%s\n----\n%s", firstJSON, secondJSON)
	}
	if first.Report.Output.ManifestHash != second.Report.Output.ManifestHash {
		t.Errorf("manifest hashes differ: %s and %s", first.Report.Output.ManifestHash, second.Report.Output.ManifestHash)
	}
	if first.Report.Worktree.ScratchAnchor.Commit != second.Report.Worktree.ScratchAnchor.Commit {
		t.Errorf("scratch anchors differ: %s and %s",
			first.Report.Worktree.ScratchAnchor.Commit, second.Report.Worktree.ScratchAnchor.Commit)
	}

	for _, file := range first.Files.Files {
		other, ok := second.Files.Lookup(file.Path)
		if !ok {
			t.Errorf("%s is missing from the second run", file.Path)
			continue
		}
		if !slices.Equal(file.Contents, other.Contents) {
			t.Errorf("%s differs between runs", file.Path)
		}
		if file.Mode != other.Mode {
			t.Errorf("%s mode differs between runs: %s and %s", file.Path, file.Mode, other.Mode)
		}
	}
}

func TestPlanExactCommitUsesThePrefetchedCache(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)

	tagged := mustPlan(ctx, t, opts)
	opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: tagged.Report.Source.Commit}
	opts.Fetch = false
	exact := mustPlan(ctx, t, opts)

	if exact.Report.Source.RefKind != string(extract.RefCommit) {
		t.Errorf("ref kind = %q, want %q", exact.Report.Source.RefKind, extract.RefCommit)
	}
	if exact.Report.Source.RefName != tagged.Report.Source.Commit || exact.Report.Source.Ref != tagged.Report.Source.Commit {
		t.Errorf("exact source = name %q ref %q, want %s", exact.Report.Source.RefName, exact.Report.Source.Ref, tagged.Report.Source.Commit)
	}
	if exact.Report.Source.Fetched {
		t.Error("exact commit run reports a fetch")
	}
	if exact.Report.Output.ManifestHash != tagged.Report.Output.ManifestHash {
		t.Errorf("exact commit manifest = %s, tagged manifest = %s", exact.Report.Output.ManifestHash, tagged.Report.Output.ManifestHash)
	}
}

func TestPlanExactCommitRefusesFetchAndMissingObjects(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: strings.Repeat("f", 40)}

	_, err := extract.Plan(ctx, opts)
	if err == nil || !strings.Contains(err.Error(), "cannot be fetched by object name") {
		t.Fatalf("fetching exact commit = %v, want a fetch refusal", err)
	}

	// Prime the cache through a trusted tag fetch, then ask for an object that
	// was not reachable from it. ResolveCommit must not contact the promisor.
	opts.Ref = extract.Ref{Kind: extract.RefTag, Name: fixtureTag}
	mustPlan(ctx, t, opts)
	opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: strings.Repeat("f", 40)}
	opts.Fetch = false
	_, err = extract.Plan(ctx, opts)
	if err == nil || !strings.Contains(err.Error(), "missing from the cache") {
		t.Fatalf("missing exact commit = %v, want a cache-miss refusal", err)
	}
}

// TestPlanReportCarriesNoLocalState checks that nothing about the machine the
// plan ran on reaches the report.
func TestPlanReportCarriesNoLocalState(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	result := mustPlan(ctx, t, opts)

	encoded, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	rendered := string(encoded)
	for _, secret := range []string{
		opts.CacheRoot,
		opts.WorkRoot,
		opts.OutputRoot,
		opts.ProfileDir,
		opts.SourceRemote,
		up.repo.Dir,
		up.repo.Home,
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("the report leaks the local path %q", secret)
		}
	}
	// The summary is the one rendering that may name directories, and an
	// operator needs it to find what the run left behind.
	if !strings.Contains(result.Summary(), opts.CacheRoot) {
		t.Error("the human summary should name the cache it used")
	}
}

// TestPlanRecordsModesAndGeneratedFiles proves the two per-file properties that
// a relocated tree cannot recover on its own.
func TestPlanRecordsModesAndGeneratedFiles(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	wantModes := map[string]relocate.Mode{
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac_amd64.s": relocate.ModeExecutable,
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go":      relocate.ModeRegular,
		"internal/kk/pkg/apis/rbac/v1/doc.go":                      relocate.ModeRegular,
	}
	wantGenerated := map[string]bool{
		"internal/kk/pkg/registry/rbac/validation/zz_generated.deepcopy.go": true,
		"internal/kk/pkg/registry/rbac/validation/rule.go":                  false,
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac_amd64.s":          false,
	}
	for _, pkg := range result.Report.Relocation.Packages {
		for _, file := range pkg.Files {
			if want, ok := wantModes[file.Destination]; ok && file.Mode != want.String() {
				t.Errorf("%s mode = %s, want %s", file.Destination, file.Mode, want)
			}
			if want, ok := wantGenerated[file.Destination]; ok && file.Generated != want {
				t.Errorf("%s generated = %t, want %t", file.Destination, file.Generated, want)
			}
			if file.SHA256 == "" {
				t.Errorf("%s carries no content digest", file.Destination)
			}
		}
	}
}

// TestPlanStripsDanglingMarkers proves the marker policy: a generator marker is
// removed only when the plan can name the evidence, and the semantic markers
// beside it survive.
func TestPlanStripsDanglingMarkers(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	doc := contentsOf(t, result, "internal/kk/pkg/apis/rbac/v1/doc.go")
	// Removed: its input package left the closure when the adapter was pruned.
	if strings.Contains(doc, "+k8s:conversion-gen=") {
		t.Errorf("the conversion marker points at a pruned package and must be removed:\n%s", doc)
	}
	// Removed: the file its generator writes into this package was pruned.
	if strings.Contains(doc, "+k8s:deepcopy-gen") {
		t.Errorf("the deepcopy marker's output was pruned and it must be removed:\n%s", doc)
	}
	// Kept: it names a public type that is deliberately never relocated.
	if !strings.Contains(doc, "+k8s:conversion-gen-external-types=k8s.io/api/rbac/v1") {
		t.Errorf("the external type marker must survive:\n%s", doc)
	}
	// Kept: the API group is part of the retained behaviour.
	if !strings.Contains(doc, "+groupName=rbac.authorization.k8s.io") {
		t.Errorf("the group name marker must survive:\n%s", doc)
	}

	assertEqual(t, "directive removals", result.Report.Rewrite.DirectiveRemovals, []string{
		"internal/kk/pkg/apis/rbac/v1/doc.go:1 marker-removal - // +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac",
		"internal/kk/pkg/apis/rbac/v1/doc.go:3 marker-removal - // +k8s:deepcopy-gen=package",
	})
}

// TestPlanWritesProvenance checks the per-package record.
func TestPlanWritesProvenance(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	record := contentsOf(t, result, "internal/kk/pkg/apis/rbac/v1/SOAPBOX_PROVENANCE.txt")
	for _, want := range []string{
		"package: internal/kk/pkg/apis/rbac/v1",
		"upstream package: pkg/apis/rbac/v1",
		"upstream repository: https://github.com/kubernetes/kubernetes.git",
		"upstream commit: " + up.commit,
		"upstream: pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/zz_generated.conversion.go",
		"pkg/apis/rbac/v1/zz_generated.deepcopy.go",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the provenance record does not mention %q:\n%s", want, record)
		}
	}
	// The record names the upstream repository the profile configures, never the
	// mirror the run happened to read, so two runs against different mirrors
	// produce the same bytes.
	if strings.Contains(record, up.repo.Dir) {
		t.Errorf("the provenance record leaks the local mirror path:\n%s", record)
	}

	generated := contentsOf(t, result, "internal/kk/pkg/registry/rbac/validation/SOAPBOX_PROVENANCE.txt")
	if !strings.Contains(generated, "generated: yes") {
		t.Errorf("the record must mark the generated file it carries:\n%s", generated)
	}
}

// TestPlanMaterializesOnlyWhenAsked covers the one write a plan may perform
// outside the directories it owns.
func TestPlanMaterializesOnlyWhenAsked(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	t.Run("computed only", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		result := mustPlan(ctx, t, opts)
		if _, err := os.Stat(opts.OutputRoot); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the output tree was written without -materialize: %v", err)
		}
		if result.Report.Output.Materialized {
			t.Error("the report claims a tree that was never written")
		}
	})

	t.Run("materialized", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.Materialize = true
		result := mustPlan(ctx, t, opts)
		if !result.Report.Output.Materialized {
			t.Error("the report does not record the written tree")
		}
		written := treePaths(t, opts.OutputRoot)
		assertEqual(t, "written tree", written, wantDestinations)

		info, err := os.Stat(filepath.Join(opts.OutputRoot, filepath.FromSlash(
			"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac_amd64.s")))
		if err != nil {
			t.Fatalf("stat the executable companion: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("the executable bit was lost, mode is %v", info.Mode())
		}
	})

	t.Run("existing destination", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.Materialize = true
		if err := os.MkdirAll(opts.OutputRoot, 0o750); err != nil {
			t.Fatalf("create the destination: %v", err)
		}
		_, err := extract.Plan(ctx, opts)
		if !errors.Is(err, relocate.ErrDestinationExists) {
			t.Fatalf("error %v does not report an existing destination", err)
		}
	})
}

// TestPlanKeepsTheCacheUsable proves the plan leaves a cache the next run can
// open, which the audit in source.Open is strict about.
func TestPlanKeepsTheCacheUsable(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)

	first := mustPlan(ctx, t, opts)
	if !first.Report.Source.CacheCreated {
		t.Fatal("the first run should have created the cache")
	}
	if first.Report.Worktree.CacheRefsMoved {
		t.Fatal("a plan moved a ref in the shared cache")
	}
	if first.Paths.Worktree != "" {
		t.Errorf("the work tree survived a run without -keep-worktree: %s", first.Paths.Worktree)
	}
	if _, err := os.Stat(filepath.Join(opts.WorkRoot, "src")); err == nil {
		if entries, err := os.ReadDir(filepath.Join(opts.WorkRoot, "src")); err == nil && len(entries) > 0 {
			t.Errorf("the work tree root still holds %d entries", len(entries))
		}
	}

	// The same directories again. Opening the cache re-runs the audit, which
	// refuses any configuration a generated cache would not carry, so a work
	// tree registration or a sparse checkout extension left behind by the first
	// run would fail here.
	second := mustPlan(ctx, t, opts)
	if second.Report.Source.CacheCreated {
		t.Error("the second run cloned again instead of reusing the cache")
	}
	if second.Report.Output.ManifestHash != first.Report.Output.ManifestHash {
		t.Error("reusing a cache changed the result")
	}
}

// TestPlanOffline covers the two offline outcomes.
func TestPlanOffline(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	t.Run("missing cache", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.Fetch = false
		opts.Offline = true
		_, err := extract.Plan(ctx, opts)
		if err == nil {
			t.Fatal("an offline run with no cache must fail")
		}
		if !strings.Contains(err.Error(), "offline run needs an existing cache") {
			t.Fatalf("error %v does not explain the missing cache", err)
		}
		if _, err := os.Stat(opts.CacheRoot); err == nil {
			t.Error("an offline run created a cache directory")
		}
	})

	t.Run("existing cache", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		online := mustPlan(ctx, t, opts)

		opts.Fetch = false
		opts.Offline = true
		offline := mustPlan(ctx, t, opts)
		if offline.Report.Source.Fetched {
			t.Error("an offline run fetched")
		}
		if !offline.Report.Source.Offline {
			t.Error("the report does not record the offline run")
		}
		if offline.Report.Output.ManifestHash != online.Report.Output.ManifestHash {
			t.Error("planning offline changed the result")
		}
	})
}

// TestPlanRefusesUnknownRefs covers a profile naming a ref upstream does not
// have.
func TestPlanRefusesUnknownRefs(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	tests := []struct {
		name string
		ref  extract.Ref
	}{
		{name: "tag", ref: extract.Ref{Kind: extract.RefTag, Name: "v9.99.9"}},
		{name: "branch", ref: extract.Ref{Kind: extract.RefBranch, Name: "release-9.99"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := planOptions(ctx, t, up, fixtureProfile)
			opts.Ref = test.ref
			_, err := extract.Plan(ctx, opts)
			if err == nil {
				t.Fatal("expected a failure")
			}
			// A ref that is not there is a statement about the profile and
			// upstream, so it has to reach the command line as a finding.
			var policy *extract.PolicyError
			if !errors.As(err, &policy) {
				t.Fatalf("error %v is not a policy failure", err)
			}
		})
	}
}

// TestPlanRefusesCredentials covers the two ways a credential could reach a
// command that has no use for one.
func TestPlanRefusesCredentials(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	t.Run("environment", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.LookupEnv = func(name string) (string, bool) {
			return "fake-credential-for-test", name == "SOAPBOX_GITHUB_TOKEN"
		}
		_, err := extract.Plan(ctx, opts)
		if !errors.Is(err, extract.ErrCredentialEnvironment) {
			t.Fatalf("error %v does not refuse the credential environment", err)
		}
		if strings.Contains(err.Error(), "fake-credential-for-test") {
			t.Fatalf("the refusal leaks the credential value: %v", err)
		}
	})

	t.Run("runner", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.Git = credentialedRunner(ctx, t)
		_, err := extract.Plan(ctx, opts)
		if !errors.Is(err, extract.ErrCredentialEnvironment) {
			t.Fatalf("error %v does not refuse the credentialed runner", err)
		}
	})
}

// TestPlanHonoursCancellation proves a cancelled plan reports the cancellation
// rather than a finding about the profile.
func TestPlanHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	cancel()

	_, err := extract.Plan(ctx, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is not a cancellation", err)
	}
	var policy *extract.PolicyError
	if errors.As(err, &policy) {
		t.Fatalf("a cancellation must not be reported as a policy failure: %v", err)
	}
}

// TestPlanRefusesBadProfiles covers the closure contracts a profile can break.
func TestPlanRefusesBadProfiles(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	tests := []struct {
		name     string
		edit     func(string) string
		sentinel error
	}{
		{
			// An upstream rename presents itself as a prune target that is not
			// there, and it must fail rather than quietly shrink the module.
			name:     "prune target renamed upstream",
			edit:     replace("    - pkg/apis/rbac/v1/zz_generated.deepcopy.go\n", "    - pkg/apis/rbac/v1/zz_generated.renamed.go\n"),
			sentinel: closure.ErrPruneMissing,
		},
		{
			name:     "required file not retained",
			edit:     replace("    - pkg/apis/rbac/v1/doc.go\n", "    - pkg/apis/rbac/v1/absent.go\n"),
			sentinel: closure.ErrRequiredMissing,
		},
		{
			// The retained helper package is denied, so the closure that the
			// profile actually produces is refused.
			name:     "denied import stays in the closure",
			edit:     replace("    - k8s.io/kubernetes/pkg/apis/rbac\n", "    - k8s.io/kubernetes/pkg/apis/rbac/v1\n"),
			sentinel: closure.ErrImportDenied,
		},
		{
			name:     "package limit exceeded",
			edit:     replace("    maxPackages: 8\n", "    maxPackages: 2\n"),
			sentinel: closure.ErrLimitExceeded,
		},
		{
			name:     "growth limit exceeded",
			edit:     replace("    maxPackageGrowth: 2\n", "    maxPackageGrowth: 1\n"),
			sentinel: closure.ErrLimitExceeded,
		},
		{
			name:     "file limit exceeded",
			edit:     replace("    maxFiles: 40\n", "    maxFiles: 2\n"),
			sentinel: closure.ErrLimitExceeded,
		},
		{
			name:     "line limit exceeded",
			edit:     replace("    maxNonTestLines: 5000\n", "    maxNonTestLines: 5\n"),
			sentinel: closure.ErrLimitExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := test.edit(fixtureProfile)
			if profile == fixtureProfile {
				t.Fatal("the fixture profile no longer contains the text this case edits")
			}
			_, err := extract.Plan(ctx, planOptions(ctx, t, up, profile))
			if !errors.Is(err, test.sentinel) {
				t.Fatalf("error %v is not %v", err, test.sentinel)
			}
			var policy *extract.PolicyError
			if !errors.As(err, &policy) {
				t.Fatalf("error %v is not a policy failure", err)
			}
		})
	}
}

// replace returns a profile edit that swaps one exact block for another.
func replace(from, to string) func(string) string {
	return func(profile string) string { return strings.Replace(profile, from, to, 1) }
}

// TestPlanAppliesPatches covers a clean patch, a conflicting one, and the
// rollback that keeps a failed pass from leaving a half patched tree.
func TestPlanAppliesPatches(t *testing.T) {
	ctx := t.Context()

	t.Run("clean", func(t *testing.T) {
		up := newUpstream(ctx, t)
		profile := withPatch(fixtureProfile, "patches/export.patch")
		opts := planOptions(ctx, t, up, profile)
		writePatch(t, opts.ProfileDir, "patches/export.patch", cleanPatch)

		result := mustPlan(ctx, t, opts)
		assertEqual(t, "selected patches", result.Report.Patches.Selected, []string{"patches/export.patch"})
		assertEqual(t, "applied patches", result.Report.Patches.Applied, []string{"patches/export.patch"})
		if got, want := result.Report.Patches.Reasserted, 1; got != want {
			t.Errorf("prune reassertions = %d, want %d", got, want)
		}

		rule := contentsOf(t, result, "internal/kk/pkg/registry/rbac/validation/rule.go")
		if !strings.Contains(rule, "ExportedRules") {
			t.Errorf("the patch did not reach the relocated file:\n%s", rule)
		}
		record := contentsOf(t, result, "internal/kk/pkg/registry/rbac/validation/SOAPBOX_PROVENANCE.txt")
		if !strings.Contains(record, "patches/export.patch") {
			t.Errorf("the provenance record does not name the applied patch:\n%s", record)
		}
	})

	t.Run("conflict rolls back", func(t *testing.T) {
		up := newUpstream(ctx, t)
		profile := withPatch(fixtureProfile, "patches/stale.patch")
		opts := planOptions(ctx, t, up, profile)
		opts.KeepWorktree = true
		writePatch(t, opts.ProfileDir, "patches/stale.patch", conflictingPatch)

		_, err := extract.Plan(ctx, opts)
		var conflict *patchset.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("error %v is not a patch conflict", err)
		}
		var policy *extract.PolicyError
		if !errors.As(err, &policy) {
			t.Fatalf("error %v is not a policy failure", err)
		}
		if conflict.PatchID != "patches/stale.patch" {
			t.Errorf("the conflict names patch %q", conflict.PatchID)
		}
		if conflict.Stage != patchset.StageApply {
			t.Errorf("the conflict failed at stage %q, want %q", conflict.Stage, patchset.StageApply)
		}
		if report := conflict.Report(); !strings.Contains(report, "soapbox patch conflict") {
			t.Errorf("the conflict renders no report:\n%s", report)
		}
	})
}

// withPatch adds one patch entry to a profile.
func withPatch(profile, file string) string {
	return strings.Replace(profile, "patches: []\n", "patches:\n  - file: "+file+"\n", 1)
}

// writePatch stores a patch file in the profile repository.
func writePatch(t *testing.T, profileDir, name, diff string) {
	t.Helper()
	full := filepath.Join(profileDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create patch directory: %v", err)
	}
	if err := os.WriteFile(full, []byte(diff), 0o600); err != nil {
		t.Fatalf("write patch: %v", err)
	}
}

// cleanPatch exports a helper the generated module needs, which is the reason
// the profile carries patches at all.
const cleanPatch = `--- a/pkg/registry/rbac/validation/rule.go
+++ b/pkg/registry/rbac/validation/rule.go
@@ -6,3 +6,8 @@ import rbacv1 "k8s.io/api/rbac/v1"
 type RuleResolver interface {
` + " \tRules() []rbacv1.PolicyRule\n" + ` }
+
+// ExportedRules is the helper the generated module needs.
+func ExportedRules() []rbacv1.PolicyRule {
+	return nil
+}
`

// conflictingPatch names context upstream no longer has, which is how a patch
// that outlived its purpose presents itself.
const conflictingPatch = `--- a/pkg/registry/rbac/validation/rule.go
+++ b/pkg/registry/rbac/validation/rule.go
@@ -1,5 +1,6 @@
 package validation

-import rbacv1 "k8s.io/api/rbac/v1alpha1"
+import rbacv1 "k8s.io/api/rbac/v1beta1"
+

 // RuleResolver resolves the rules a subject holds.
`

// TestPlanStrictRefusesNotices proves -strict turns an advisory finding into a
// refusal, using the formatting difference relocation can genuinely introduce.
func TestPlanStrictRefusesNotices(t *testing.T) {
	ctx := t.Context()
	// Upstream groups its own imports with the external ones. Relocation moves
	// the rewritten path to a different position in the sorted group, so the
	// pinned gofmt would reformat the result. Byte range replacement never
	// reorders imports, so the difference is real and the plan reports it.
	up := newUpstreamWith(ctx, t, map[string]string{
		"plugin/pkg/auth/authorizer/rbac/rbac.go": unsortedImports,
	})

	lenient := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))
	assertEqual(t, "unformatted files", lenient.Report.Rewrite.Unformatted, []string{
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
	})
	if len(lenient.Report.Notices) == 0 {
		t.Error("an unformatted file must produce a notice")
	}

	strict := planOptions(ctx, t, up, fixtureProfile)
	strict.Strict = true
	result, err := extract.Plan(ctx, strict)
	var policy *extract.PolicyError
	if !errors.As(err, &policy) {
		t.Fatalf("error %v is not a policy failure", err)
	}
	// A strict refusal still hands back the complete result, because the report
	// is what tells the operator which notices to act on.
	if result == nil {
		t.Fatal("a strict refusal must still report what it found")
	}
	if len(result.Report.Notices) == 0 {
		t.Error("the strict result carries no notices")
	}
}

// unsortedImports is upstream source whose relocated imports no longer sort.
const unsortedImports = `package rbac

import (
	"fmt"

	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"
	rbacregistryvalidation "k8s.io/kubernetes/pkg/registry/rbac/validation"
	"k8s.io/api/rbac/v1"
)

// Authorizer answers authorization requests.
type Authorizer struct {
	resolver rbacregistryvalidation.RuleResolver
	rule     v1.PolicyRule
}

// Describe renders the authorizer.
func (a *Authorizer) Describe() string {
	return fmt.Sprintf("%v %v allows=%t", a.resolver, a.rule, rbacv1helpers.RuleAllows())
}
`
