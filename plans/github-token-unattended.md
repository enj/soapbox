# GITHUB_TOKEN unattended sync and RBAC dependency-pruning plan

## Context

Soapbox is live at `enj/soapbox`; `monis.app/kk/rbac_authorizer@v0.36.1` is live and immutable. The initial bootstrap used a narrowly scoped GitHub App, but routine synchronization runs inside `enj/rbac_authorizer` itself, so the repository-scoped, job-scoped `GITHUB_TOKEN` is the simpler credential. The current scheduled workflow is deliberately plan-only and cannot yet read HTTPS refs, discover/fetch state, discover new releases, replay a relevant commit range, graft a new profile epoch, or self-apply safely. This change will remove the App only after a replacement workflow has proved it can reconcile and publish with `GITHUB_TOKEN`.

The same release line will add two dependency modes. `external` preserves the current real `k8s.io/apiserver` type identity and interface assertions. `local` intentionally breaks that compatibility, replaces the minimal apiserver API surface with local types, and is the mode selected for future `rbac_authorizer` releases. The pure `k8s.io/component-helpers/auth/rbac/validation.Covers` package will be copied internally in both modes so `k8s.io/component-helpers` leaves the module graph. Published `v0.36.1` is never regenerated or moved; the next eligible Kubernetes release starts a new epoch.

## Invariants

1. `GITHUB_TOKEN` is accepted only in the destination repository, on its configured default branch, for `schedule` or `workflow_dispatch`. It is never inherited by source Git or Go subprocesses.
2. The token travels only in subprocess environment/config, never argv, URLs, reports, artifacts, or logs; raw and encoded forms seed the existing `gitcli.Redactor` before first use.
3. Manual exact-hash plan/apply remains available. Automatic apply is a separate explicit operational policy and trusted workflow mode; the in-memory plan hash is never scraped from stdout.
4. Remote writes remain append-only, leased, fast-forward-only, and atomic by scope. Existing tags never move.
5. Replay covers the relevant upstream DAG, not release snapshots alone. Chunk checkpoints may move only progress/state refs; consumer refs move only after the complete release range passes all gates.
6. Engine version remains part of the profile epoch. A changed engine/profile grafts a new epoch onto current destination `main`; it never rewrites the old epoch or `v0.36.1`.
7. `external` apiserver mode keeps the current API exactly. `local` mode makes no apiserver compatibility claim and must prove both `k8s.io/apiserver` and `k8s.io/component-helpers` are absent from source imports, `go.mod`, `go.sum`, and the loaded module graph.
8. All maintained executable logic is Go. Tests use real temporary repositories, module proxies, and Git subprocesses rather than mocks.
9. Human Soapbox commits retain the existing signing/identity/trailer policy. New outward changes require a fresh exact manifest and approval.

## Phase 1 — schema and credential migration

### 1. Introduce profile schema v2

Update `tools/internal/config/` and `soapbox.yaml`:

- Remove `githubApp` and its required env-name/API URL validation.
- Add an operational `publication.mode: manual|automatic`, excluded from output-profile bytes because it changes outward behavior, not generated module bytes.
- Add `compatibility.apiserver: external|local`, included in profile bytes because it changes imports and public API.
- Add a generic fail-closed `dependencies.forbiddenModules` list, included in profile bytes.
- Change the local-mode RBAC committer to the honest Actions identity `github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>`; external profiles may retain their configured identity.
- Keep strict decoding. Add an explicit v1-to-v2 migration path for the already-derived repository rather than silently accepting stale App fields.

Create an approval-gated derived-repository upgrade command by reusing setup’s payload/action/atomic-write machinery (`tools/internal/setup/`). It updates only setup-owned files (`soapbox.yaml`, nested shim metadata/main, and generated workflows), reports exact creates/replaces/deletes, and refuses unknown overwrites or a dirty tree. This avoids making one-shot `setup` destructive or hand-editing live generated files.

### 2. Replace App auth with a typed GITHUB_TOKEN credential

In `tools/internal/gitcli/`:

- Add a typed GitHub HTTPS credential constructor for `x-access-token:<token>` using host-scoped `GIT_CONFIG_COUNT/KEY/VALUE` entries. The authorization header remains environment-only.
- Seed the raw token, Basic payload, base64 credential, and complete header into the redactor.
- Refuse empty/control-character credentials, duplicate/conflicting config entries, non-`github.com` HTTPS targets, and any attempt to reuse the credential on anonymous source commands.
- Add `RemoteRefs(ctx, remote)` around `git ls-remote --refs`, with remote rewrite checks, exact ref/OID parsing, deterministic sorting, duplicate refusal, bounded output, cancellation, and no peeled-tag ambiguity.
- Add an exact remote-ref fetch that downloads the advertised state commit without moving consumer refs or writing `FETCH_HEAD`.

In `tools/internal/publish/remote.go`, add an HTTPS `RemoteRefLister` backed by the new Git method. Keep `LocalRemote` for rehearsals.

In `tools/internal/ghapi/`, retain the generic REST client and add a static bearer authorizer for `GITHUB_TOKEN`; use it for repository/workflow checks. Delete the unused App-specific `tools/internal/ghapp/` package.

In `tools/internal/cli/sync.go`:

- Read fixed env name `SOAPBOX_GITHUB_TOKEN` only for sync publication.
- Build separate anonymous source and credentialed destination runners.
- Hide that credential from the nested `generate.Generate` environment lookup while preserving the stronger anonymous-runner and controlled-inherit checks.
- Validate Actions context (`GITHUB_ACTIONS`, `GITHUB_REPOSITORY`, `GITHUB_REF`, `GITHUB_EVENT_NAME`) before unattended mode can read or write.

### 3. Generate a GITHUB_TOKEN workflow

Update `tools/internal/setup/workflows.go` and tests:

- Job permissions: `contents: write`, `actions: read`; never `id-token: write`.
- Pass `SOAPBOX_GITHUB_TOKEN: ${{ github.token }}` only to the Soapbox step; remove App secret injection.
- Keep `persist-credentials: false`, protected-default-branch guard, `schedule` at `37 4 * * *`, `workflow_dispatch`, non-cancelling singleton concurrency, and pinned actions.
- Run all generation/module/replay gates inside sync because GITHUB_TOKEN pushes intentionally do not trigger another `push` workflow.
- Add `-unattended` only when `publication.mode` is `automatic`; manual mode remains plan-only.

Replace `docs/github-app.md` with a GITHUB_TOKEN security/operation guide and update README, config reference, setup, determinism, replay, and conflict-runbook text. Add `*.private-key.pem` to tracked ignore rules.

## Phase 2 — remote state and trusted apply

1. List destination refs before generation. Verify canonical same-repository identity, object format, state namespace, immutable tags, and expected main ancestry.
2. Discover `soapbox-state`; fetch its exact advertised object into the local object database and call existing `state.Load`. Remove the need for workflow-supplied `-state-commit` while retaining the flag as a manual override whose value must equal the advertised ref.
3. If state and consumer refs already describe the requested release, return a deterministic no-op without regenerating the old release under a new engine/profile.
4. In unattended mode, call existing `sync.Apply` with the verified in-memory `Manifest.Hash`. This self-approval is legal only after workflow-context checks and `publication.mode: automatic`; manual CLI paths still require explicit `-approve`.
5. Before and after apply, re-read remote refs. Preserve the existing state-first then consumer-scoped atomic CAS ordering and reconcile observed state on the next step/run.
6. Use `ghapi.Workflow` to verify `.github/workflows/sync.yml` is enabled; fail clearly rather than silently losing scheduled synchronization.

## Phase 3 — source discovery, relevant-DAG backfill, and epochs

### 1. Discover pending releases

Add a source discovery layer using the existing bare cache and `config.MapReleaseTag`:

- Fetch/list annotated semantic tags at or above `minimumRelease`.
- Respect prerelease policy.
- Verify tag objects and ancestry from the immutable anchor.
- Compare destination tags and state to compute pending releases in semantic order.
- Treat existing identical destination tags as no-ops and differing tags as fatal.

### 2. Transform the relevant commit range

Extend internal ref selection with an engine-only exact-commit kind. Build the source commit DAG between the recorded cursor/anchor and pending release heads using existing `gitcli.CommitLog`, raw dates, `gitgraph`, and `replay.Run`.

For each commit:

- Use changed watched package directories as a cheap relevance prefilter; watched sets are directory-granular and expand when imports change.
- Collapse provably irrelevant commits onto their mapped parents.
- For relevant commits, run extraction/generation against the exact commit.
- Resolve intermediate Kubernetes staging commits through the existing `gomodmap.ResolveCommitVersions` path and append the mapping index; never invent pseudo-versions.
- Preserve merge parents and replay metadata through existing replay APIs.
- At source release tags, run `release.Project` and all generated-module gates before creating the destination tag object.

### 3. Chunk and resume safely

Drive the range in `determinism.chunkSize` source-commit chunks:

- Use existing `state.Track` and `refs/soapbox/progress/<track>` for chunk cursors.
- Push only progress/state refs after intermediate chunks.
- Record mapping blob/digest/entry count in state.
- Resume by verifying source and destination ancestry, profile, mapping, and exact progress OIDs.
- Move `main` and release tags only once all chunks through that release pass.
- Loop within the 180-minute workflow budget; leave a valid checkpoint and exit successfully when another run must continue.

### 4. Graft new epochs

Remove sync’s current artificial refusal on a changed source/profile and use the new-epoch support already present in `state.Merge`:

- Same profile: continue the prior cursor/mapping.
- Changed engine/profile/compatibility mode: create a new epoch whose source is the first new commit and whose destination graft is the currently observed destination `main`.
- Require both source and destination grafts to descend from the previous epoch.
- Never regenerate an already published tag under the new epoch.

The steady state is zero pending commits/releases and a fully no-op ref plan.

## Phase 4 — component-helpers copy materialization

Finish the already-designed `deppolicy` copy path instead of embedding an ad hoc maintained function:

1. Implement materialization of an approved staging package after policy evaluation.
2. For `k8s.io/component-helpers/auth/rbac/validation`, copy its single `policy_comparator.go` package under the preserved staging path below `internal/kk`, rewrite the call-site import, and retain source commit/module/license/patent provenance.
3. Recompute closure/module graph and rerun interoperability, global-state, diamond, completeness, security/cadence/cost, license, and facade gates after copying.
4. Configure the RBAC profile with `copy-approved`, that one package, measured cost ceilings/justification, and `forbiddenModules: [k8s.io/component-helpers]`.
5. Assert `Covers` behavior differentially over upstream fixtures and randomized rule sets. Prove `k8s.io/component-helpers` disappears while `k8s.io/api`, `apimachinery`, and `client-go` remain coherent.

## Phase 5 — dual apiserver compatibility modes

### External mode

Preserve current behavior exactly:

- Real `user.Info`, `authorizer.Attributes/Decision/Authorizer/RuleResolver` and rule-info types.
- Real request context keys.
- Current external interface assertions.
- `k8s.io/apiserver` remains required and `forbiddenModules` must not name it.

### Local mode (selected for future rbac_authorizer releases)

Add a deterministic compatibility transformation selected by `compatibility.apiserver: local`:

- Generate local `UserInfo`/`DefaultUserInfo`, `Attributes`, `Decision` constants, `Authorizer`, `RuleResolver`, `ResourceRuleInfo`, `NonResourceRuleInfo`, and default rule-info structs covering exactly the retained RBAC signatures.
- Inline `SystemPrivilegedGroup` and the primitive-only service-account username comparison.
- Remove `request.UserFrom`/`NamespaceFrom` context-key dependence. Change `ConfirmNoEscalation` to take explicit `UserInfo` and namespace (or prune it if the facade does not expose it); never recreate incompatible private context keys.
- Rewrite retained imports/signatures to the local compatibility package using minimal AST/patch transformations with modification notices and upstream provenance.
- Export the reachable local types through the root facade, remove external apiserver assertions, and add local interface assertions.
- Set `forbiddenModules: [k8s.io/apiserver, k8s.io/component-helpers]` and fail if either appears in imports, requirements, checksums, `go list -m all`, or the typed graph.
- Record the intentional public API break in behavior changes, NOTICE, README, facade manifest, and release proof.

Generate both modes from the same upstream fixture in tests. External mode must still compile against real apiserver interfaces; local mode must compile without downloading apiserver and pass equivalent RBAC authorization/rule-resolution/subject-location behavior tests. Measure and record module count, zip bytes, compiled packages, and clean-cache latency before/after.

The live `v0.36.1` remains external and untouched. The live profile changes to local mode only for the next eligible Kubernetes release, creating a new profile epoch and a new immutable `v0.36.x` tag.

