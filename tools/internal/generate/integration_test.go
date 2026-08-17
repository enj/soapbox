package generate_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/publish"
	enginerelease "github.com/enj/soapbox/tools/internal/release"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/state"
	enginesync "github.com/enj/soapbox/tools/internal/sync"
	"github.com/enj/soapbox/tools/internal/testsupport"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

// endToEnd is one prepared generation over the fixture repository.
type endToEnd struct {
	upstream *upstream
	proxy    string
	roots    roots
	opts     generate.Options
}

// newEndToEnd assembles everything one complete generation needs.
//
// The pieces are real rather than stood in for: a Git repository on disk, a
// module proxy the go command actually resolves through, and the version index a
// previous run would have written. Nothing here replaces a subprocess or a file
// system with a stub, because every property these tests exist to check is a
// property of what those subprocesses do.
func newEndToEnd(ctx context.Context, t *testing.T, mutate func(cfg *config.Config)) *endToEnd {
	t.Helper()
	return newEndToEndWith(ctx, t, nil, mutate)
}

// newEndToEndWith prepares a generation over a fixture with some files replaced.
func newEndToEndWith(ctx context.Context, t *testing.T, overrides map[string]string, mutate func(cfg *config.Config)) *endToEnd {
	t.Helper()

	// The checksum database is switched off for the fixture proxy, which
	// publishes modules no database has ever seen. Inheriting the variable is
	// the one route the runner leaves open, because it deliberately owns every
	// exemption list itself.
	t.Setenv(goSumDBVariable, "off")

	up := newUpstreamWith(ctx, t, overrides)
	// The profile keeps naming the real upstream repository, because a profile
	// is only valid with a real one. Pointing the run at the fixture is what
	// SourceRemote is for, and the report records that an override happened
	// without recording its value.
	cfg := loadProfile(t, "")
	if mutate != nil {
		mutate(cfg)
	}
	dirs := newRoots(t, cfg)
	writeVersionIndex(ctx, t, dirs.store, up.commit)

	proxy := newProxy(t)
	opts := dirs.options(cfg, anonymousGit(t), fixtureGo(t, proxy))
	opts.SourceRemote = up.url()
	opts.Fetch = true
	opts.Materialize = true
	return &endToEnd{upstream: up, proxy: proxy, roots: dirs, opts: opts}
}

func addIntermediateStagingFixtures(ctx context.Context, t *testing.T, e *endToEnd, releaseTags ...string) map[string]generate.StagingSource {
	t.Helper()
	stagingTag := fixtureStagingTag
	if len(releaseTags) > 0 {
		stagingTag = releaseTags[0]
	}
	modulePaths := make([]string, 0, len(proxyModules))
	for modulePath := range proxyModules {
		modulePaths = append(modulePaths, modulePath)
	}
	slices.Sort(modulePaths)

	sources := make(map[string]generate.StagingSource, len(modulePaths))
	commits := make(map[string]string, len(modulePaths))
	pseudos := make(map[string]string, len(modulePaths))
	for _, modulePath := range modulePaths {
		repo := testsupport.NewRepo(ctx, t, testsupport.Options{
			Branch:    "master",
			UserName:  "Kubernetes Publishing Bot",
			UserEmail: "k8s-publishing-bot@users.noreply.github.com",
		})
		repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")
		base := repo.WriteAndCommit(ctx, t, "published.txt", modulePath+"\n",
			"publish staging module\n\n"+gomodmap.KubernetesCommitTrailer+": "+e.upstream.commit+"\n")
		tagger := gitcli.Signature{
			Name:  "Kubernetes Publishing Bot",
			Email: "k8s-publishing-bot@users.noreply.github.com",
			Date:  "2026-01-02T03:04:05Z",
		}
		commitSignature := tagger
		commitSignature.Date = "1767323045 +0000"
		commit := base
		if stagingTag != fixtureStagingTag {
			tree, err := repo.Git.ResolveTree(ctx, base)
			if err != nil {
				t.Fatalf("resolve staging module %s tree: %v", modulePath, err)
			}
			oldTarget, err := repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
				Tree: tree, Parents: []string{base}, Message: "update dependencies for previous tag\n",
				Author: commitSignature, Committer: commitSignature,
			})
			if err != nil {
				t.Fatalf("write staging module %s previous tag spur: %v", modulePath, err)
			}
			commit, err = repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
				Tree: tree, Parents: []string{base},
				Message: "publish current staging module\n\n" + gomodmap.KubernetesCommitTrailer + ": " + e.upstream.commit + "\n",
				Author:  commitSignature, Committer: commitSignature,
			})
			if err != nil {
				t.Fatalf("write staging module %s current lineage: %v", modulePath, err)
			}
			if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
				Name: fixtureStagingTag, Commit: oldTarget,
				Message: "staging " + fixtureStagingTag + "\n", Tagger: tagger,
			}); err != nil {
				t.Fatalf("tag staging module %s anchor: %v", modulePath, err)
			}
		}
		if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
			Name: stagingTag, Commit: commit, Message: "staging " + stagingTag + "\n", Tagger: tagger,
		}); err != nil {
			t.Fatalf("tag staging module %s: %v", modulePath, err)
		}
		commits[modulePath] = commit
		pseudos[modulePath] = "v0.0.0-20260102030405-" + commit[:12]
		sources[modulePath] = generate.StagingSource{Remote: "file://" + repo.Dir}
	}

	for _, modulePath := range modulePaths {
		files := make(map[string]string, len(proxyModules[modulePath]))
		for name, contents := range proxyModules[modulePath] {
			files[name] = contents
		}
		// The real intermediate component-helpers module depends on the API
		// pseudo-version from the same publication wave. Keep the fixture coherent
		// so minimal version selection proves the pins rather than raising one.
		if modulePath == stagingComponentHelpers {
			files["go.mod"] = strings.ReplaceAll(files["go.mod"], fixtureStagingTag, pseudos[stagingAPI])
		}
		commit := commits[modulePath]
		pseudo := pseudos[modulePath]
		writeProxyModule(t, e.proxy, modulePath, pseudo, commit, files)
		versionDir := filepath.Join(e.proxy, filepath.FromSlash(modulePath), "@v")
		info, err := os.ReadFile(filepath.Join(versionDir, pseudo+".info"))
		if err != nil {
			t.Fatalf("read pseudo-version info for %s: %v", modulePath, err)
		}
		if err := os.WriteFile(filepath.Join(versionDir, commit+".info"), info, 0o600); err != nil {
			t.Fatalf("write commit query info for %s: %v", modulePath, err)
		}
	}
	return sources
}

// relayout prepares a second generation over the same upstream commit with
// entirely different directories.
//
// It is what the determinism check needs. Two runs over two fixture repositories
// would differ in their source commit, and the generated evidence records that
// commit, so the trees would legitimately differ and the comparison would prove
// nothing. Holding the commit fixed and moving every directory is the comparison
// that isolates the property being claimed.
func (e *endToEnd) relayout(ctx context.Context, t *testing.T) *endToEnd {
	t.Helper()
	cfg := loadProfile(t, "")
	dirs := newRoots(t, cfg)
	writeVersionIndex(ctx, t, dirs.store, e.upstream.commit)

	opts := dirs.options(cfg, anonymousGit(t), fixtureGo(t, e.proxy))
	opts.SourceRemote = e.upstream.url()
	opts.Fetch = true
	opts.Materialize = true
	return &endToEnd{upstream: e.upstream, proxy: e.proxy, roots: dirs, opts: opts}
}

// fixtureGo builds the Go runner every toolchain phase drives.
//
// Every location the go command keeps state in is the package's own: see
// TestMain for why the module cache is isolated for correctness and the build
// cache for reliability.
func fixtureGo(t *testing.T, proxy string) *gocli.Runner {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, ".config", "go", "telemetry"), 0o750); err != nil {
		t.Fatalf("isolated home: %v", err)
	}
	// Telemetry counters are written asynchronously, so a go command that
	// enabled them could still be writing into the temporary home while the
	// test framework is removing it.
	if err := os.WriteFile(filepath.Join(home, ".config", "go", "telemetry", "mode"), []byte("off\n"), 0o600); err != nil {
		t.Fatalf("isolated home telemetry: %v", err)
	}

	isolation := []string{"HOME=" + home, "GOMODCACHE=" + moduleCache(t), "GOPATH=" + filepath.Join(home, "go")}
	// The build cache is carried over from the process rather than isolated,
	// which is what every other package in this repository that drives the go
	// command does. It is keyed by content, so it cannot serve one fixture's
	// compilation for another's, and a warm cache avoids the burst of concurrent
	// first writes that a cold one performs while the loader compiles the
	// standard library.
	if value, ok := os.LookupEnv("GOCACHE"); ok && value != "" {
		isolation = append(isolation, "GOCACHE="+value)
	}

	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:        t.TempDir(),
		Inherit:    []string{"PATH", goSumDBVariable},
		Isolation:  isolation,
		Proxy:      "file://" + filepath.ToSlash(proxy),
		ModCacheRW: true,
	})
	if err != nil {
		t.Fatalf("go runner: %v", err)
	}
	return runner
}

// generateOnce runs the prepared generation and requires it to succeed.
func (e *endToEnd) generateOnce(ctx context.Context, t *testing.T) *generate.Result {
	t.Helper()
	result, err := generate.Generate(ctx, e.opts)
	if err != nil {
		if result != nil {
			t.Fatalf("generate: %v\n%s", err, result.Summary())
		}
		t.Fatalf("generate: %v", err)
	}
	return result
}

