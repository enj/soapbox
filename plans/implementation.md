# Soapbox implementation plan

## Context

`k8s.io/kubernetes` cannot be consumed as a normal library module because its staging dependencies use `v0.0.0` requirements and repository local `replace` directives. Soapbox will turn one configured Kubernetes package, or set of packages, into an independently consumable module while retaining useful upstream history and provenance.

The first live output is `github.com/enj/rbac_authorizer`, imported as `monis.app/kk/rbac_authorizer`. Its first public version will be `v0.36.1`, derived from Kubernetes `v1.36.1`. Later Kubernetes `v1.X.Y` tags, including prereleases, map to module `v0.X.Y` tags.

The approved goal and this implementation plan will remain in the repository under `plans/`. `goal.md` moves to `plans/goal.md`, and this approved plan is copied to `plans/implementation.md` as the durable execution reference. The approved follow-on plan for GITHUB_TOKEN publishing, unattended reconciliation, and module dependency modes is [`plans/github-token-unattended.md`](github-token-unattended.md).

The implementation has five hard constraints:

1. Maintained executable logic is Go. Workflow YAML may invoke a single Go command. Go may invoke installed `git` and `go` executables through typed `os/exec` wrappers. There will be no shell scripts or shell command composition.
2. Public `k8s.io/api` types replace `k8s.io/kubernetes/pkg/apis` types whenever equivalence and behavior can be proved.
3. Staging packages may be copied only when an objective safety and leverage analysis approves them. A large module by itself is not a reason to copy code.
4. Every human commit in `enj/soapbox` is SSH signed, has author and committer `Monis Khan <i@monis.app>`, and contains exactly `Signed-off-by: Monis Khan <mok@microsoft.com>`.
5. Published module tags are immutable. Initial publication happens only after a local dry run and a separate explicit approval of the exact repositories, vanity page diff, refs, tag objects, and commit OIDs.

Generated replay commits preserve the upstream author, author date, message, relevant graph relationships, and a `Kubernetes-commit: <sha>` trailer. The `github-actions[bot]` identity is the committer. Generated commits are unsigned.

## Architecture

### Template and engine

`enj/soapbox` will be both a GitHub template and the source of a versioned Go engine.

1. The template has no root `go.mod`. The engine is the nested module `github.com/enj/soapbox/tools`.
2. `tools/soapbox.go` exposes the small public entry point used by template derived repositories. All engine implementation remains under `tools/internal/`.
3. `soapbox setup` creates the generated repository's root module, replaces the copied engine with a small nested `tools` module and command shim, and pins that shim plus the engine's indirect graph roots to an immutable `tools/vX.Y.Z` engine release. The complete verified checksums let the shim run from a clean cache without editing module metadata. Tool dependencies never enter the generated library's module graph.
4. Setup uses an explicit payload allowlist. Repository planning files, `.claude/`, engine source, engine tests, and development documentation are removed from the derived repository before its first generated tag.
5. Generated library source, `soapbox.yaml`, patches, the nested tool shim, and workflows coexist on the default branch. Replay commits modify only generated paths. Configuration or engine changes form explicit profile epochs and never rewrite published history.

### Repository layout

```text
soapbox/
├── CLAUDE.md
├── README.md
├── LICENSE
├── NOTICE
├── soapbox.yaml
├── plans/
│   ├── goal.md
│   └── implementation.md
├── patches/
│   ├── index.yaml
│   └── README.md
├── docs/
│   ├── setup.md
│   ├── config-reference.md
│   ├── replay-model.md
│   ├── determinism.md
│   ├── provenance.md
│   ├── behavior-changes.md
│   ├── dependency-policy.md
│   ├── github-token.md
│   ├── vanity.md
│   ├── conflict-runbook.md
│   └── decisions/0001-no-staging-copy-rbac.md
├── .github/workflows/
│   ├── ci.yml
│   └── template-selftest.yml
├── .golangci.yml
└── tools/
    ├── go.mod
    ├── go.sum
    ├── soapbox.go
    ├── cmd/soapbox/main.go
    └── internal/
        ├── cli/
        ├── config/
        ├── gitcli/
        ├── gitgraph/
        ├── source/
        ├── closure/
        ├── patchset/
        ├── relocate/
        ├── rewrite/
        ├── gomodmap/
        ├── modgen/
        ├── deppolicy/
        ├── typeswap/
        ├── facade/
        ├── provenance/
        ├── treebuild/
        ├── replay/
        ├── publish/
        ├── state/
        ├── ghapi/
        ├── upgrade/
        ├── vanity/
        ├── verify/
        ├── report/
        └── testsupport/
```

