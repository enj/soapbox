# Setup

How to go from this template to a derived repository, and what each command does
along the way. Everything described here is local. No command in this guide
creates a repository, pushes a ref, or publishes a tag.

## Prerequisites

| Requirement | Why |
|---|---|
| Go `1.26.5` exactly | `determinism.toolchain` pins it. The engine refuses to run under any other toolchain because gofmt output and generated module metadata are toolchain dependent. |
| Git `2.45` or later | `GIT_NO_LAZY_FETCH` is honoured from that release, which is what keeps a blobless partial clone from silently fetching during a local probe. |
| Writable Go caches | The Kubernetes module graph does not fit in a small default cache. |

Run every engine command from `tools/`:

```text
GOPATH=/Users/mo/claude/.gocache/gopath
GOMODCACHE=/Users/mo/claude/.gocache/mod
GOCACHE=/Users/mo/claude/.gocache/build
GOLANGCI_LINT_CACHE=/Users/mo/claude/.gocache/lint
```

## The commands

```text
soapbox <command> [flags]

doctor    check the local toolchain, identity, and signing policy
validate  decode and validate the soapbox.yaml profile
plan      compute the extraction plan for one upstream ref
generate  compose the generated module for one upstream release tag
sync      plan, and with an approval publish, one upstream release
setup     transform this template checkout into one derived repository
upgrade   upgrade setup-owned files in a derived repository to a new engine
version   print the engine version
help      print usage for soapbox or one command
```

Exit codes are part of the workflow contract and are stable:

| Code | Name | Meaning |
|---|---|---|
| 0 | `ExitOK` | Success. |
| 1 | `ExitFailure` | An unexpected runtime failure. |
| 2 | `ExitUsage` | A malformed command line. The failing command's flags are printed. |
| 3 | `ExitCheck` | The command ran and found policy violations. This is the code CI reads as "something to review". |
| 4 | `ExitCanceled` | The context ended before the command did. |

Stdout carries the machine-readable artifact and nothing else. Every diagnostic
goes to stderr, so a workflow that captures stdout gets one artifact rather than
an artifact with a log appended to it.

### doctor

```text
go run ./cmd/soapbox doctor -dir ..
```

Checks the local toolchain and, when `-dir` is a Git repository, this project's
commit policy: `gpg.format=ssh`, the configured signing key, commit and tag
signing, an allowed signers file that actually authorizes the policy key, and
that `HEAD` is signed by that key with author and committer `Monis Khan
<i@monis.app>`, exactly one `Signed-off-by: Monis Khan <mok@microsoft.com>`
trailer, and no co-author trailers. A repository with no commits passes rather
than failing, so the check is usable on a fresh checkout.

The Go version check is the one deliberate exception: a version at or above the
`1.26` floor that is not the pinned patch release is a warning, not a failure.

### validate

```text
go run ./cmd/soapbox validate -dir ..
```

Decodes `soapbox.yaml` strictly and prints a summary. `-format canonical` prints
the normalized profile; `-format profile` prints the configuration portion of
the replay profile hash. The released engine version is framed beside those
bytes, so changing either starts a different epoch.
A profile the operator can fix — an unknown field, a duplicate key, a failed
validation rule — exits 3, not 1.

### plan

```text
go run ./cmd/soapbox plan -dir .. -cache /state/src -tag v1.36.1
```

Computes one extraction: acquire the source, materialize the configured root
packages in a sparse work tree, prune, apply the selected patch series, iterate
to a closure fixed point, relocate, and rewrite. It stops before module
composition, so it needs no Go toolchain and no module proxy.

`-materialize` writes the relocated tree to `-out`. Without it the plan measures
and reports without leaving a tree behind. `-report <path>` writes the JSON
report even when the run is refused, which is what makes a refusal reviewable.

`plan` refuses to start if `SOAPBOX_GITHUB_TOKEN` is set in the environment. A
plan needs no credential, so holding one is a configuration error rather than a
convenience.

### generate

```text
go run ./cmd/soapbox generate -dir .. -cache /state/src -tag v1.36.1 -materialize
```