// TestGenerateProvesTheSubstitutionAgainstUpstreamIdentities is the type policy
// gate.
//
// The analysis has to run over the upstream package identities the profile
// names, not over the relocated ones. A profile pairs
// k8s.io/kubernetes/pkg/apis/rbac with a published API package, and proving
// something about a path this engine invented would prove something about the
// engine's own rewriting rather than about the code being extracted.
func TestGenerateProvesTheSubstitutionAgainstUpstreamIdentities(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	if len(result.Report.Types.Pairs) != 1 {
		t.Fatalf("report: %d pairs analysed, want 1", len(result.Report.Types.Pairs))
	}
	pair := result.Report.Types.Pairs[0]

	// The pairing is spelled in upstream terms on both sides.
	if pair.Internal != "k8s.io/kubernetes/pkg/apis/rbac" {
		t.Errorf("pair internal = %s, want the upstream path the profile names", pair.Internal)
	}
	if pair.External != stagingAPI+"/rbac/v1" {
		t.Errorf("pair external = %s, want %s/rbac/v1", pair.External, stagingAPI)
	}
	if pair.Action != string(typeswap.ActionPruneInternal) {
		t.Errorf("pair action = %s, want %s; blockers:\n  %s",
			pair.Action, typeswap.ActionPruneInternal, joinLines(pair.Blockers))
	}

	// The reachability proof is the decisive RBAC fact: retained code already
	// imports the public package and names no unversioned internal symbol. The
	// structural analyses must say they are inapplicable, not claim that the
	// intentionally different internal and public declarations are identical.
	for _, name := range []string{"markers", "reachability", "conversions", "methodSets", "fieldIdentity"} {
		analysis, ok := pair.Analysis(name)
		if !ok {
			t.Errorf("analysis %s was not run", name)
			continue
		}
		if !analysis.Passed {
			t.Errorf("analysis %s did not pass: %s", name, joinLines(analysis.Blockers))
		}
		if name != "markers" && name != "reachability" && !strings.Contains(strings.Join(analysis.Evidence, "\n"), "makes no claim that the declarations are interchangeable") {
			t.Errorf("analysis %s does not explain why type identity is inapplicable", name)
		}
	}

	// A behaviour change the analysis found has to reach the published NOTICE,
	// because a change nobody wrote down is a change nobody can test.
	if len(result.Report.Provenance.BehaviorChanges) == 0 {
		t.Error("report: the type policy found no documented behaviour change, so the disclosure gate proved nothing")
	}
	notice := fileContents(t, result, "NOTICE")
	for _, change := range result.Report.Provenance.BehaviorChanges {
		if !strings.Contains(notice, change.Summary) {
			t.Errorf("NOTICE does not state the behaviour change %q", change.Summary)
		}
	}
}

func TestGenerateIgnoresTestOnlyDependenciesDuringTypeProof(t *testing.T) {
	ctx := t.Context()
	e := newEndToEndWith(ctx, t, map[string]string{
		"plugin/pkg/auth/authorizer/rbac/rbac_test.go": `package rbac

import _ "k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac/bootstrappolicy"
`,
	}, nil)
	result := e.generateOnce(ctx, t)

	for _, file := range treePaths(result) {
		if strings.HasSuffix(file, "_test.go") {
			t.Fatalf("generated module includes test-only source %s", file)
		}
	}
}

// TestGenerateRefusesAnUnprovableSubstitution is the other half of the same
// gate.
//
// Removing the generated conversions removes the mechanical evidence that the
// two declarations match. The profile still prunes the internal package, so the
// run has to refuse: pruning on an unproved equivalence publishes a module that
// claims something nobody demonstrated.
func TestGenerateRefusesAnUnprovableSubstitution(t *testing.T) {
	ctx := t.Context()
	e := newEndToEndWith(ctx, t, map[string]string{
		"pkg/apis/rbac/v1/doc.go": strings.Replace(upstreamAPIDoc,
			"// +k8s:conversion-gen-external-types="+stagingAPI+"/rbac/v1\n", "", 1),
	}, nil)

	result, err := generateFailure(ctx, t, e.opts)
	if !strings.Contains(err.Error(), "is blocked") {
		t.Errorf("generate: error = %v, want a blocked substitution", err)
	}
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after a blocked substitution: %v", statErr)
	}
	if result == nil || result.Report.Failure == nil {
		t.Fatalf("generate: no reviewable report for a blocked substitution: %v", err)
	}
	if result.Report.Failure.Stage != "types" {
		t.Errorf("failure stage = %s, want types", result.Report.Failure.Stage)
	}
	// The evidence that was gathered is still in the report, because a refusal
	// is when it is most worth reading.
	if len(result.Report.Types.Pairs) == 0 {
		t.Error("report: the type analysis was discarded by the refusal")
	}
}

// TestGenerateRefusedCopyProposalStillSucceeds proves a profile that proposes a
// staging copy whose candidate is refused by correctness gates still produces a
// valid module. The authorizer package cannot be copied because the facade
// asserts its interface, so the identity gate refuses it. The decision records
// the refusal and the generation completes with zero copies.
func TestGenerateRefusedCopyProposalStillSucceeds(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Dependencies.Policy = config.DependencyPolicyCopyApproved
		cfg.Dependencies.CopyPackages = []string{"staging/src/" + stagingAPIServer + "/pkg/authorization/authorizer"}
		cfg.Dependencies.Gates.Cost.MaxCopiedPackages = 1
		cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor = 1
		cfg.Dependencies.Gates.Cost.MinPackagesRemoved = 1
	})

	result := e.generateOnce(ctx, t)

	// The proposal is refused by correctness gates, not by the engine.
	if len(result.Report.Dependencies.Copy) != 0 {
		t.Errorf("dependency copy = %v, want none (the authorizer should be refused by correctness gates)", result.Report.Dependencies.Copy)
	}
	if result.Report.Failure != nil {
		t.Errorf("generation failed: %s", result.Report.Failure.Message)
	}
}

// TestGenerateRefusesForbiddenModule proves a profile that names a staging
// module as forbidden causes the generation to refuse, even when the module
// is a legitimate transitive dependency.
func TestGenerateRefusesForbiddenModule(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		// The stagingAPI module is a real dependency of the extracted code.
		// Forbidding it must cause a policy refusal.
		cfg.Dependencies.ForbiddenModules = []string{stagingAPI}
	})

	result, err := generateFailure(ctx, t, e.opts)
	if result == nil || result.Report.Failure == nil {
		t.Fatalf("generate: no reviewable report for a forbidden module: %v", err)
	}
	if result.Report.Failure.Stage != "dependencies" {
		t.Errorf("failure stage = %s, want dependencies", result.Report.Failure.Stage)
	}
	if !strings.Contains(err.Error(), "forbidden module") {
		t.Errorf("error = %v, want it to mention forbidden module", err)
	}
	if !strings.Contains(err.Error(), stagingAPI) {
		t.Errorf("error = %v, want it to name the forbidden module %s", err, stagingAPI)
	}
}

// TestGenerateCopiesApprovedStagingPackage is the end-to-end staging copy proof.
//
// The component-helpers validation package is a pure leaf: no types cross the
// public boundary, no global state, no diamond, and no build-constrained files.
// The retained validation/rule.go imports it. After the copy:
//
//  1. The copied file appears in the generated tree at the correct relocated path.
//  2. The retained file's import is rewritten to point at the copy.
//  3. The component-helpers module requirement drops from go.mod.
//  4. The post-copy module still type checks (graph reload gate).
//  5. The dependency report records the copy.
func TestGenerateCopiesApprovedStagingPackage(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Dependencies.Policy = config.DependencyPolicyCopyApproved
		cfg.Dependencies.CopyPackages = []string{
			"staging/src/" + stagingComponentHelpers + "/text/policy",
		}
		cfg.Dependencies.Gates.Cost.MaxCopiedPackages = 1
		cfg.Dependencies.Gates.Cost.MaxCopiedLines = 500
		cfg.Dependencies.Gates.Cost.MaxGeneratedFiles = 1
		cfg.Dependencies.Gates.Cost.MaxDistinctLicenses = 1
		cfg.Dependencies.Gates.Cost.MaxModuleZipBytes = 1 << 20 // 1 MiB ceiling
		cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor = 10
		cfg.Dependencies.Gates.Cost.MinModulesRemoved = 1
		cfg.Dependencies.Gates.Cost.MinPackagesRemoved = 1
		cfg.Dependencies.Gates.Cost.MinLinesRemoved = 1
		// The minimumLeverage override is required because go/packages does not
		// populate Syntax (and therefore countLines returns 0) for packages
		// resolved through a file proxy, so linesRemoved is unmeasured and the
		// leverage gate fails on MinLinesRemoved. This is a fixture limitation,
		// not a materializer bug.
		cfg.Dependencies.Overrides = []config.DependencyOverride{{
			Package:       "staging/src/" + stagingComponentHelpers + "/text/policy",
			Gate:          "minimumLeverage",
			Justification: "file proxy does not populate go/packages Syntax for deps, so linesRemoved is zero",
			Approver:      "test",
			ExpiresAfter:  "v1.99",
		}}
	})

	result := e.generateOnce(ctx, t)

	if result.Report.Failure != nil {
		t.Fatalf("generate: refused at %s: %s", result.Report.Failure.Stage, result.Report.Failure.Message)
	}
	for _, c := range result.Report.Dependencies.Candidates {
		t.Logf("candidate %s: action=%s proposed=%v failed=%v", c.ImportPath, c.Action, c.Proposed, c.FailedGates)
	}

	// 1. The copied file appears at the correct relocated path.
	copiedPath := "internal/kk/staging/src/" + stagingComponentHelpers + "/text/policy/matcher.go"
	got := treePaths(result)
	if !slices.Contains(got, copiedPath) {
		t.Errorf("generated tree does not contain the copied file %s, got:\n  %s", copiedPath, joinLines(got))
	}

	// 2. The retained validation/rule.go import is rewritten to the copy.
	rulePath := "internal/kk/pkg/registry/rbac/validation/rule.go"
	ruleContents := fileContents(t, result, rulePath)
	relocatedImport := e.opts.Config.Destination.Module + "/internal/kk/staging/src/" + stagingComponentHelpers
	if !strings.Contains(ruleContents, relocatedImport) {
		t.Errorf("retained rule.go does not import the relocated copy %s:\n%s", relocatedImport, ruleContents)
	}
	if strings.Contains(ruleContents, `"`+stagingComponentHelpers+`/`) {
		t.Errorf("retained rule.go still imports the external staging module %s:\n%s", stagingComponentHelpers, ruleContents)
	}

	// 3. The component-helpers module requirement dropped from go.mod.
	goMod := fileContents(t, result, "go.mod")
	if strings.Contains(goMod, stagingComponentHelpers) {
		t.Errorf("go.mod still requires the copied staging module %s:\n%s", stagingComponentHelpers, goMod)
	}
	// The other staging modules should still be required.
	if !strings.Contains(goMod, stagingAPI) {
		t.Errorf("go.mod dropped the still-needed staging module %s:\n%s", stagingAPI, goMod)
	}

	// 4. The report records the copy.
	deps := result.Report.Dependencies
	if len(deps.Copy) != 1 {
		t.Fatalf("dependency copy = %v, want exactly one staging path", deps.Copy)
	}
	if deps.Copy[0] != "staging/src/"+stagingComponentHelpers+"/text/policy" {
		t.Errorf("copied staging path = %s, want staging/src/%s/text/policy", deps.Copy[0], stagingComponentHelpers)
	}
	if deps.Totals.Copied != 1 {
		t.Errorf("copied total = %d, want 1", deps.Totals.Copied)
	}

	// 5. The tree was written and what was written matches what was reported.
	if !result.Report.Output.Materialized {
		t.Error("report: Materialized = false, want the tree to have been written")
	}
	written := walkTree(t, e.roots.output)
	if !slices.Equal(written, got) {
		t.Errorf("written tree differs from the reported one:\n written:\n  %s\n reported:\n  %s", joinLines(written), joinLines(got))
	}

	// 6. The per-package provenance record names the staging module, not the
	// Kubernetes root repository. The file bytes came from the module cache at
	// the staging module's pinned version, so the provenance must say so.
	copiedPkgDir := "internal/kk/staging/src/" + stagingComponentHelpers + "/text/policy"
	provenancePath := copiedPkgDir + "/SOAPBOX_PROVENANCE.txt"
	if !slices.Contains(got, provenancePath) {
		t.Fatalf("generated tree has no provenance record at %s", provenancePath)
	}
	provenance := fileContents(t, result, provenancePath)
	// The provenance names the validated Origin URL (the canonical staging
	// repo), not the module path or the Kubernetes root repo.
	expectedOriginURL := "https://github.com/kubernetes/component-helpers"
	if !strings.Contains(provenance, "upstream repository: "+expectedOriginURL) {
		t.Errorf("copy provenance does not name the Origin URL %s:\n%s",
			expectedOriginURL, provenance)
	}
	if !strings.Contains(provenance, "upstream commit: "+stagingCommits[stagingComponentHelpers]) {
		t.Errorf("copy provenance does not name the staging commit %s:\n%s",
			stagingCommits[stagingComponentHelpers], provenance)
	}
	// It must NOT name the Kubernetes root repo — that would be claiming
	// provenance from a tree the bytes were not read from.
	if strings.Contains(provenance, "kubernetes/kubernetes") {
		t.Errorf("copy provenance incorrectly names the Kubernetes root repository:\n%s", provenance)
	}
	for _, want := range []string{
		"upstream package: text/policy",
		"upstream: text/policy/matcher.go",
	} {
		if !strings.Contains(provenance, want) {
			t.Errorf("copy provenance does not use module-relative source %q:\n%s", want, provenance)
		}
	}
	if strings.Contains(provenance, "upstream package: staging/src/") {
		t.Errorf("copy provenance uses a Kubernetes-root source path:\n%s", provenance)
	}

	// 7. The retained validation/rule.go provenance records the staging import
	// rewrite alongside the original extraction changes. The import of
	// component-helpers/text/policy was rewritten to the relocated copy, and
	// the provenance must say so.
	retainedPkgDir := "internal/kk/pkg/registry/rbac/validation"
	retainedProvFile := retainedPkgDir + "/SOAPBOX_PROVENANCE.txt"
	retainedProvenance := fileContents(t, result, retainedProvFile)
	// The provenance should contain the staging module path as part of the
	// recorded import rewrite change.
	if !strings.Contains(retainedProvenance, stagingComponentHelpers) {
		t.Errorf("retained package provenance does not record the staging import rewrite for %s:\n%s",
			stagingComponentHelpers, retainedProvenance)
	}

	// 8. The copied module's LICENSE file appears in the generated tree at the
	// correct relocated path beside the copied package.
	copiedLicensePath := "internal/kk/staging/src/" + stagingComponentHelpers + "/LICENSE"
	if !slices.Contains(got, copiedLicensePath) {
		t.Errorf("generated tree does not contain the copied module's LICENSE at %s, got:\n  %s",
			copiedLicensePath, joinLines(got))
	} else if copiedLicense := fileContents(t, result, copiedLicensePath); copiedLicense != fixtureLicense {
		t.Errorf("copied module LICENSE differs from the verified source text:\n%s", copiedLicense)
	}

	// 9. The NOTICE records the copied package with its Origin URL, version,
	// import path, and licence.
	notice := fileContents(t, result, "NOTICE")
	if !strings.Contains(notice, "Copied dependency packages") {
		t.Error("NOTICE does not contain the copied dependency section")
	}
	if !strings.Contains(notice, stagingComponentHelpers+"/text/policy") {
		t.Errorf("NOTICE does not name the copied package %s/text/policy:\n%s",
			stagingComponentHelpers, notice)
	}
	if !strings.Contains(notice, expectedOriginURL) {
		t.Errorf("NOTICE does not contain Origin URL %s:\n%s", expectedOriginURL, notice)
	}
	if !strings.Contains(notice, stagingComponentHelpers+"@"+fixtureStagingTag) {
		t.Errorf("NOTICE does not contain module@version %s@%s:\n%s",
			stagingComponentHelpers, fixtureStagingTag, notice)
	}
	if !strings.Contains(notice, stagingCommits[stagingComponentHelpers]) {
		t.Errorf("NOTICE does not contain staging commit %s:\n%s",
			stagingCommits[stagingComponentHelpers], notice)
	}
}