After setup, `enj/rbac_authorizer` will contain:

```text
rbac_authorizer/
├── go.mod
├── go.sum
├── authorizer.go
├── zz_generated_assertions.go
├── doc.go
├── authorizer_test.go
├── LICENSE
├── NOTICE
├── README.md
├── internal/kk/<preserved upstream package paths>/
├── soapbox.yaml
├── patches/
├── tools/
│   ├── go.mod
│   ├── go.sum
│   └── cmd/soapbox/main.go
└── .github/workflows/
    ├── ci.yml
    └── sync.yml
```

`refs/heads/soapbox-state` stores resumable cursors and dependency mappings without entering the module tree. Temporary `refs/soapbox/progress/<track>` refs hold gated backfill chunks. Consumer branches and tags advance only after all checks pass.

## Configuration contract

`soapbox.yaml` uses versioned, strictly decoded YAML. Unknown fields fail. The RBAC profile specifies:

1. Source repository `https://github.com/kubernetes/kubernetes.git` and source import prefix `k8s.io/kubernetes`.
2. First public source release `v1.36.1`, all later semantic tags including prereleases, `master`, and active release branches.
3. Package root `plugin/pkg/auth/authorizer/rbac` with package granularity, not recursive directory copying. This excludes sibling package `bootstrappolicy`.
4. Exact required prune files, required retained files, exact denied imports, non test production closure, limits, and a measured closure golden.
5. Type policy `prefer-external`, including the verified pairing between internal RBAC API types and `k8s.io/api/rbac/v1`.
6. Dependency policy `external` by default, an empty staging copy list for RBAC, non-overridable correctness gates, and expiring cost overrides.
7. Destination module `monis.app/kk/rbac_authorizer`, internal prefix `internal/kk`, repository `enj/rbac_authorizer`, and public root package `rbacauthorizer`.
8. Ordered patch files with ancestry based `since` and `until` selectors plus branch selectors.
9. Explicit facade exports, aliases, interface implementation assertions, staging dependency mapping, bot identity, release mapping, deterministic Go toolchain, publication mode, compatibility mode, and vanity location `enj/enj.github.io:kk/rbac_authorizer/index.html`.

Every configured path is relative, traversal free, and checked after symlink resolution to remain inside its permitted root. Source and destination URLs are host allowlisted. Config validation fails before network writes.

## Generation and replay pipeline

### 1. Source acquisition and immutable anchor

`tools/internal/source` anonymously maintains a bare, blobless partial clone of Kubernetes using `git clone --filter=blob:none --bare`. It fetches source heads and annotated tags without credentials.

On first setup, `tools/internal/gitgraph` computes a common merge base for all initially tracked refs and verifies that it is an ancestor of `v1.36.1`. That source commit becomes the immutable, untagged transformed anchor. Future tracked branches must descend from it. The resolved anchor SHA is written to config and state so discovery cannot silently change published history.

The engine traverses full source DAG metadata from the anchor to selected refs in topological order. It does not use a correctness critical path filtered `rev-list`. Cheap `diff-tree` checks decide which commits require materialization. When a current closure file begins importing a new package, that commit is materialized and captures the new package's complete state even though its older history was previously irrelevant.

### 2. Package closure, exact pruning, and assets

For each candidate source commit:

1. Materialize configured root packages in a sparse worktree.
2. Apply exact required prune entries before parsing imports. Prune paths are files, never globs or directories. An absent path fails closed, as does a prune outside the materialized closure or removal of a root package's final file.
3. Parse every direct, non test `.go` file regardless of platform build constraint. Follow imports beginning with `k8s.io/kubernetes/` until reaching a fixed point.
4. Materialize all direct package files required for portable builds, including Go, cgo, assembly, headers, syso files, and files matched by `go:embed`. Configured asset globs cover runtime data not discoverable statically. Subpackages are included only when imported or explicitly configured.
5. Materialize patch targets, apply the selected patch series, reassert pruning, and recompute closure. Repeat until fixed.
6. Fail if an exact denied import reenters the closure, if a patch targets a pruned file, or if any closure or growth limit is exceeded.
7. Type check the post-prune source before relocation.

For RBAC `v1.36.1`, the pre-prune internal closure is four packages and 3,289 non test lines:

```text
plugin/pkg/auth/authorizer/rbac
pkg/registry/rbac/validation
pkg/apis/rbac
pkg/apis/rbac/v1
```

The RBAC profile prunes these eight exact files:

```text
pkg/registry/rbac/validation/internal_version_adapter.go
pkg/apis/rbac/v1/zz_generated.conversion.go
pkg/apis/rbac/v1/register.go
pkg/apis/rbac/v1/defaults.go
pkg/apis/rbac/v1/zz_generated.defaults.go
pkg/apis/rbac/v1/helpers.go
pkg/apis/rbac/v1/zz_generated.validations.go
pkg/apis/rbac/v1/zz_generated.deepcopy.go
```

The resulting tree is three packages and about 978 lines:

```text
plugin/pkg/auth/authorizer/rbac
pkg/registry/rbac/validation
pkg/apis/rbac/v1  with only doc.go and evaluation_helpers.go
```

`pkg/apis/rbac` disappears. The deny rule matches the exact unversioned import `k8s.io/kubernetes/pkg/apis/rbac`, not its retained `/v1` helper subpackage.

The retained authorizer and validation code already uses `k8s.io/api/rbac/v1` types. `evaluation_helpers.go` remains because matcher functions and `CompactString` do not exist in staging. The removed registration, conversion, defaulting, and validation files are not used by the selected authorizer path. Removing them also stops import time mutation of `k8s.io/api/rbac/v1.SchemeBuilder`. This intentional behavior change is rendered into `docs/behavior-changes.md`, generated `NOTICE`, and package provenance.

The real external build baseline is recorded by the completed proof in `docs/rbac-v1.36.1-proof.md`; the exact closure shape is checked in at `testdata/closure/rbac-v1.36.1.json`:

```text
external boundary packages       11
non-stdlib transitive packages  198
compiled modules                 42
module graph nodes              139
go.sum lines                    133
```

Package, module, graph, and checksum-line counts are exact for the pinned release. `k8s.io/component-base` is included as a transitive staging dependency.

### 3. Patch application

`tools/internal/patchset` applies ordered unified diffs against upstream paths with `git apply --3way --index` before relocation or import rewriting. Pruning is reasserted after every patch pass.

A failed patch stops the complete ref transaction. The run writes a deterministic report containing source ref and SHA, selected profile, failing patch, status, conflict markers, and diff. CI uploads the report and updates one keyed tracking issue. No consumer branch or tag moves.

### 4. Public API type preference

`tools/internal/typeswap` implements `prefer-external`. It treats type replacement as a proof obligation, not a textual rewrite.

Reachability first distinguishes a real type substitution from dead-package pruning. A real substitution is allowed only when all applicable analyses pass:

1. Upstream generator markers identify an explicit internal and external type pairing.
2. Every retained reference that names the internal package is enumerated for rewriting.
3. Conversion code is field preserving and has no manual or lossy logic.
4. Retained selector use and full method sets remain compatible.
5. Field names, order, recursive types, JSON tags, and protobuf tags match.
6. Removed initialization and global mutations are either unobservable from the selected API or explicitly documented and tested as behavior changes.