Everything `plan` does, then: resolve staging module versions, compose and
verify the root `go.mod`, generate the facade from both the pre-prune and the
post-prune tree and refuse any difference between them, run the type policy, run
the dependency policy, render provenance, and write the module.

`-cache` is required and must sit outside the profile directory. A generation
removes its scratch root and refuses to write into an existing output tree, so
defaulting either to a directory nobody named is how a run deletes something an
operator was keeping. The version index defaults to `<cache>/staging-versions.json`;
it is the one path the engine allows to live inside the cache.

### setup

```text
go run ./cmd/soapbox setup -dir .. -engine-version tools/v0.2.2 \
    -engine-sum /tmp/engine.sum
go run ./cmd/soapbox setup -dir .. -engine-version tools/v0.2.2 \
    -engine-sum /tmp/engine.sum -apply -approve <hash>
```

Transforms a template checkout into a derived repository, in place. It defaults
to a dry run and stays that way unless the operator both asks to apply and names
the manifest hash they read. See [What setup does](#what-setup-does).

### sync

```text
go run ./cmd/soapbox sync -dir .. -cache /state/src -destination /path/to/repo -local-remote
```

Everything `generate` does, then replay the source commit, project the release,
build the state record, and produce a publication manifest. Without `-apply` it
plans and nothing outward happens. The destination must already have the
setup-derived control-plane commit, either as its published consumer branch or
as local `HEAD` during a pre-push rehearsal. Sync preserves `soapbox.yaml`,
patches, the pinned `tools` shim, workflows, and other operator-owned files while
replacing the generated module paths; it refuses an empty destination rather
than publish an unmaintainable generated-only root. See
[Publication](#publication) for what is and is not possible today.

### upgrade

```text
soapbox upgrade -engine-version tools/v0.2.2 \
    -engine-mod /path/to/engine/go.mod \
    -engine-sum /path/to/engine/go.sum \
    -target-config /path/to/approved/soapbox.yaml
```

Upgrades the setup-owned files in a derived repository to a new engine release.
The root `go.mod` (generated module output) is never overwritten. The nested
`tools/go.mod`, `tools/go.sum`, engine shim, and two workflows are always
upgrade-owned. `soapbox.yaml` is upgrade-owned only when schema migration or an
explicit `-target-config` changes it.

**Bootstrap note**: a schema-v1 derived shim is pinned to `tools/v0.1.0` and
does not contain the `upgrade` command. Run the *target* engine binary — built
from the approved engine candidate checkout or, once released, via
`go run github.com/enj/soapbox/tools/cmd/soapbox@v0.2.2` — against
`-dir <derived>`, not the old `tools/cmd/soapbox` shim inside the derived
repository.

Required flags: `-engine-version` names the target release. `-engine-mod`
points to the engine's own `go.mod` at that release (not the derived shim's).
`-engine-sum` provides the verified `go.sum` for the nested tools module.

`-target-config` is optional. When supplied, it names the exact current-schema
profile bytes to install, so a release may upgrade engine, workflow, dependency,
and compatibility policy atomically. The target is decoded strictly, must be a
regular file rather than a symlink, and may not retarget immutable source,
destination, release, provenance-key, or vanity identity fields. The sole
identity fill is resolving an empty `source.refs.anchorCommit`; once non-empty,
that anchor is immutable too. Exact target bytes and the prior profile digest are
included in the approval manifest. Without the flag, a current profile is
retained and a schema-v1 profile receives only the default migration described
below.

The upgrade refuses to run on a dirty work tree, on a repository that is not
setup-derived (the root `go.mod` must declare the destination module and
`tools/go.mod` must pin the engine), or on a downgrade (a target version older
than the current pin). Every update action records a preimage digest, and Apply
verifies the preimage still matches before the first write. Non-regular files
(symlinks, devices, directories) at owned paths are refused.

A schema v1 profile with a `githubApp` section is migrated to v2
automatically: the `githubApp` section is removed, `publication.mode` is set to
`manual`, and `compatibility.apiserver` to `external`. The migrated
`soapbox.yaml` is included in the manifest so the approval hash binds the
profile change. The legacy profile is validated strictly under the v1 schema
(unknown fields, missing App section, and invalid App env names are rejected)
before migration.

Without `-apply` the command reports the manifest and writes nothing.
`-apply -approve <hash>` writes the exact manifest that hash names.
`-report <path>` writes the JSON manifest to a file. `-format json` outputs
JSON to stdout.

## What setup does

`soapbox setup` composes every file it owns from the profile alone, classifies
every tracked path in the repository, and reports a manifest. Applying that
manifest is a second, separately approved step.

### The payload

Exactly six paths are written, and no others:

```text
go.mod                          the derived repository's root module, no requirements yet
tools/go.mod                    the nested shim, pinning the engine and its indirect graph roots
tools/go.sum                    only when -engine-sum supplies complete verified checksums
tools/cmd/soapbox/main.go       the shim command
.github/workflows/ci.yml        read-only verification
.github/workflows/sync.yml      publishing
```

The root module carries no requirements. The first generation writes them, so
setup never guesses a dependency graph.

### What is removed

Development-only material does not reach a derived repository:

```text
.claude/            .serena/            plans/
docs/               tools/cmd/          tools/internal/
.golangci.yml       CLAUDE.md           tools/soapbox.go
tools/soapbox_test.go
.github/workflows/template-selftest.yml
```

`docs/` is on that list, which is why this guide lives in the template and not
in a generated module: a derived repository documents the module it publishes,
not the engine that built it.

### What is kept

`LICENSE`, `NOTICE`, `README.md`, `soapbox.yaml`, `doc.go`, `.gitattributes`,
`.gitignore`, `patches/`, the facade and assertions files named in the profile,
and everything under `destination.internalPrefix`. Setup writes no `README.md`,
`NOTICE`, or `LICENSE`; the first generation renders them.

Anything tracked that setup does not recognise is preserved and reported under
`ignored`. Setup never deletes a file it cannot name.

### Preconditions

`-dir` must be the root of a Git repository with a `HEAD`, a clean work tree,
and no tracked symlinks. The repository must still look like a template:
`soapbox.yaml`, `plans/implementation.md`, `tools/soapbox.go`,
`tools/internal/cli/cli.go`, and `tools/cmd/soapbox/main.go` must be tracked,
and a root `go.mod` must not be. The profile's `determinism.toolchain` must
equal the engine's own.

### The engine pin

`-engine-version` is required, spelled either `v1.2.3` or `tools/v1.2.3`. It
must be a canonical semantic version naming an immutable release. A
pseudo-version is refused, because the shim pins a published release so the
running engine can be read off the `go.mod`. Run setup from the matching
version of the template: setup reads that template's `tools/go.mod` and records
the engine's graph roots as indirect requirements of the shim. Omitting them
makes Go's pruned module graph request a `go mod tidy` before the shim can run.

`-engine-sum` is optional and takes a *file* holding the complete verified
`go.sum` content for the nested module. A module checksum cannot be computed
from a checkout. Without this input, `tools/go.sum` is not written and the
manifest records a notice telling you to run `go mod download all` inside
`tools/` once the pinned release exists. With it, every line is validated and
the file must cover both the module and `/go.mod` checksum for the pinned engine
and every graph root named by its `go.mod`. This is what lets the generated shim
run from a clean module cache without modifying `tools/go.mod` or `tools/go.sum`.

### The approval

A dry run prints a manifest containing every action — `create`, `replace`, and
`delete` — each with the digest and byte count of the content involved. A delete
records the digest of what it destroys, so an approval covers removed bytes
rather than a filename.

`-apply` requires `-approve <hash>`, and `-approve` requires `-apply`. Neither
half means anything alone. Apply recomputes the plan from the repository as it
is now and refuses if the hash differs, then re-reads and re-digests every file
it is about to delete. The setup manifest hash is bare hex, with no `sha256:`
prefix; the sync manifest hash carries one. They are different artifacts and the
spellings do not interchange.

`-report` must point outside the repository. Setup requires a clean work tree,
so a manifest written into that tree would make the very next command refuse to
run.

## The derived repository

```text
rbac_authorizer/
├── go.mod
├── go.sum
├── authorizer.go                  the curated facade
├── zz_generated_assertions.go     compile-time interface assertions
├── doc.go
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

`refs/heads/soapbox-state` holds the resumable state record without entering the
module tree. `refs/soapbox/progress/` is reserved for gated backfill chunks.

### The generated workflows

`ci.yml` runs on pushes and pull requests to the default branch with
`permissions: {}` at the top level and `contents: read` on the job. It builds,
vets, and tests the root module and the shim, then runs
`go run ./cmd/soapbox validate -dir ..`. It never receives write credentials
and the checkout keeps no token.

`sync.yml` runs on `schedule` at `37 4 * * *` and on `workflow_dispatch`, never
on pull request code, and there is no `pull_request_target` trigger. The job
refuses to run from any ref but the protected default branch, serializes on the
non-cancelling concurrency group `soapbox-sync`, and holds `contents: write`
plus `actions: read`. It first builds the Soapbox binary in a tokenless step.
Only execution steps receive `SOAPBOX_GITHUB_TOKEN`:

```text
${{ runner.temp }}/soapbox sync -dir .. -destination .. -cache ${{ runner.temp }}/soapbox-cache
```

`publication.mode: manual` lists HTTPS refs, discovers pending releases, and
emits the exact next checkpoint or release plan without applying it. A second
manual dispatch may carry that hash in the optional `approve` input; the hash is
validated from `SOAPBOX_APPROVAL`, the step deterministically replans, and only
an exact match applies.

`publication.mode: automatic` normally adds `-unattended`; after the workflow
context, protected branch, enabled workflow, destination state, and in-memory
plan hash have all been verified, it applies progress/state checkpoints and final
consumer refs without a copied hash from stdout. Its manual dispatch also exposes
an optional boolean `verify-write` input. When true, a separate `-verify-write`
step skips source discovery and proves write access with an atomic leased no-op
push of the current branch.

Both workflows pin their actions to full commit object names.

## Publication

The typed Git boundary lists HTTPS refs, fetches exact advertised state and tag
objects without moving consumer refs or writing `FETCH_HEAD`, and publishes with
atomic compare-and-swap leases. Long histories advance only state and
`refs/soapbox/progress/*` between chunks. The consumer branch and immutable tag
move together only after the complete release passes all generation and module
gates. A post-publication state push atomically leases unchanged branch, tag,
and progress observations so a concurrent ref change cannot be recorded as
fact.

Local rehearsals remain available with `-local-remote`. Network publication is
available only in a validated same-repository Actions context through the
job-scoped `GITHUB_TOKEN`.

## Current limitations

These are properties of the engine as it stands, not of the approved design.

1. **Moving branch names are preview-only.** Public `generate` and manual exact
   `sync` calls select reviewed release tags. Unattended reconciliation uses an
   engine-only exact-commit selector after a release head and immutable lower
   anchor have bounded the source history.
2. **Retained-reference type rewrites remain profile-specific.** The generic
   `prefer-external` analysis proves substitutions, but a substitution that
   changes retained source still needs an enumerated deterministic rewrite.
3. **One consumer release per workflow invocation.** A run may apply many
   progress chunks within its budget, but stops after publishing and reconciling
   one immutable release so the next invocation starts from a fresh checkout of
   that exact consumer head.
4. **No vanity page generation.** See [vanity.md](vanity.md).
5. **No repository creation.** Repository creation and all other bootstrap
   actions remain outside unattended sync and require a separately approved
   outward-action manifest.

## Where to look next

| Question | Document |
|---|---|
| What every profile field means | [config-reference.md](config-reference.md) |
| How source history becomes destination history | [replay-model.md](replay-model.md) |
| Why two runs produce identical bytes | [determinism.md](determinism.md) |
| What the generated module records about its origin | [provenance.md](provenance.md) |
| What the generated module does differently from upstream | [behavior-changes.md](behavior-changes.md) |
| When a staging package may be copied | [dependency-policy.md](dependency-policy.md) |
| How the publishing identity is set up | [github-token.md](github-token.md) |
| How `monis.app/kk/...` resolves | [vanity.md](vanity.md) |
| What to do when a run is refused | [conflict-runbook.md](conflict-runbook.md) |