// TestGenerateCopyThenForbidRemovesModule proves that a copy-approved profile
// that copies a package from a staging module and then lists that module as
// forbidden succeeds: the copy replaces the external dependency, the forbidden
// module check runs after the copy, and the module is absent from go.mod,
// go.sum, go list -m all, and all import paths in the generated tree.
func TestGenerateCopyThenForbidRemovesModule(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Dependencies.Policy = config.DependencyPolicyCopyApproved
		cfg.Dependencies.CopyPackages = []string{
			"staging/src/" + stagingComponentHelpers + "/text/policy",
		}
		cfg.Dependencies.ForbiddenModules = []string{stagingComponentHelpers}
		cfg.Dependencies.Gates.Cost.MaxCopiedPackages = 1
		cfg.Dependencies.Gates.Cost.MaxCopiedLines = 500
		cfg.Dependencies.Gates.Cost.MaxGeneratedFiles = 1
		cfg.Dependencies.Gates.Cost.MaxDistinctLicenses = 1
		cfg.Dependencies.Gates.Cost.MaxModuleZipBytes = 1 << 20
		cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor = 10
		cfg.Dependencies.Gates.Cost.MinModulesRemoved = 1
		cfg.Dependencies.Gates.Cost.MinPackagesRemoved = 1
		cfg.Dependencies.Gates.Cost.MinLinesRemoved = 1
		cfg.Dependencies.Overrides = []config.DependencyOverride{{
			Package:       "staging/src/" + stagingComponentHelpers + "/text/policy",
			Gate:          "minimumLeverage",
			Justification: "file proxy does not populate go/packages Syntax for deps, so linesRemoved is zero",
			Approver:      "test",
			ExpiresAfter:  "v1.99",
		}}
	})

	result := e.generateOnce(ctx, t)

	if result.Report.Failure != nil {
		t.Fatalf("generate: refused at %s: %s", result.Report.Failure.Stage, result.Report.Failure.Message)
	}

	// The copied file appears.
	copiedPath := "internal/kk/staging/src/" + stagingComponentHelpers + "/text/policy/matcher.go"
	got := treePaths(result)
	if !slices.Contains(got, copiedPath) {
		t.Errorf("generated tree does not contain copied file %s", copiedPath)
	}

	// The forbidden module is absent from go.mod.
	goMod := fileContents(t, result, "go.mod")
	if strings.Contains(goMod, stagingComponentHelpers) {
		t.Errorf("go.mod still requires forbidden module %s:\n%s", stagingComponentHelpers, goMod)
	}
	if got, want := result.Report.Module.GoModHash, fmt.Sprintf("%x", sha256.Sum256([]byte(goMod))); got != want {
		t.Errorf("reported go.mod hash = %s, want final hash %s", got, want)
	}
	for _, requirement := range result.Report.Module.Kept {
		if requirement.Path == stagingComponentHelpers {
			t.Errorf("module report still keeps forbidden module: %#v", requirement)
		}
	}
	if !slices.Contains(result.Report.Module.Dropped, stagingComponentHelpers) {
		t.Errorf("module report dropped = %v, want %s", result.Report.Module.Dropped, stagingComponentHelpers)
	}

	// The forbidden module is absent from go.sum.
	if slices.Contains(got, "go.sum") {
		goSum := fileContents(t, result, "go.sum")
		if strings.Contains(goSum, stagingComponentHelpers) {
			t.Errorf("go.sum contains forbidden module %s:\n%s", stagingComponentHelpers, goSum)
		}
	}

	// No Go file imports a package belonging to the forbidden module.
	for _, path := range got {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		contents := fileContents(t, result, path)
		if strings.Contains(contents, `"`+stagingComponentHelpers+"/") || strings.Contains(contents, `"`+stagingComponentHelpers+`"`) {
			t.Errorf("file %s still imports the forbidden module %s:\n%s", path, stagingComponentHelpers, contents)
		}
	}
}

func TestGenerateDualApiserverCompatibilityModes(t *testing.T) {
	ctx := t.Context()

	t.Run("external preserves apiserver identity", func(t *testing.T) {
		e := newEndToEnd(ctx, t, nil)
		result := e.generateOnce(ctx, t)
		goMod := fileContents(t, result, "go.mod")
		if !strings.Contains(goMod, stagingAPIServer+" "+fixtureStagingTag) {
			t.Errorf("external go.mod does not require %s:\n%s", stagingAPIServer, goMod)
		}
		assertions := fileContents(t, result, "zz_generated_assertions.go")
		if !strings.Contains(assertions, stagingAPIServer+"/pkg/authorization/authorizer") {
			t.Errorf("external assertions do not use real apiserver identity:\n%s", assertions)
		}
	})

	t.Run("local removes apiserver and exports local types", func(t *testing.T) {
		e := newEndToEnd(ctx, t, func(cfg *config.Config) {
			cfg.Compatibility.Apiserver = config.CompatibilityApiserverLocal
			cfg.Dependencies.ForbiddenModules = append(cfg.Dependencies.ForbiddenModules, stagingAPIServer)
		})
		result := e.generateOnce(ctx, t)
		goMod := fileContents(t, result, "go.mod")
		if strings.Contains(goMod, stagingAPIServer) {
			t.Errorf("local go.mod retains apiserver:\n%s", goMod)
		}
		for _, file := range result.Files.Files {
			if strings.HasSuffix(file.Path, ".go") && strings.Contains(string(file.Contents), `"`+stagingAPIServer+`/`) {
				t.Errorf("local file %s imports apiserver:\n%s", file.Path, file.Contents)
			}
		}
		facade := fileContents(t, result, "authorizer.go")
		for _, name := range []string{
			"UserInfo", "DefaultUserInfo", "Attributes", "AttributesRecord",
			"Decision", "DecisionDeny", "DecisionAllow", "DecisionNoOpinion",
			"Authorizer", "RuleResolver", "ResourceRuleInfo",
			"DefaultResourceRuleInfo", "NonResourceRuleInfo", "DefaultNonResourceRuleInfo",
		} {
			if !strings.Contains(facade, name) {
				t.Errorf("local facade does not expose %s:\n%s", name, facade)
			}
		}
		assertions := fileContents(t, result, "zz_generated_assertions.go")
		if strings.Contains(assertions, stagingAPIServer) {
			t.Errorf("local assertions retain external apiserver:\n%s", assertions)
		}
		validation := fileContents(t, result, "internal/kk/pkg/registry/rbac/validation/rule.go")
		if strings.Contains(validation, "/pkg/endpoints/request") || strings.Contains(validation, "UserFrom(ctx)") || strings.Contains(validation, "NamespaceFrom(ctx)") {
			t.Errorf("local validation retains private request context identity:\n%s", validation)
		}
		notice := fileContents(t, result, "NOTICE")
		if !strings.Contains(notice, "not assignable to k8s.io/apiserver types") || !strings.Contains(notice, "explicit user and namespace") {
			t.Errorf("local NOTICE does not disclose compatibility break:\n%s", notice)
		}
	})
}