Hand written conversions, missing methods, lossy fields, runtime scheme differences required by the facade, or unproved global effects block a real substitution.

Dead-package pruning makes no substitution claim. It requires the internal package to be absent from the retained closure, zero retained references to its symbols, at least one retained package already importing the configured external package, a valid upstream marker pair, no observable global effect, and byte-identical pre-prune and post-prune facade manifests. Conversion, method-set, and field-identity checks are recorded as inapplicable in this mode, because no Go value changes type.

RBAC follows that dead-package path. Retained code already uses `k8s.io/api/rbac/v1`; Kubernetes' unversioned internal declarations intentionally omit public serialization tags and carry helpers such as `CompactString` that the public declarations do not. No retained type is rewritten, and the analysis must not misreport those intentionally different declarations as interchangeable.

Dangling generator markers in retained `pkg/apis/rbac/v1/doc.go` are stripped only when their target or generated output was pruned. Every removed marker is listed in provenance. Other package documentation and `+groupName` remain untouched.

### 5. Staging dependency policy

`tools/internal/deppolicy` runs after a provisional module resolves real package and module graphs. The default action is to keep an external module dependency.

Copying selected staging packages is allowed only for pure leaf utilities with high measured leverage. The following correctness gates are non-overridable:

1. Interoperability gate. A candidate package cannot own a defined type, interface method type, or function type that crosses the generated public boundary. Relocation would create an incompatible Go type identity.
2. Global state gate. A candidate cannot contain unexported context keys, mutable exported singletons, feature gates, scheme mutations, metrics or other registry registration, or relevant `init()` side effects.
3. Diamond gate. A candidate cannot appear both relocated and externally reachable in the same consumer build.

Additional scored gates cover security critical code, native code, closure completeness, generated file count, distinct licenses and patents, update cadence, copied LOC, module zip bytes, compiled modules removed, and transitive LOC removed. Overrides may relax cost gates only. They require a justification, approver, and Kubernetes minor expiry. They never relax the three correctness gates.

When copying is approved, all files keep their complete upstream relative path below `internal/kk`, including `staging/src/k8s.io/<module>/...`. This preserves nested Go `internal` restrictions. Provenance records original module path, version, source SHA, license, patent files, and override.

No staging package is copied for RBAC. `k8s.io/apiserver` downloads a roughly 2.83 MB zip once and compiles only 6 of its 246 packages. Copying the closure would trade one module for ownership of the same 2,052 lines while breaking real `authorizer` type identity, request context keys, feature gates, registry behavior, CVE module identity, and tested staging version coherence. `docs/decisions/0001-no-staging-copy-rbac.md` records this decision, and a policy fixture must hard fail this copy proposal.

> **Follow-on**: The approved supplemental plan [`plans/github-token-unattended.md`](github-token-unattended.md) supersedes this decision only for pure component-helper validation packages that carry no API types and whose copy delivers measured module removal. Copying `k8s.io/apiserver` itself remains prohibited in external compatibility mode because external type identity is the load-bearing property.

### 6. Relocation, rewriting, and license notices

Kubernetes packages retain their full relative paths below `internal/kk`. This is a hard invariant because nested `internal` path elements use the last such element to enforce visibility.

For example:

```text
k8s.io/kubernetes/pkg/registry/rbac/validation
    becomes
monis.app/kk/rbac_authorizer/internal/kk/pkg/registry/rbac/validation
```

`tools/internal/rewrite` performs syntax aware transformations:

1. Only imports rooted at the configured source import prefix are eligible. Imports owned by external modules are never relocated or rewritten.
2. Go imports are replaced only at `ast.ImportSpec` literal positions in the original byte stream. This preserves aliases, comments, cgo preambles, build constraints, and surrounding formatting.
3. Kubernetes generator comments and `go:generate` directives use an explicit key allowlist. External type directives, API group strings, annotation strings, and arbitrary string literals are never globally replaced.
4. Proto `go_package` values use a dedicated parser. Embed paths are preserved and verified against copied assets.
5. Rewritten files are reparsed. Their non-import syntax shape must remain unchanged, and pinned `gofmt` must produce no additional diff.
6. Every modified source file retains its upstream license header and receives a deterministic prominent modification notice. Root `NOTICE` and per-package `SOAPBOX_PROVENANCE.txt` files record source repository, source SHA, pruning, behavior changes, relocation, import mapping, copied staging provenance, and patches.