## Phase 6 — derived-repository migration and App decommission

1. Release the engine only after local dual-mode and unattended proofs pass. The initial implementation shipped as `tools/v0.2.0`; `tools/v0.2.1` adds fail-closed control-plane graft discovery and a destination-only `verify-write` dispatch required by the live migration.
2. Use the new approval-gated upgrade command to produce an initial exact change for `enj/rbac_authorizer`: schema v2/profile local mode with **manual** publication, engine pin/checksums, GITHUB_TOKEN workflow, and committer identity.
3. Rehearse against local bare HTTPS-like remotes with multiple upstream tags, merges, a crash mid-chunk, remote drift, token redaction, epoch change, and fixed-point reruns.
4. Produce a fresh outward-action manifest containing the exact Soapbox engine tag, both derived control-plane commits, workflow/profile diffs, default-branch protection, ref leases/OIDs, and App cleanup.
5. After approval, publish the engine and manual control-plane commit, then mark destination `main` protected with force pushes and deletions disabled but without rules that reject unsigned replay commits or job-scoped direct pushes. Dispatch and verify a manual plan-only run.
6. Apply and publish the second exact control-plane commit, changing only publication policy and its generated workflow to automatic. Dispatch it with `verify-write: true`; that destination-only path skips source discovery, its atomic leased no-op branch push must report `writeVerified: true`, every consumer/state/progress ref must remain unchanged, and no recursive CI run is expected even when a source release is pending.
7. Only after replacement verification: delete the three App secrets, uninstall/delete the App, delete the local PEM, and verify the App reaches no repositories. Keep the tracked private-key ignore rule.
8. Do not move `v0.36.1`; the automatic pipeline waits for and publishes only the next eligible release.

## Critical files

- Auth/remote transport: `tools/internal/gitcli/{gitcli.go,source.go,redact.go}`, `tools/internal/publish/remote.go`, `tools/internal/ghapi/`, delete `tools/internal/ghapp/`.
- Workflow/CLI/config migration: `tools/internal/cli/sync.go`, `tools/internal/setup/{compose.go,workflows.go}`, `tools/internal/config/{config.go,validate.go}`, `soapbox.yaml`, `.gitignore`.
- Unattended orchestration: new `tools/internal/sync/reconcile.go`, existing `sync.go`, `apply.go`, `state/`, `replay/`, `release/`, `source/`, `gomodmap/`.
- Upgrade path: reuse `tools/internal/setup/` composition/classification; add a narrow derived-repository upgrade command/package.
- Dependency modes: `tools/internal/generate/deps.go`, `deppolicy/`, `relocate/`, `rewrite/`, `provenance/`, `facade/`, RBAC patches/profile and closure/dependency goldens.
- Docs: README, setup, config reference, determinism, replay model, dependency policy, behavior changes, provenance, conflict runbook; replace GitHub App guide.

## Verification

1. Run `gofmt` and `goimports` with no diff; `go vet ./...`, `go test ./...`, `go test -race ./...`, `go build ./...`, and `golangci-lint run` with the project’s writable caches.
2. Credential tests: token absent, malformed, encoded leakage, argv/URL/log/report scans, wrong host/repository/ref/event, source-runner isolation, cancellation, credential renewal per job boundary.
3. Git integration: HTTPS ref listing, annotated tags, exact state fetch, stale CAS, atomic rollback, remote rewrites, branch/tag drift, protected no-force/no-delete behavior.
4. Reconciliation fixtures: multiple ordered tags/prereleases, relevant/irrelevant commits, merges, intermediate staging versions, profile epoch graft, chunk crash/resume, fixed point, existing immutable tag conflict.
5. Workflow golden tests: permissions, `${{ github.token }}`, no App secrets/id-token/pull_request_target, pinned actions, default-branch guard, singleton concurrency, plan/manual/automatic modes.
6. Generated modules: external and local compatibility variants, clean cache, type loading, format/vet/test/race/build, facade manifests, provenance/licenses, module graph forbidden-module assertions, differential RBAC behavior.
7. Live canary only after exact approval: workflow dispatch plan, GITHUB_TOKEN automatic no-op/new-release publish, live OID verification, clean-cache `go get`, workflow-enabled check, then App/secret/PEM removal verification.