// TestGenerateProducesCompleteModule is the end-to-end proof.
//
// It asserts what a consumer of the generated module would find: the relocated
// upstream code, a go.mod that resolves, the curated facade, and the root
// evidence. Each of those is produced by a different phase, so the test is also
// the proof that the phases compose rather than merely each working alone.
func TestGenerateProducesCompleteModule(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	if result.Report.Failure != nil {
		t.Fatalf("generate: refused at %s: %s", result.Report.Failure.Stage, result.Report.Failure.Message)
	}

	// The tree a consumer sees.
	want := []string{
		"LICENSE",
		"NOTICE",
		"README.md",
		"authorizer.go",
		"doc.go",
		"go.mod",
		"go.sum",
		"zz_generated_assertions.go",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
		"internal/kk/pkg/registry/rbac/validation/rule.go",
		"internal/kk/pkg/apis/rbac/v1/doc.go",
		"internal/kk/pkg/apis/rbac/v1/evaluation_helpers.go",
	}
	got := treePaths(result)
	if missing, ok := containsAll(got, want); !ok {
		t.Errorf("generated tree is missing %s, got:\n  %s", missing, joinLines(got))
	}

	// Pruning removed the registration file, so the unversioned internal API
	// package must not have reached the module at all.
	for _, path := range got {
		if strings.Contains(path, "pkg/apis/rbac/v1/register.go") {
			t.Errorf("generated tree contains the pruned file %s", path)
		}
		if strings.HasSuffix(path, "internal/kk/pkg/apis/rbac/types.go") {
			t.Errorf("generated tree contains the denied internal API package: %s", path)
		}
	}

	// The tree was actually written, and what was written is what was reported.
	if !result.Report.Output.Materialized {
		t.Error("report: Materialized = false, want the tree to have been written")
	}
	written := walkTree(t, e.roots.output)
	if !slices.Equal(written, got) {
		t.Errorf("written tree differs from the reported one:\n written:\n  %s\n reported:\n  %s", joinLines(written), joinLines(got))
	}
}

// TestGeneratePinsStagingAndTidiesModule proves the module metadata is the
// toolchain's answer rather than the engine's guess.
func TestGeneratePinsStagingAndTidiesModule(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)
	if result.Report.Source.ReleaseTag != fixtureStagingTag {
		t.Errorf("release tag = %q, want %q", result.Report.Source.ReleaseTag, fixtureStagingTag)
	}

	// Every required staging module was pinned, at the version the release
	// policy maps the upstream tag onto.
	pinned := make([]string, 0, len(result.Report.Staging.Modules))
	for _, module := range result.Report.Staging.Modules {
		pinned = append(pinned, module.Path)
		if module.Version != fixtureStagingTag {
			t.Errorf("staging pin %s: version = %s, want %s", module.Path, module.Version, fixtureStagingTag)
		}
		if module.Commit != stagingCommits[module.Path] {
			t.Errorf("staging pin %s: commit = %s, want %s", module.Path, module.Commit, stagingCommits[module.Path])
		}
		if module.Directory != "staging/src/"+module.Path {
			t.Errorf("staging pin %s: directory = %s, want staging/src/%s", module.Path, module.Directory, module.Path)
		}
	}
	if !slices.Equal(pinned, stagingPaths()) {
		t.Errorf("staging pins = %v, want %v", pinned, stagingPaths())
	}
	if !result.Report.Staging.Cached {
		t.Error("report: staging pins were resolved rather than read from the index")
	}

	// The published go.mod requires the pinned versions and carries no
	// replacement, which is what makes it resolvable by a consumer.
	goMod := fileContents(t, result, "go.mod")
	if strings.Contains(goMod, "replace ") {
		t.Errorf("generated go.mod carries a replacement:\n%s", goMod)
	}
	if !strings.Contains(goMod, "module "+e.opts.Config.Destination.Module) {
		t.Errorf("generated go.mod does not declare the destination module:\n%s", goMod)
	}
	for _, modulePath := range stagingPaths() {
		if !strings.Contains(goMod, modulePath+" "+fixtureStagingTag) {
			t.Errorf("generated go.mod does not require %s %s:\n%s", modulePath, fixtureStagingTag, goMod)
		}
	}
	// go.sum exists because the module requires code outside the standard
	// library, and a consumer cannot build without it.
	if sum := fileContents(t, result, "go.sum"); !strings.Contains(sum, stagingAPI) {
		t.Errorf("generated go.sum does not cover %s:\n%s", stagingAPI, sum)
	}
}

func TestGenerateExactCommitUsesAnIntermediateVersionIndexEntry(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	e.opts.Materialize = false
	tagged := e.generateOnce(ctx, t)

	store, err := gomodmap.NewStore(e.roots.store)
	if err != nil {
		t.Fatalf("open version index: %v", err)
	}
	index, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load version index: %v", err)
	}
	entry, ok := index.Lookup(e.upstream.commit)
	if !ok {
		t.Fatalf("version index has no release entry for %s", e.upstream.commit)
	}
	intermediate := gomodmap.NewIndex()
	if err := intermediate.Put(gomodmap.Entry{Source: entry.Source, Modules: entry.Modules}); err != nil {
		t.Fatalf("build intermediate entry: %v", err)
	}
	intermediatePath := e.roots.store + ".intermediate"
	intermediateStore, err := gomodmap.NewStore(intermediatePath)
	if err != nil {
		t.Fatalf("open intermediate version index: %v", err)
	}
	if err := intermediateStore.Save(ctx, intermediate); err != nil {
		t.Fatalf("save intermediate entry: %v", err)
	}

	e.opts.StorePath = intermediatePath
	e.opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: e.upstream.commit}
	e.opts.ReleaseContext = fixtureTag
	e.opts.HistoryAnchor = e.upstream.commit
	e.opts.HistoryAnchorRelease = fixtureTag
	e.opts.Fetch = false
	exact := e.generateOnce(ctx, t)
	if exact.Report.Source.RefKind != string(extract.RefCommit) || exact.Report.Source.RefName != e.upstream.commit {
		t.Errorf("exact source = %s %s, want commit %s", exact.Report.Source.RefKind, exact.Report.Source.RefName, e.upstream.commit)
	}
	if exact.Report.Source.ReleaseTag != "" {
		t.Errorf("exact commit release tag = %q, want empty", exact.Report.Source.ReleaseTag)
	}
	if !exact.Report.Staging.Cached {
		t.Error("exact commit did not reuse its intermediate staging entry")
	}
	if exact.Report.Output.ManifestHash == tagged.Report.Output.ManifestHash {
		t.Error("exact commit and release-tag provenance unexpectedly produced one manifest")
	}
}

func TestGenerateExactCommitResolvesIntermediateStagingHistory(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Dependencies.Policy = config.DependencyPolicyExternal
		cfg.Dependencies.CopyPackages = nil
		cfg.Dependencies.ForbiddenModules = nil
		cfg.Dependencies.Overrides = nil
	})
	e.opts.Materialize = false
	// Fetch the source release first. Exact-commit generation itself refuses to
	// fetch by object name and consumes this bounded cache.
	e.generateOnce(ctx, t)

	e.opts.StorePath = e.roots.store + ".cold-intermediate"
	e.opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: e.upstream.commit}
	e.opts.ReleaseContext = fixtureTag
	e.opts.HistoryAnchor = e.upstream.commit
	e.opts.HistoryAnchorRelease = fixtureTag
	e.opts.StagingSources = addIntermediateStagingFixtures(ctx, t, e)
	e.opts.Fetch = false
	result := e.generateOnce(ctx, t)

	if result.Report.Staging.Cached {
		t.Error("cold intermediate resolution was reported as cached")
	}
	if len(result.Report.Staging.Modules) != len(e.opts.StagingSources) {
		t.Fatalf("resolved %d staging modules, want %d", len(result.Report.Staging.Modules), len(e.opts.StagingSources))
	}
	for _, pinned := range result.Report.Staging.Modules {
		if !strings.Contains(pinned.Version, "-20260102030405-") {
			t.Errorf("staging module %s version = %q, want a Go-resolved pseudo-version", pinned.Path, pinned.Version)
		}
		if pinned.Commit == "" {
			t.Errorf("staging module %s records no mapped commit", pinned.Path)
		}
	}

	store, err := gomodmap.NewStore(e.opts.StorePath)
	if err != nil {
		t.Fatalf("open intermediate index: %v", err)
	}
	index, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load intermediate index: %v", err)
	}
	entry, ok := index.Lookup(e.upstream.commit)
	if !ok {
		t.Fatalf("intermediate index has no entry for %s", e.upstream.commit)
	}
	if entry.Tag != "" {
		t.Errorf("intermediate entry tag = %q, want empty", entry.Tag)
	}
	if len(entry.Modules) != len(result.Report.Staging.Modules) {
		t.Errorf("intermediate index records %d modules, report has %d", len(entry.Modules), len(result.Report.Staging.Modules))
	}
}