### 7. Module dependency mapping

`tools/internal/gomodmap` and `tools/internal/modgen` build a provisional root module before dependency policy and facade loading.

1. Parse the source commit's root `go.mod`. Relative replacements into `staging/src` identify staging modules. Non staging versions are copied exactly.
2. At a Kubernetes tag `v1.X.Y[-pre]`, pin staging modules to `v0.X.Y[-pre]`.
3. Between tags, map the source commit to each observed staging repository by its `Kubernetes-commit` trailers. Adapt graph algorithms from `kubernetes/publishing-bot/pkg/git/{mainline,mapping,kube}.go` to the Git CLI transport.
4. Ask the Go toolchain to resolve the mapped staging commit to a valid pseudo version. Do not hand construct pseudo versions.
5. Cache source SHA to dependency version mappings on the state ref. Run `go mod tidy` under the pinned toolchain and fail if minimal version selection changes any computed pin.
6. Inherit the source module's `go` and `godebug` requirements. The engine module pins the exact patch toolchain used for deterministic formatting.

### 8. Curated facade

After the provisional module and dependency policy resolve, `tools/internal/facade` loads relocated packages with `go/packages` and generates the root facade deterministically.

1. Internal types and interfaces use aliases so caller implementations satisfy the copied contracts.
2. External module types are referenced directly or exposed through aliases. They are never redeclared or relocated.
3. Functions use real forwarding declarations with resolved signatures. Assignable function variables are prohibited.
4. Constants may be forwarded directly. Exported variables are rejected unless config specifies an explicit copy or accessor policy.
5. The generator recursively checks public signatures and exported members. A reachable internal named type needs a facade alias. A reachable external type must retain its real upstream identity.
6. An API manifest is compared with the expected surface. Upstream removals, signature changes, and collisions stop publication.
7. `zz_generated_assertions.go` contains compile-time assertions that the generated `RBACAuthorizer` implements the real `k8s.io/apiserver/pkg/authorization/authorizer.Authorizer` and `RuleResolver` interfaces.

The initial facade exports `New`, `RBACAuthorizer`, the four validation getter and lister interfaces, authorization resolver interfaces, `DefaultRuleResolver`, `NewDefaultRuleResolver`, rule evaluation helpers, subject locator interfaces, and required constructors. The four lister-backed structs with colliding names receive explicit names such as `RoleGetterFromLister`. Pruned RBAC builder helpers are not part of the first public API.

### 9. Deterministic commits and graph replay

`tools/internal/treebuild` creates Git objects through `hash-object`, an isolated index, `update-index --index-info`, `write-tree`, and `commit-tree`. It never uses a shell or mutates the user's working tree.

Each replay profile hash covers normalized output-affecting config, exact prune and deny entries, patch bytes and selectors, staging copy decisions, type policy decisions, engine version, and formatting toolchain. Observational limits and goldens gate publication but do not enter the profile hash because they do not alter output bytes.

1. Preserve upstream author name, email, date, message, and relevant parent relationships.
2. Set the `github-actions[bot]` identity as committer and use the upstream committer date for reproducibility.
3. Append exactly one `Kubernetes-commit: <sha>` trailer and force commit signing off.
4. Map an unchanged transformed tree to its nearest destination parent without creating a commit.
5. Deduplicate mapped merge parents and preserve a merge when a side parent contains generated changes. Port and test publishing-bot first-parent, merge-point, and source-to-destination algorithms rather than its shell engine.
6. Start a new profile epoch on the current destination branch after a control-plane change. Never regenerate older tags under a new profile.

Tracked release branches diverge from the common transformed anchor and only fast forward. A newly discovered branch is accepted only if its source history descends from the recorded anchor.

### 10. Release objects and append-only publication

At each selected source tag, generate an exact release projection with real `v0.X.Y` staging dependencies. If this differs from the replay commit's pseudo-version tree, add the deterministic dependency update commit shape used by Kubernetes staging repositories.

Create a deterministic annotated destination tag. It maps `v1.X.Y[-pre]` to `v0.X.Y[-pre]`, uses the upstream tagger timestamp, records the source release URL and SHA, and never moves.

`tools/internal/publish` enforces:

1. Existing identical tags are no-ops. Existing different tags are fatal.
2. Branch updates must be fast forwards.
3. The typed Git runner has no force push API and rejects force refspecs.
4. Final branch and tag updates use one atomic push after every gate passes.
5. Long backfills publish only progress refs and state between chunks. Installation tokens renew before expiry. Consumer refs remain at their last fully gated state.

## GitHub automation and security

### Publishing credential

Publishing uses the built-in `GITHUB_TOKEN` that GitHub Actions provides to every workflow run. The token is scoped to the repository, expires when the job ends, and requires no external secret management. Credentials reach Git through process environment configuration, never command arguments or remote URLs. Exact secret values seed a redacting writer before any subprocess starts.

The workflow token is scoped to the repository it runs in. It cannot reach `kubernetes/kubernetes`, so replayed issue-closing text cannot act upstream. Pull request workflows never receive write credentials. Publishing runs only from the protected default branch through `schedule` or explicitly authorized `workflow_dispatch`. `pull_request_target` is prohibited.

Token setup is automatic: `sync.yml` passes `${{ github.token }}` as `SOAPBOX_GITHUB_TOKEN`. See `docs/github-token.md`.

### Workflows

All actions are pinned to full commit SHAs and use least privilege workflow permissions.

1. `ci.yml` runs the local nested tool shim in read-only verification mode and never receives write credentials.
2. `sync.yml` runs at an off minute, supports manual dispatch, has one non-cancelling concurrency group, and contains only one single-line Go invocation for maintained logic. The job holds `contents: write` and `actions: read`. Automatic publication mode adds `-unattended`.
3. Failure artifacts contain conflict and gate reports after secret redaction.
4. The state ref records observed upstream heads and provides low-noise repository activity. Each successful run also checks that the workflow remains enabled. The 60-day public schedule behavior remains a monitored platform risk because GitHub does not precisely define qualifying activity.

### Vanity bootstrap

`tools/internal/vanity` generates `kk/rbac_authorizer/index.html` using the existing `enj/enj.github.io/go/index.html` shape. It commits exact `go-import` and `go-source` metadata once, waits until `https://monis.app/kk/rbac_authorizer?go-get=1` serves the expected metadata, and then normal sync stops requesting site repository access.

## Implementation phases

### Phase 0: Durable plans and signed repository bootstrap

1. Create `plans/`, move `goal.md` to `plans/goal.md`, and copy this approved plan to `plans/implementation.md` before the first commit.
2. Create project `CLAUDE.md` from the Go base template, set `group_id` to `go-soapbox`, document the nested module, and override Python checks with Go checks.
3. Initialize `main` locally. Configure `gpg.format=ssh`, `user.signingkey=/Users/mo/.config/wunderkind/ssh/id_ed25519.pub`, `commit.gpgsign=true`, and a repository-local allowed signers file.
4. Do not use `git commit -s`, because it would use `i@monis.app`. Insert the required `mok@microsoft.com` trailer literally.
5. Make one minimal bootstrap commit, then mechanically verify SSH signature, author, committer, and exactly one required trailer.
6. Redirect `GOPATH`, `GOMODCACHE`, and `GOCACHE` to `/Users/mo/.cache` for local Go commands.

### Phase 1: Engine foundation