func TestGenerateExactCommitCopiesFromPseudoVersionEvidence(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Dependencies.Policy = config.DependencyPolicyCopyApproved
		cfg.Dependencies.CopyPackages = []string{
			"staging/src/" + stagingComponentHelpers + "/text/policy",
		}
		cfg.Dependencies.ForbiddenModules = []string{stagingComponentHelpers}
		cfg.Dependencies.Gates.Cost.MaxCopiedPackages = 1
		cfg.Dependencies.Gates.Cost.MaxCopiedLines = 500
		cfg.Dependencies.Gates.Cost.MaxGeneratedFiles = 1
		cfg.Dependencies.Gates.Cost.MaxDistinctLicenses = 1
		cfg.Dependencies.Gates.Cost.MaxModuleZipBytes = 1 << 20
		cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor = 10
		cfg.Dependencies.Gates.Cost.MinModulesRemoved = 1
		cfg.Dependencies.Gates.Cost.MinPackagesRemoved = 1
		cfg.Dependencies.Gates.Cost.MinLinesRemoved = 1
		cfg.Dependencies.Overrides = []config.DependencyOverride{{
			Package:       "staging/src/" + stagingComponentHelpers + "/text/policy",
			Gate:          "minimumLeverage",
			Justification: "file proxy does not populate go/packages Syntax for deps, so linesRemoved is zero",
			Approver:      "test",
			ExpiresAfter:  "v1.99",
		}}
	})
	e.opts.Materialize = false
	e.generateOnce(ctx, t)

	e.opts.StorePath = e.roots.store + ".cold-pseudo-copy"
	e.opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: e.upstream.commit}
	e.opts.ReleaseContext = fixtureTag
	e.opts.HistoryAnchor = e.upstream.commit
	e.opts.HistoryAnchorRelease = fixtureTag
	e.opts.StagingSources = addIntermediateStagingFixtures(ctx, t, e)
	e.opts.Fetch = false
	result := e.generateOnce(ctx, t)

	var componentVersion, componentCommit string
	for _, pinned := range result.Report.Staging.Modules {
		if pinned.Path == stagingComponentHelpers {
			componentVersion = pinned.Version
			componentCommit = pinned.Commit
		}
	}
	if !strings.Contains(componentVersion, "-20260102030405-") {
		t.Fatalf("component-helpers version = %q, want pseudo-version", componentVersion)
	}
	copiedPath := "internal/kk/staging/src/" + stagingComponentHelpers + "/text/policy/matcher.go"
	if !slices.Contains(treePaths(result), copiedPath) {
		t.Fatalf("generated tree does not contain pseudo-version copy %s", copiedPath)
	}
	notice := fileContents(t, result, "NOTICE")
	for _, want := range []string{
		stagingComponentHelpers + "@" + componentVersion,
		"upstream commit: " + componentCommit,
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("NOTICE does not contain %q:\n%s", want, notice)
		}
	}
}

func TestReplayDAGProjectsInitialReleaseWithRealGeneration(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	e.opts.Materialize = false

	cache, err := source.Open(ctx, source.Options{
		Remote: e.upstream.url(), CacheRoot: e.roots.cache,
		WorktreeRoot: filepath.Join(e.roots.work, "dag-source-worktrees"),
		Git:          e.opts.Git,
	})
	if err != nil {
		t.Fatalf("open source cache: %v", err)
	}
	if err := cache.Fetch(ctx, source.Refs{Tags: []string{fixtureTag}}); err != nil {
		t.Fatalf("fetch source release: %v", err)
	}
	resolved, err := cache.Resolve(ctx, source.Refs{Tags: []string{fixtureTag}})
	if err != nil || len(resolved) != 1 {
		t.Fatalf("resolve source release = %#v, %v", resolved, err)
	}

	destination := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Soapbox DAG Test", UserEmail: "dag@example.com",
	})
	for path, contents := range map[string]string{
		".github/workflows/ci.yml":   "name: ci\n",
		".github/workflows/sync.yml": "name: sync\n",
		"go.mod":                     "module example.com/control\n",
		"soapbox.yaml":               "version: 2\n",
		"tools/cmd/soapbox/main.go":  "package main\n",
		"tools/go.mod":               "module example.com/control/tools\n",
	} {
		destination.WriteFile(t, path, contents)
	}
	parent := destination.Commit(ctx, t, "setup control plane\n", gitcli.CommitOptions{},
		".github/workflows/ci.yml", ".github/workflows/sync.yml", "go.mod", "soapbox.yaml",
		"tools/cmd/soapbox/main.go", "tools/go.mod")

	result, err := enginesync.ReplayDAG(ctx, enginesync.DAGOptions{
		Config: e.opts.Config, SourceCache: cache, DestinationGit: destination.Git,
		Generate: e.opts, AnchorCommit: e.upstream.commit, EpochParent: parent,
		Release: source.Release{Source: resolved[0], DestinationTag: fixtureStagingTag},
	})
	if err != nil {
		t.Fatalf("replay DAG: %v", err)
	}
	if result.ReleaseGeneration == nil || result.ReleaseGeneration.Report.Source.Commit != e.upstream.commit {
		t.Fatalf("release generation = %#v, want source %s", result.ReleaseGeneration, e.upstream.commit)
	}
	if result.GeneratedCommits != 1 || result.PrefilteredCommits != 0 {
		t.Errorf("generated %d, prefiltered %d, want 1 and 0", result.GeneratedCommits, result.PrefilteredCommits)
	}
	if result.Replay == nil || len(result.Replay.Heads) != 1 || result.Replay.Heads[0].Destination == "" {
		t.Fatalf("replay result = %#v, want one destination head", result.Replay)
	}
}

func TestReplayDAGContinuesPublishedReleaseAndPrefiltersDocs(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	e.opts.Materialize = false

	e.upstream.repo.WriteFile(t, "docs/next.md", "not part of the generated closure\n")
	docs := e.upstream.repo.Commit(ctx, t, "docs: prepare next release\n", gitcli.CommitOptions{}, "docs/next.md")
	const rbacPath = "plugin/pkg/auth/authorizer/rbac/rbac.go"
	e.upstream.repo.WriteFile(t, rbacPath, upstreamRBAC+"\n// NextRelease keeps the fixture tree observably new.\n")
	next := e.upstream.repo.Commit(ctx, t, "feat: update the rbac authorizer\n", gitcli.CommitOptions{}, rbacPath)
	const nextSourceTag = "v1.36.2"
	const nextModuleTag = "v0.36.2"
	if err := e.upstream.repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name: nextSourceTag, Commit: next, Message: "Kubernetes " + nextSourceTag + "\n",
		Tagger: gitcli.Signature{Name: "Fixture Author", Email: "fixture@example.test", Date: "2026-02-02T03:04:05Z"},
	}); err != nil {
		t.Fatalf("tag next source release: %v", err)
	}
	for modulePath, files := range proxyModules {
		writeProxyModule(t, e.proxy, modulePath, nextModuleTag, stagingCommits[modulePath], files)
	}

	cache, err := source.Open(ctx, source.Options{
		Remote: e.upstream.url(), CacheRoot: e.roots.cache,
		WorktreeRoot: filepath.Join(e.roots.work, "continuation-source-worktrees"),
		Git:          e.opts.Git,
	})
	if err != nil {
		t.Fatalf("open source cache: %v", err)
	}
	if err := cache.Fetch(ctx, source.Refs{Tags: []string{fixtureTag, nextSourceTag}}); err != nil {
		t.Fatalf("fetch source releases: %v", err)
	}
	resolved, err := cache.Resolve(ctx, source.Refs{Tags: []string{fixtureTag, nextSourceTag}})
	if err != nil || len(resolved) != 2 {
		t.Fatalf("resolve source releases = %#v, %v", resolved, err)
	}
	byName := map[string]source.Revision{}
	for _, revision := range resolved {
		byName[revision.Name] = revision
	}

	destination := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Soapbox DAG Test", UserEmail: "dag@example.com",
	})
	for path, contents := range map[string]string{
		".github/workflows/ci.yml":   "name: ci\n",
		".github/workflows/sync.yml": "name: sync\n",
		"go.mod":                     "module example.com/control\n",
		"soapbox.yaml":               "version: 2\n",
		"tools/cmd/soapbox/main.go":  "package main\n",
		"tools/go.mod":               "module example.com/control/tools\n",
	} {
		destination.WriteFile(t, path, contents)
	}
	setupParent := destination.Commit(ctx, t, "setup control plane\n", gitcli.CommitOptions{},
		".github/workflows/ci.yml", ".github/workflows/sync.yml", "go.mod", "soapbox.yaml",
		"tools/cmd/soapbox/main.go", "tools/go.mod")

	first, err := enginesync.ReplayDAG(ctx, enginesync.DAGOptions{
		Config: e.opts.Config, SourceCache: cache, DestinationGit: destination.Git,
		Generate: e.opts, AnchorCommit: e.upstream.commit, EpochParent: setupParent,
		Release: source.Release{Source: byName[fixtureTag], DestinationTag: fixtureStagingTag},
	})
	if err != nil {
		t.Fatalf("project first release: %v", err)
	}
	firstHead := first.Replay.Heads[0].Destination

	second, err := enginesync.ReplayDAG(ctx, enginesync.DAGOptions{
		Config: e.opts.Config, SourceCache: cache, DestinationGit: destination.Git,
		Generate: e.opts, AnchorCommit: e.upstream.commit, AnchorTag: fixtureTag,
		MappedAnchor: true, EpochParent: firstHead,
		Release: source.Release{Source: byName[nextSourceTag], DestinationTag: nextModuleTag},
	})
	if err != nil {
		t.Fatalf("project next release: %v", err)
	}
	if second.GeneratedCommits != 1 || second.PrefilteredCommits != 2 {
		t.Errorf("generated %d, prefiltered %d, want 1 and 2", second.GeneratedCommits, second.PrefilteredCommits)
	}
	records := map[string]replay.Record{}
	for _, record := range second.Replay.Records {
		records[record.Source] = record
	}
	if !records[e.upstream.commit].Collapsed || !records[docs].Collapsed {
		t.Errorf("published anchor/docs did not collapse: %#v %#v", records[e.upstream.commit], records[docs])
	}
	if records[next].Destination == "" || records[next].Collapsed {
		t.Fatalf("next release record = %#v, want a written destination commit", records[next])
	}
	if second.ReleaseGeneration.Report.Source.RefName != nextSourceTag || second.ReleaseGeneration.Report.Source.ReleaseTag != nextModuleTag {
		t.Errorf("release generation = source %q destination %q, want %q and %q",
			second.ReleaseGeneration.Report.Source.RefName, second.ReleaseGeneration.Report.Source.ReleaseTag,
			nextSourceTag, nextModuleTag)
	}
}

func TestPlanChunkCheckpointsResumesAndGraftsEpoch(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Determinism.ChunkSize = 1
	})
	e.opts.Materialize = false

	e.upstream.repo.WriteFile(t, "docs/chunk-one.md", "irrelevant first chunk input\n")
	docs := e.upstream.repo.Commit(ctx, t, "docs: add chunk note\n", gitcli.CommitOptions{}, "docs/chunk-one.md")
	const rbacPath = "plugin/pkg/auth/authorizer/rbac/rbac.go"
	e.upstream.repo.WriteFile(t, rbacPath, upstreamRBAC+"\n// ChunkRelevant changes watched source.\n")
	relevant := e.upstream.repo.Commit(ctx, t, "feat: change watched rbac source\n", gitcli.CommitOptions{}, rbacPath)
	e.upstream.repo.WriteFile(t, "docs/chunk-two.md", "release boundary\n")
	final := e.upstream.repo.Commit(ctx, t, "docs: cut next release\n", gitcli.CommitOptions{}, "docs/chunk-two.md")
	const nextSourceTag = "v1.36.2"
	const nextModuleTag = "v0.36.2"
	if err := e.upstream.repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name: nextSourceTag, Commit: final, Message: "Kubernetes " + nextSourceTag + "\n",
		Tagger: gitcli.Signature{Name: "Fixture Author", Email: "fixture@example.test", Date: "2026-02-02T03:04:05Z"},
	}); err != nil {
		t.Fatalf("tag next source release: %v", err)
	}
	for modulePath, files := range proxyModules {
		writeProxyModule(t, e.proxy, modulePath, nextModuleTag, stagingCommits[modulePath], files)
	}

	cache, err := source.Open(ctx, source.Options{
		Remote: e.upstream.url(), CacheRoot: e.roots.cache,
		WorktreeRoot: filepath.Join(e.roots.work, "chunk-source-worktrees"),
		Git:          e.opts.Git,
	})
	if err != nil {
		t.Fatalf("open source cache: %v", err)
	}
	if err := cache.Fetch(ctx, source.Refs{Tags: []string{fixtureTag, nextSourceTag}}); err != nil {
		t.Fatalf("fetch source releases: %v", err)
	}
	resolved, err := cache.Resolve(ctx, source.Refs{Tags: []string{fixtureTag, nextSourceTag}})
	if err != nil || len(resolved) != 2 {
		t.Fatalf("resolve source releases = %#v, %v", resolved, err)
	}
	byName := map[string]source.Revision{}
	for _, revision := range resolved {
		byName[revision.Name] = revision
	}

	destination := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Soapbox Chunk Test", UserEmail: "chunk@example.com",
	})
	for path, contents := range map[string]string{
		".github/workflows/ci.yml":   "name: ci\n",
		".github/workflows/sync.yml": "name: sync\n",
		"go.mod":                     "module example.com/control\n",
		"soapbox.yaml":               "version: 2\n",
		"tools/cmd/soapbox/main.go":  "package main\n",
		"tools/go.mod":               "module example.com/control/tools\n",
	} {
		destination.WriteFile(t, path, contents)
	}
	setupParent := destination.Commit(ctx, t, "setup control plane\n", gitcli.CommitOptions{},
		".github/workflows/ci.yml", ".github/workflows/sync.yml", "go.mod", "soapbox.yaml",
		"tools/cmd/soapbox/main.go", "tools/go.mod")
	first, err := enginesync.ReplayDAG(ctx, enginesync.DAGOptions{
		Config: e.opts.Config, SourceCache: cache, DestinationGit: destination.Git,
		Generate: e.opts, AnchorCommit: e.upstream.commit, EpochParent: setupParent,
		Release: source.Release{Source: byName[fixtureTag], DestinationTag: fixtureStagingTag},
	})
	if err != nil {
		t.Fatalf("project initial release: %v", err)
	}
	consumerHead := first.Replay.Heads[0].Destination
	consumerTree, err := destination.Git.ResolveTree(ctx, consumerHead)
	if err != nil {
		t.Fatalf("resolve initial consumer tree: %v", err)
	}
	initialTagInfo, err := cache.Git().TagInfo(ctx, fixtureTag)
	if err != nil {
		t.Fatalf("read initial source tag: %v", err)
	}
	initialRelease, err := enginerelease.Project(ctx, destination.Git, enginerelease.Options{
		Policy: e.opts.Config.Release.Policy,
		Source: enginerelease.Source{
			Tag: fixtureTag, Commit: e.upstream.commit, Tagger: initialTagInfo.Tagger,
			URL: "https://github.com/kubernetes/kubernetes/releases/tag/" + fixtureTag,
		},
		Replay:     enginerelease.Replay{Commit: consumerHead, Tree: consumerTree},
		Projection: consumerTree,
		Bot: enginerelease.Identity{
			Name: e.opts.Config.Commit.Committer.Name, Email: e.opts.Config.Commit.Committer.Email,
		},
	})
	if err != nil {
		t.Fatalf("project initial release tag: %v", err)
	}
	if err := destination.Git.UpdateRef(ctx, "refs/heads/main", consumerHead, setupParent); err != nil {
		t.Fatalf("advance local consumer branch: %v", err)
	}

	format, err := destination.Git.ObjectFormat(ctx)
	if err != nil {
		t.Fatalf("destination object format: %v", err)
	}
	prior, err := state.New(state.Document{
		Schema: state.Schema, ObjectFormat: format,
		Destination: state.Destination{
			Repository: e.opts.Config.Destination.Repository,
			Module:     e.opts.Config.Destination.Module,
		},
		Anchor: state.Anchor{Source: e.upstream.commit, Ref: "refs/tags/" + fixtureTag},
		Epoch: state.Epoch{
			Profile: "sha256:" + strings.Repeat("a", 64),
			Source:  e.upstream.commit, Destination: setupParent,
		},
		Cursors: []state.Cursor{{Ref: "refs/tags/" + fixtureTag, Source: e.upstream.commit, Destination: consumerHead}},
		Published: []state.Published{
			{Ref: "refs/heads/main", Kind: state.KindBranch, Source: e.upstream.commit, Object: consumerHead},
			{Ref: "refs/tags/" + fixtureStagingTag, Kind: state.KindTag, Source: e.upstream.commit, Object: initialRelease.Object},
		},
		Engine: state.Engine{Version: "old-engine", Toolchain: e.opts.Config.Determinism.Toolchain},
	})
	if err != nil {
		t.Fatalf("build prior state: %v", err)
	}
	stateSignature := gitcli.Signature{
		Name: e.opts.Config.Commit.Committer.Name, Email: e.opts.Config.Commit.Committer.Email,
		Date: "1700000000 +0000",
	}
	priorRecord, err := state.Store(ctx, destination.Git, state.StoreOptions{
		Document: prior, Author: stateSignature, Committer: stateSignature,
	})
	if err != nil {
		t.Fatalf("store prior state: %v", err)
	}
	if err := destination.Git.CreateRef(ctx, e.opts.Config.Destination.StateRef, priorRecord.Commit); err != nil {
		t.Fatalf("create local state ref: %v", err)
	}

	remote := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Soapbox Remote", UserEmail: "remote@example.com",
	})
	if err := remote.Git.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make remote bare: %v", err)
	}
	remotePath := filepath.Join(remote.Dir, ".git")
	if err := destination.Git.PushAtomic(ctx, remotePath, []gitcli.PushUpdate{
		{Ref: "refs/heads/main", New: consumerHead, ExpectAbsent: true},
		{Ref: "refs/tags/" + fixtureStagingTag, New: initialRelease.Object, ExpectAbsent: true},
		{Ref: e.opts.Config.Destination.StateRef, New: priorRecord.Commit, ExpectAbsent: true},
	}); err != nil {
		t.Fatalf("seed destination remote: %v", err)
	}

	e.opts.StagingSources = addIntermediateStagingFixtures(ctx, t, e, nextModuleTag)
	e.opts.Config.Source.Refs.AnchorCommit = e.upstream.commit
	release := source.Release{Source: byName[nextSourceTag], DestinationTag: nextModuleTag}

	autoRemote := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Soapbox Automatic Remote", UserEmail: "automatic@example.com",
	})
	if err := autoRemote.Git.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make automatic remote bare: %v", err)
	}
	autoRemotePath := filepath.Join(autoRemote.Dir, ".git")
	if err := destination.Git.PushAtomic(ctx, autoRemotePath, []gitcli.PushUpdate{
		{Ref: "refs/heads/main", New: consumerHead, ExpectAbsent: true},
		{Ref: "refs/tags/" + fixtureStagingTag, New: initialRelease.Object, ExpectAbsent: true},
		{Ref: e.opts.Config.Destination.StateRef, New: priorRecord.Commit, ExpectAbsent: true},
	}); err != nil {
		t.Fatalf("seed automatic remote: %v", err)
	}
	automaticDestination := enginesync.Destination{
		Git: destination.Git, Remote: autoRemotePath,
		Identity:         "github.com/" + e.opts.Config.Destination.Repository,
		AllowLocalRemote: true,
	}
	automatic, err := enginesync.Reconcile(ctx, enginesync.ReconcileOptions{
		Config: e.opts.Config, SourceCache: cache,
		LocalGit:    destination.Git.Anonymous().WithNoLazyFetch(),
		Destination: automaticDestination, Generate: e.opts, Automatic: true,
	})
	if err != nil {
		t.Fatalf("automatic reconciliation: %v", err)
	}
	if !automatic.FixedPoint || len(automatic.Actions) < 2 || automatic.Actions[len(automatic.Actions)-1].Kind != "release" {
		t.Fatalf("automatic reconciliation = %#v, want checkpoints then a release fixed point", automatic)
	}
	for _, action := range automatic.Actions {
		if !action.Applied {
			t.Errorf("automatic action was not applied: %#v", action)
		}
	}
	autoRefs := remoteRefMap(ctx, t, destination.Git, autoRemotePath, format.HexLength())
	if autoRefs["refs/heads/main"] == consumerHead || autoRefs["refs/tags/"+nextModuleTag] == "" {
		t.Errorf("automatic consumer refs = %#v, want advanced branch and new tag", autoRefs)
	}

	planRemote := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Soapbox Plan Remote", UserEmail: "plan@example.com",
	})
	if err := planRemote.Git.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make plan remote bare: %v", err)
	}
	planRemotePath := filepath.Join(planRemote.Dir, ".git")
	if err := destination.Git.PushAtomic(ctx, planRemotePath, []gitcli.PushUpdate{
		{Ref: "refs/heads/main", New: consumerHead, ExpectAbsent: true},
		{Ref: "refs/tags/" + fixtureStagingTag, New: initialRelease.Object, ExpectAbsent: true},
		{Ref: e.opts.Config.Destination.StateRef, New: priorRecord.Commit, ExpectAbsent: true},
	}); err != nil {
		t.Fatalf("seed plan remote: %v", err)
	}
	planDestination := automaticDestination
	planDestination.Remote = planRemotePath
	planned, err := enginesync.Reconcile(ctx, enginesync.ReconcileOptions{
		Config: e.opts.Config, SourceCache: cache,
		LocalGit:    destination.Git.Anonymous().WithNoLazyFetch(),
		Destination: planDestination, Generate: e.opts,
	})
	if err != nil {
		t.Fatalf("plan-only reconciliation: %v", err)
	}
	if len(planned.Actions) != 1 || planned.Actions[0].Kind != "checkpoint" || planned.Actions[0].Applied {
		t.Fatalf("plan-only reconciliation = %#v, want one unapplied checkpoint", planned)
	}
	planRefs := remoteRefMap(ctx, t, destination.Git, planRemotePath, format.HexLength())
	if planRefs["refs/heads/main"] != consumerHead || planRefs[e.opts.Config.Destination.StateRef] != priorRecord.Commit || planRefs[state.ProgressNamespace+nextModuleTag] != "" {
		t.Errorf("plan-only reconciliation moved remote refs: %#v", planRefs)
	}
	manuallyApplied, err := enginesync.Reconcile(ctx, enginesync.ReconcileOptions{
		Config: e.opts.Config, SourceCache: cache,
		LocalGit:    destination.Git.Anonymous().WithNoLazyFetch(),
		Destination: planDestination, Generate: e.opts,
		Apply: true, Approval: planned.Actions[0].PlanHash,
	})
	if err != nil {
		t.Fatalf("manual approved reconciliation: %v", err)
	}
	if len(manuallyApplied.Actions) != 1 || !manuallyApplied.Actions[0].Applied {
		t.Fatalf("manual approved reconciliation = %#v, want one applied checkpoint", manuallyApplied)
	}
	planRefs = remoteRefMap(ctx, t, destination.Git, planRemotePath, format.HexLength())
	if planRefs["refs/heads/main"] != consumerHead || planRefs[e.opts.Config.Destination.StateRef] == priorRecord.Commit || planRefs[state.ProgressNamespace+nextModuleTag] == "" {
		t.Errorf("manual approved checkpoint refs = %#v", planRefs)
	}

	destinationConfig := enginesync.Destination{
		Git: destination.Git, Remote: remotePath,
		Identity: "local/chunk-test", AllowLocalRemote: true,
	}
	discovery := &enginesync.Discovery{
		Format: format,
		Observed: map[string]string{
			"refs/heads/main":                  consumerHead,
			e.opts.Config.Destination.StateRef: priorRecord.Commit,
		},
		StateCommit: priorRecord.Commit, State: prior,
		Pending: []enginesync.PendingRelease{{Source: release, DestinationTag: nextModuleTag}},
	}
	firstChunk, err := enginesync.PlanChunk(ctx, enginesync.ChunkOptions{
		Config: e.opts.Config, Discovery: discovery, SourceCache: cache,
		Destination: destinationConfig, Generate: e.opts, Release: release,
	})
	if err != nil {
		t.Fatalf("plan first chunk: %v", err)
	}
	if firstChunk.Complete {
		t.Fatal("first chunk unexpectedly completed the release")
	}
	if actions := firstChunk.Publish.Actions(publish.ScopeConsumer); len(actions) != 0 {
		t.Fatalf("checkpoint plan contains consumer actions: %#v", actions)
	}
	if firstChunk.Track.Source != relevant || firstChunk.Track.Done != 2 || firstChunk.Track.Total != 3 {
		t.Errorf("first track = %#v, want source %s at 2/3", firstChunk.Track, relevant)
	}
	if firstChunk.Document.Epoch.Source != docs || firstChunk.Document.Epoch.Destination != consumerHead {
		t.Errorf("grafted epoch = %#v, want source %s on %s", firstChunk.Document.Epoch, docs, consumerHead)
	}
	if firstChunk.Document.Epoch.Profile == prior.Epoch.Profile {
		t.Error("profile change did not start a new epoch")
	}
	repeated, err := enginesync.PlanChunk(ctx, enginesync.ChunkOptions{
		Config: e.opts.Config, Discovery: discovery, SourceCache: cache,
		Destination: destinationConfig, Generate: e.opts, Release: release,
	})
	if err != nil {
		t.Fatalf("repeat first chunk plan: %v", err)
	}
	if repeated.State.Commit != firstChunk.State.Commit || repeated.Publish.Hash() != firstChunk.Publish.Hash() || repeated.Track != firstChunk.Track {
		t.Fatalf("repeated chunk differs:\n first state=%s plan=%s track=%#v\n again state=%s plan=%s track=%#v",
			firstChunk.State.Commit, firstChunk.Publish.Hash(), firstChunk.Track,
			repeated.State.Commit, repeated.Publish.Hash(), repeated.Track)
	}
	applied, err := enginesync.ApplyCheckpoint(ctx, firstChunk, firstChunk.Publish.Hash(), false)
	if err != nil {
		t.Fatalf("apply first checkpoint: %v", err)
	}
	if !slices.Contains(applied.Pushed, firstChunk.Track.Ref) || !slices.Contains(applied.Pushed, e.opts.Config.Destination.StateRef) {
		t.Errorf("first checkpoint pushed %v, want progress and state", applied.Pushed)
	}

	observed := remoteRefMap(ctx, t, destination.Git, remotePath, format.HexLength())
	if observed["refs/heads/main"] != consumerHead {
		t.Fatalf("consumer branch moved during checkpoint: %s, want %s", observed["refs/heads/main"], consumerHead)
	}
	secondDiscovery := &enginesync.Discovery{
		Format: format, Observed: observed,
		StateCommit: firstChunk.State.Commit, State: firstChunk.Document,
		Pending: []enginesync.PendingRelease{{Source: release, DestinationTag: nextModuleTag}},
	}
	secondChunk, err := enginesync.PlanChunk(ctx, enginesync.ChunkOptions{
		Config: e.opts.Config, Discovery: secondDiscovery, SourceCache: cache,
		Destination: destinationConfig, Generate: e.opts, Release: release,
	})
	if err != nil {
		t.Fatalf("plan resumed chunk: %v", err)
	}
	if !secondChunk.Complete || secondChunk.Track.Source != final || secondChunk.Track.Done != 3 {
		t.Fatalf("resumed track = %#v complete=%v, want final source at 3/3", secondChunk.Track, secondChunk.Complete)
	}
	if secondChunk.Document.Mapping.Entries <= firstChunk.Document.Mapping.Entries {
		t.Errorf("mapping entries did not grow: %d then %d", firstChunk.Document.Mapping.Entries, secondChunk.Document.Mapping.Entries)
	}
	if _, _, err := state.LoadMapping(ctx, destination.Git, secondChunk.State.Commit); err != nil {
		t.Fatalf("completed state has no reachable mapping evidence: %v", err)
	}
	_, err = enginesync.ApplyCheckpoint(ctx, secondChunk, secondChunk.Publish.Hash(), false)
	if err != nil {
		t.Fatalf("apply resumed checkpoint: %v", err)
	}
	observed = remoteRefMap(ctx, t, destination.Git, remotePath, format.HexLength())
	if observed["refs/heads/main"] != consumerHead {
		t.Fatalf("consumer branch moved after completed checkpoint: %s, want %s", observed["refs/heads/main"], consumerHead)
	}
	finalDiscovery := &enginesync.Discovery{
		Format: format, Observed: observed,
		StateCommit: secondChunk.State.Commit, State: secondChunk.Document,
		Pending: []enginesync.PendingRelease{{Source: release, DestinationTag: nextModuleTag}},
	}
	e.opts.Config.Publication.Mode = config.PublicationModeManual
	manualPlan, err := enginesync.PlanFinal(ctx, enginesync.FinalizeOptions{
		Config: e.opts.Config, Discovery: finalDiscovery, SourceCache: cache,
		Destination: destinationConfig, Generate: e.opts, Release: release,
	})
	if err != nil {
		t.Fatalf("plan manual final consumer publication: %v", err)
	}
	if _, err := enginesync.ApplyTrusted(ctx, manualPlan); err == nil || !strings.Contains(err.Error(), "publication mode") {
		t.Fatalf("trusted apply in manual mode = %v, want publication-mode refusal", err)
	}
	e.opts.Config.Publication.Mode = config.PublicationModeAutomatic
	finalPlan, err := enginesync.PlanFinal(ctx, enginesync.FinalizeOptions{
		Config: e.opts.Config, Discovery: finalDiscovery, SourceCache: cache,
		Destination: destinationConfig, Generate: e.opts, Release: release,
	})
	if err != nil {
		t.Fatalf("plan final consumer publication: %v", err)
	}
	if finalPlan.Manifest.Hash == "" {
		t.Fatal("final consumer plan has no synchronization hash")
	}
	trusted, err := enginesync.ApplyTrusted(ctx, finalPlan)
	if err != nil {
		t.Fatalf("apply trusted finalization: %v", err)
	}
	if trusted.Publication == nil || trusted.Reconciliation == nil || trusted.State.Commit == "" {
		t.Fatalf("trusted apply result = %#v, want publication and reconciliation", trusted)
	}
	observed = remoteRefMap(ctx, t, destination.Git, remotePath, format.HexLength())
	if observed["refs/heads/main"] != secondChunk.Track.Destination {
		t.Errorf("final consumer branch = %s, want %s", observed["refs/heads/main"], secondChunk.Track.Destination)
	}
	if observed["refs/tags/"+nextModuleTag] != finalPlan.Manifest.Objects.Tag {
		t.Errorf("final release tag = %s, want %s", observed["refs/tags/"+nextModuleTag], finalPlan.Manifest.Objects.Tag)
	}
	if observed[e.opts.Config.Destination.StateRef] != trusted.State.Commit {
		t.Errorf("reconciled state = %s, want %s", observed[e.opts.Config.Destination.StateRef], trusted.State.Commit)
	}
	reconciled, err := state.Load(ctx, destination.Git, trusted.State.Commit)
	if err != nil {
		t.Fatalf("load reconciled state: %v", err)
	}
	if len(reconciled.Tracks) != 0 {
		t.Errorf("reconciled state retains completed tracks: %#v", reconciled.Tracks)
	}
	published := map[string]state.Published{}
	for _, entry := range reconciled.Published {
		published[entry.Ref] = entry
	}
	if published["refs/heads/main"].Object != secondChunk.Track.Destination ||
		published["refs/tags/"+nextModuleTag].Object != finalPlan.Manifest.Objects.Tag {
		t.Errorf("reconciled published refs = %#v", published)
	}
	controlFiles, err := destination.Git.ListTree(ctx, secondChunk.Track.Destination)
	if err != nil {
		t.Fatalf("read generated consumer tree: %v", err)
	}
	controlBlob, err := destination.Git.WriteBlob(ctx, []byte("name: upgraded sync\n"))
	if err != nil {
		t.Fatalf("write control-plane blob: %v", err)
	}
	controlChanged := false
	for i := range controlFiles {
		if controlFiles[i].Path == ".github/workflows/sync.yml" {
			controlFiles[i].Object = controlBlob
			controlChanged = true
			break
		}
	}
	if !controlChanged {
		t.Fatal("generated consumer tree has no sync workflow")
	}
	controlTree, err := destination.Git.WriteTree(ctx, controlFiles)
	if err != nil {
		t.Fatalf("write control-plane tree: %v", err)
	}
	controlHead, err := destination.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: controlTree, Parents: []string{secondChunk.Track.Destination},
		Author: stateSignature, Committer: stateSignature,
		Message: "chore: update control plane\n",
	})
	if err != nil {
		t.Fatalf("write control-plane commit: %v", err)
	}
	if err := destination.Git.PushAtomic(ctx, remotePath, []gitcli.PushUpdate{{
		Ref: "refs/heads/main", New: controlHead, ExpectedOld: secondChunk.Track.Destination,
	}}); err != nil {
		t.Fatalf("publish control-plane fast-forward: %v", err)
	}
	if err := destination.Git.UpdateRef(ctx, "refs/heads/main", controlHead, consumerHead); err != nil {
		t.Fatalf("advance local branch to control-plane head: %v", err)
	}
	if err := destination.Git.UpdateRef(ctx, e.opts.Config.Destination.StateRef, trusted.State.Commit, priorRecord.Commit); err != nil {
		t.Fatalf("advance local state ref: %v", err)
	}
	e.opts.Config.Source.Refs.AnchorCommit = e.upstream.commit
	reconcileDestination := destinationConfig
	reconcileDestination.Identity = "github.com/" + e.opts.Config.Destination.Repository
	beforeFixed := remoteRefMap(ctx, t, destination.Git, remotePath, format.HexLength())
	fixed, err := enginesync.Reconcile(ctx, enginesync.ReconcileOptions{
		Config: e.opts.Config, SourceCache: cache,
		LocalGit:    destination.Git.Anonymous().WithNoLazyFetch(),
		Destination: reconcileDestination, Generate: e.opts, Automatic: true,
	})
	if err != nil {
		t.Fatalf("reconcile fixed point: %v", err)
	}
	if !fixed.FixedPoint || !fixed.WriteVerified || len(fixed.Actions) != 0 {
		t.Errorf("fixed reconciliation = %#v, want no actions and verified write access", fixed)
	}
	afterFixed := remoteRefMap(ctx, t, destination.Git, remotePath, format.HexLength())
	if !maps.Equal(beforeFixed, afterFixed) {
		t.Errorf("fixed-point write verification moved refs: before %#v, after %#v", beforeFixed, afterFixed)
	}
}