Implement `tools/internal/cli`, `config`, and `gitcli`, plus public `tools/soapbox.go`. Add strict schema decoding, typed commands, context cancellation, path and host validation, secret redaction, signing doctor checks, and real temporary Git repository fixtures.

### Phase 2: Source graph, closure, pruning, patches, and rewriting

Implement `source`, `gitgraph`, `closure`, `patchset`, `relocate`, and `rewrite`. Port only proven publishing-bot graph algorithms. Add pre-prune and post-prune RBAC goldens, prune idempotence, missing-prune failure, exact deny reentry failure, patch-against-pruned failure, dangling marker stripping, behavior reports, adversarial syntax fixtures, and three-way conflict reports.

### Phase 3: Modules, dependency policy, public API types, and facade

Implement in this order: `gomodmap`, `modgen`, `deppolicy`, `typeswap`, `facade`, and `provenance`. Verify exact `v0.36.1` staging mapping, intermediate pseudo-versions, the `k8s.io/apiserver` copy hard failure, all five type-equivalence analyses, pre-prune and post-prune facade equality, external-type alias-only behavior, interface assertions, and per-file notices.

### Phase 4: Deterministic replay

Implement `treebuild` and `replay`. Exercise linear history, relevant and irrelevant commits, merge commits, side branches, branch-selective patches, prune and copy profiles, profile epochs, and unchanged tree collapse in real local repositories.

### Phase 5: Authenticated append-only publishing

Implement `ghapi`, `state`, `publish`, `report`, and token-based authentication. Use local bare remotes for normal tests. Assert tag moves, non-fast-forward branches, force refspecs, partial gate failures, and secret leakage all fail closed.

### Phase 6: Template setup and documentation

Implement the setup allowlist transformation from copied engine to pinned nested shim, generated `ci.yml` and `sync.yml`, template self-test, configuration reference, replay and determinism rationale, type and dependency policies, behavior changes, publishing token guide, vanity guide, and conflict runbook. Mark `enj/soapbox` as a GitHub template only at the outward-action gate.

### Phase 7: Real upstream dry run and technical spikes

Before creating any remote repository or public tag:

1. Measure blobless clone and sparse materialization over the selected source range. Add batched `cat-file` prewarming if lazy blob fetches dominate.
2. Compute and record the common source anchor for `master` and active release branches. Verify every selected tag descends from it.
3. Sample dependency mapping across exact tags and intermediate commits for every staging module observed in the measured graph, including direct and transitive staging modules.
4. Run the complete RBAC replay twice with different temporary paths and randomized in-memory iteration. Destination trees, commits, branches, and annotated tags must be byte-identical.
5. Produce a local bare `enj/rbac_authorizer` equivalent and local `v0.36.1` tag. Record the pre-prune and post-prune closure, facade, dependency, license, and behavior-change reports.
6. Type-check, build, vet, and test the post-prune module. Assert compile-time compatibility with real apiserver authorizer interfaces.
7. Assert the dependency graph remains inside the recorded package, module, zip-byte, graph-node, and `go.sum` tolerances, with no staging copy selected.
8. Exercise the CLI end to end with the project `verify` skill, not only unit tests.

### Phase 8: Explicit outward-action gate

Present one manifest containing:

1. Creation and initial push of `enj/soapbox`.
2. Creation of `enj/rbac_authorizer` from the template.
3. Workflow permissions and publishing token configuration.
4. Exact vanity page diff in `enj/enj.github.io`.
5. Every destination branch, commit OID, annotated tag OID, and source SHA.
6. The prune manifest, documented behavior changes, dependency policy decision, facade API, and interface assertions.
7. The immutable first public tag `v0.36.1` and all passing verification evidence.

Request a fresh explicit confirmation bound to the manifest hash. Only then create repositories, enable the template flag, push the vanity page, and publish the first module tag. Verify the live result with `go get monis.app/kk/rbac_authorizer@v0.36.1` from a clean cache before enabling unattended sync.

## Verification matrix

### Engine checks

Run from `tools/` with pinned writable caches:

```text
gofmt and goimports with no diff
go vet ./...
go test ./...
go test -race ./...
go build ./...
golangci-lint run
```

### Generated module checks

Run before every consumer ref update:

```text
gofmt with no diff
post-prune go/packages type check with all dependencies
go vet ./...
go test ./...
go build ./...
pre-prune and post-prune facade manifest equality
external authorizer interface assertions
closure and dependency golden diff
prune, marker-strip, behavior-change, provenance, and tag invariants
double-generation determinism check
```

Generated Kubernetes source is exempt from `goimports` regrouping because import-position preservation is intentional. Hand-maintained engine source uses goimports.

### Test strategy

1. Use table-driven tests for config, prune and deny rules, selectors, semver mapping, AST rewriting, type equivalence, dependency policy, facade symbols, and validation.
2. Use real temporary Git repositories and bare remotes for subprocess, patch, graph, tree, and publishing behavior. Do not mock Git, file I/O, or pure functions.
3. Build merge topology fixtures in Go and assert exact parent and source mapping tables.
4. Add security tests for traversal, symlink escapes, hostile refs, malicious patch paths, credential redaction, untrusted workflow events, forbidden force updates, incompatible copied types, context-key duplication, global registries, and expired overrides.
5. Add network integration tests behind an explicit flag, with Kubernetes `v1.36.1` RBAC as the canonical golden.
6. Run a live-module smoke test with static roles and listers through the public facade.
7. Assert template setup removes `plans/`, `.claude/`, engine source, and development-only files from the derived release tree.

## Small commit sequence

Each commit includes its tests and must pass the exact Soapbox signature, identity, and trailer check.

1. `chore(repo): initialize signed template and durable plans`
2. `feat(config): add strict schema and doctor command`
3. `feat(gitcli): add typed Git execution and redaction`
4. `feat(source): add partial clone and graph discovery`
5. `feat(extract): add package-granular closure and relocation`
6. `feat(closure): add exact pruning, denies, and measured goldens`
7. `feat(patchset): add ref-scoped three-way patches`
8. `feat(rewrite): add syntax-aware path and marker rewriting`
9. `feat(modules): add staging dependency mapping`
10. `feat(deps): add staging copy safety and leverage policy`
11. `feat(types): add internal-to-external API analysis`
12. `feat(api): add curated facade and interface assertions`
13. `feat(replay): add deterministic DAG transformation`
14. `feat(publish): add append-only refs and resumable state`
15. `feat(github): add App auth and vanity bootstrap`
16. `ci(template): add pinned verification and sync workflows`
17. `docs: add setup, provenance, behavior, and policy guides`
18. `test(rbac): add real upstream dry-run proof`

Release the engine with a signed annotated `tools/v0.1.0` tag only after the local proof passes. Repository creation, template enablement, vanity publication, and generated module tags remain outside local implementation until Phase 8 approval.

## Critical files

1. `plans/goal.md` and `plans/implementation.md` preserve intent and the approved execution design.
2. `soapbox.yaml` defines the stable schema, RBAC prune profile, type policy, dependency policy, and facade contract.
3. `tools/internal/gitcli/` is the only subprocess and credential boundary.
4. `tools/internal/closure/` controls package granularity, exact pruning, denied imports, and measured growth.
5. `tools/internal/deppolicy/` decides whether staging packages may be copied.
6. `tools/internal/typeswap/` proves internal-to-public API substitutions.
7. `tools/internal/rewrite/` performs minimal syntax-aware source changes.
8. `tools/internal/gomodmap/` aligns staging modules to each source commit.
9. `tools/internal/facade/` controls public API identity and compatibility.
10. `tools/internal/replay/` maps the source DAG into deterministic destination commits.
11. `tools/internal/publish/` is the final append-only safety boundary.
12. `.github/workflows/sync.yml` is the unattended production entry point.
13. `docs/behavior-changes.md`, `docs/dependency-policy.md`, `docs/replay-model.md`, `docs/provenance.md`, and `docs/conflict-runbook.md` record invariants future maintainers must not weaken.