func remoteRefMap(ctx context.Context, t *testing.T, git *gitcli.Runner, remote string, width int) map[string]string {
	t.Helper()
	refs, err := git.RemoteRefs(ctx, remote, width)
	if err != nil {
		t.Fatalf("list remote refs: %v", err)
	}
	result := make(map[string]string, len(refs))
	for _, ref := range refs {
		result[ref.Name] = ref.Target
	}
	return result
}

// TestGenerateProvesPruningKeptThePublicAPI covers the gate the whole pre-prune
// pass exists to make possible.
func TestGenerateProvesPruningKeptThePublicAPI(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	facade := result.Report.Facade
	if facade.PreManifestHash != facade.PostManifestHash {
		t.Errorf("facade manifests differ across the prune: pre = %s, post = %s", facade.PreManifestHash, facade.PostManifestHash)
	}
	if len(facade.Differences) != 0 {
		t.Errorf("facade differences = %v, want none", facade.Differences)
	}

	// The published names are the profile's, and the README states the same
	// ones, so the two cannot drift apart.
	want := []string{
		"AuthorizationRuleResolver",
		"DefaultRuleResolver",
		"New",
		"NewDefaultRuleResolver",
		"RBACAuthorizer",
		"RoleGetter",
	}
	if !slices.Equal(facade.Entries, want) {
		t.Errorf("facade entries = %v, want %v", facade.Entries, want)
	}
	if !slices.Equal(result.Report.Provenance.PublicAPI, want) {
		t.Errorf("README public API = %v, want %v", result.Report.Provenance.PublicAPI, want)
	}
}

// TestGenerateReportsHonestZeroCandidates proves the dependency phase records a
// decision rather than skipping.
//
// An empty candidate set and a phase that never ran encode identically unless
// the decision itself is written down, and the difference matters: one says this
// profile copies nothing, the other says nobody asked.
func TestGenerateReportsHonestZeroCandidates(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	deps := result.Report.Dependencies
	if deps.Policy != "external" {
		t.Errorf("dependency policy = %q, want external", deps.Policy)
	}
	if len(deps.Copy) != 0 {
		t.Errorf("dependency copy = %v, want none", deps.Copy)
	}
	if deps.Totals.Candidates != 0 || deps.Totals.Copied != 0 {
		t.Errorf("dependency totals = %+v, want zero candidates and zero copies", deps.Totals)
	}
	if deps.Candidates == nil {
		t.Error("dependency candidates = nil, want an empty list so the encoding is stable")
	}
}

// TestGenerateCrossChecksProvenance proves the root evidence accounts for the
// tree it describes.
func TestGenerateCrossChecksProvenance(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	prov := result.Report.Provenance
	if prov.LicenseID != "Apache-2.0" {
		t.Errorf("licence = %q, want Apache-2.0", prov.LicenseID)
	}
	if !prov.UpstreamNotice {
		t.Error("report: the upstream NOTICE was not embedded, but the fixture commit has one")
	}

	// The licence travels byte for byte, and the NOTICE quotes the upstream one
	// rather than merging with it.
	if got := fileContents(t, result, "LICENSE"); got != fixtureLicense {
		t.Errorf("generated LICENSE is not the upstream text:\n%s", got)
	}
	notice := fileContents(t, result, "NOTICE")
	if !strings.Contains(notice, "The Kubernetes Authors") {
		t.Errorf("generated NOTICE does not embed the upstream one:\n%s", notice)
	}
	// Every relocated package carries its own record beside it, and the root
	// NOTICE is what ties them together.
	for _, path := range treePaths(result) {
		if strings.HasSuffix(path, "/SOAPBOX_PROVENANCE.txt") {
			return
		}
	}
	t.Error("generated tree carries no per-package provenance record")
}

// TestGenerateIsDeterministicAcrossRoots is the property the report exists for.
//
// Two runs over one source commit with different directory layouts have to
// produce the same module and the same report. It is what makes a report
// comparable in CI and what makes an unexpected difference a real signal rather
// than noise from the machine that produced it.
func TestGenerateIsDeterministicAcrossRoots(t *testing.T) {
	ctx := t.Context()
	first := newEndToEnd(ctx, t, nil)
	firstResult := first.generateOnce(ctx, t)

	// The second run reads the same upstream commit through an entirely
	// different set of directories.
	second := first.relayout(ctx, t)
	secondResult := second.generateOnce(ctx, t)

	if first.roots.output == second.roots.output || first.roots.cache == second.roots.cache {
		t.Fatal("fixture: both runs used the same directories, so this proves nothing")
	}
	if firstResult.Report.Source.Commit != secondResult.Report.Source.Commit {
		t.Fatalf("fixture: the two runs read different commits, %s and %s",
			firstResult.Report.Source.Commit, secondResult.Report.Source.Commit)
	}

	if firstResult.Report.Output.ManifestHash != secondResult.Report.Output.ManifestHash {
		t.Errorf("manifest hashes differ across layouts: %s and %s",
			firstResult.Report.Output.ManifestHash, secondResult.Report.Output.ManifestHash)
	}
	if !slices.Equal(treePaths(firstResult), treePaths(secondResult)) {
		t.Errorf("trees differ across layouts:\n  %s\nand\n  %s",
			joinLines(treePaths(firstResult)), joinLines(treePaths(secondResult)))
	}

	// The reports have to agree byte for byte, which is the property that makes
	// a difference between two runs a real signal rather than noise from the
	// machine that produced it.
	firstJSON, err := firstResult.Report.JSON()
	if err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	secondJSON, err := secondResult.Report.JSON()
	if err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("reports differ across layouts:\n%s\nand\n%s", firstJSON, secondJSON)
	}

	// The report itself must carry no absolute path from either machine.
	for _, result := range []*generate.Result{firstResult, secondResult} {
		data, err := result.Report.JSON()
		if err != nil {
			t.Fatalf("report JSON: %v", err)
		}
		for _, dir := range []string{
			result.Paths.Cache, result.Paths.Work, result.Paths.Output,
			result.Paths.Store, result.Paths.PreModule, result.Paths.PostModule,
		} {
			if dir != "" && strings.Contains(string(data), dir) {
				t.Errorf("report names the absolute directory %s", dir)
			}
		}
		// The source remote override is a path on this machine, so only the
		// fact of an override may be recorded.
		if strings.Contains(string(data), first.upstream.repo.Dir) {
			t.Error("report names the source remote override")
		}
		if !result.Report.Source.RemoteOverridden {
			t.Error("report: RemoteOverridden = false, want the override recorded as a fact")
		}
	}
}

// TestGenerateLeavesNoOutputWhenAGateRefuses is the fail-closed property.
//
// A refused generation must leave nothing behind. A tree written before the last
// gate ran is a tree an operator can use, and the whole argument for gating is
// that an unacceptable module never becomes available.
func TestGenerateLeavesNoOutputWhenAGateRefuses(t *testing.T) {
	ctx := t.Context()
	// Removing a file the profile requires to survive pruning is a refusal the
	// extraction phase reaches, which is early enough that no later phase can be
	// what prevented the write.
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Prune.Required = append(cfg.Prune.Required, "pkg/registry/rbac/validation/missing.go")
	})

	result, err := generateFailure(ctx, t, e.opts)
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after a refusal: %v", statErr)
	}
	// The refusal is still reviewable from an artifact rather than from stderr.
	if result == nil {
		t.Fatalf("generate: got no result for a measured refusal: %v", err)
	}
	if result.Report.Failure == nil {
		t.Fatal("generate: the report records no failure")
	}
	if !result.Report.Failure.Policy {
		t.Errorf("failure = %+v, want it classified as a policy refusal", result.Report.Failure)
	}
	if result.Report.Output.Materialized {
		t.Error("report: Materialized = true after a refusal")
	}
}

// TestGenerateStrictRefusesNoticesBeforeWriting proves strict mode gates the
// output rather than annotating it.
func TestGenerateStrictRefusesNoticesBeforeWriting(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	e.opts.Strict = true

	result, err := generateFailure(ctx, t, e.opts)
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after a strict refusal: %v", statErr)
	}
	if result == nil {
		t.Fatalf("generate: got no result for a strict refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "closure golden") && !strings.Contains(err.Error(), "strict") {
		t.Errorf("generate: error = %v, want it to name the notice that refused", err)
	}
}

// treePaths renders the generated module's file paths, sorted.
func treePaths(result *generate.Result) []string {
	paths := make([]string, 0, len(result.Files.Files))
	for _, file := range result.Files.Files {
		paths = append(paths, file.Path)
	}
	slices.Sort(paths)
	return paths
}

// fileContents reads one generated file out of the composed set.
func fileContents(t *testing.T, result *generate.Result, path string) string {
	t.Helper()
	file, ok := result.Files.Lookup(path)
	if !ok {
		t.Fatalf("generated tree has no %s, it has:\n  %s", path, joinLines(treePaths(result)))
	}
	return string(file.Contents)
}

// walkTree lists the files actually written below root, module relative and
// sorted.
func walkTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, relativeTo(root, path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(paths)
	return paths
}
