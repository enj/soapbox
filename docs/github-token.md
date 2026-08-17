# GitHub Token

Soapbox publishes using the built-in `GITHUB_TOKEN` that GitHub Actions
provides to every workflow run. The token is scoped to the repository the
workflow belongs to, expires when the job ends, and requires no external
secret management.

## The security posture

Five properties hold, and each is enforced somewhere rather than merely
intended.

**The generated repository never touches `kubernetes/kubernetes`.** Replayed
commit messages contain upstream text, including issue-closing phrases such as
`Fixes #1234`. Writing to the upstream repository would act on those phrases.
The workflow token cannot, because it is scoped to its own repository.

**Pull request workflows never receive write credentials.** The generated
`ci.yml` holds no secret, its token may only read, and its checkout is told not
to persist a token so a later step cannot find one. `pull_request_target` — the
one trigger that would hand a fork's code the repository's own secrets — is
prohibited and appears in no workflow.

**Publishing runs only from the protected default branch.** The generated
`sync.yml` triggers on `schedule` and `workflow_dispatch` only, and its job
refuses to run from any ref but the default branch.

**The workflow token has exactly the permissions the job needs.** `sync.yml`
grants `contents: write` (to push branches and tags) and `actions: read` (to
verify the workflow is still enabled). No other permission is requested.

**Credentials never appear in an argument vector.** They reach Git through the
process environment only, using host-scoped runtime `http.extraHeader`
configuration that the engine sets and resets per invocation. There is no
credential helper, no askpass program, and no credential embedded in a remote
URL anywhere in the engine. Messages and tag bodies travel on stdin; identities
travel in the environment.

## Repository prerequisite

GitHub must report the configured default branch as protected before automatic
synchronization is enabled. Otherwise the generated job's
`github.ref_protected` guard intentionally skips every schedule and dispatch.
The protection must still permit the repository's job-scoped `GITHUB_TOKEN` to
make Soapbox's leased, atomic, fast-forward pushes. In particular, do not require
a pull request, signed replay commits, or status checks that a `GITHUB_TOKEN`
push cannot trigger. Force pushes and deletions remain disabled. Enabling or
changing this repository setting is an outward action and belongs in the exact
migration manifest.

## Configuration

The workflow passes the built-in token as `SOAPBOX_GITHUB_TOKEN`:

```yaml
env:
  SOAPBOX_GITHUB_TOKEN: ${{ github.token }}
```

The engine reads this environment variable to authenticate pushes, destination
ref reads, object fetches, and GitHub API checks. In manual mode the optional
workflow-dispatch `approve` input is separately exposed as `SOAPBOX_APPROVAL`;
it is a public manifest digest, is validated as lowercase `sha256:<64 hex>`, is
hidden from nested generation, and never enters the command line. No additional
secrets or App installation are required.

## Publication modes

The `publication.mode` field in `soapbox.yaml` controls whether the sync
workflow self-applies:

- **`manual`** — scheduled and empty manual dispatches plan only. After
  reviewing the reported hash, an operator dispatches the same workflow with
  its optional `approve` input set to that exact hash. The input travels as
  `SOAPBOX_APPROVAL` in the execution environment, never through the command
  line; the engine deterministically replans and applies only an exact match.
  This is the default for new profiles.
- **`automatic`** — the generated workflow adds `-unattended` to the sync
  command. A scheduled or ordinary manual dispatch self-applies the in-memory
  manifest under the trusted policy, so publication proceeds without further
  human intervention once the profile has been approved. A manual dispatch may
  instead set its boolean `verify-write` input; that selects `-verify-write`,
  performs only an atomic leased no-op push of the current branch, and never
  discovers or publishes a source release.

## Workflow token isolation

The generated sync workflow separates the build from the execution so the
publishing credential never reaches the Go build or module-download subprocess:

1. **Build step** (no token):
   `go build -o ${{ runner.temp }}/soapbox ./cmd/soapbox`
2. **Execution step** (receives `SOAPBOX_GITHUB_TOKEN`): runs the pre-built
   binary with sync flags.

Both steps run inside `tools/`. Only the execution step's `env` block carries
the token.

## Push safety

A push target must be `https` to `github.com`, or a local path for a rehearsal,
and may not embed credentials. A named remote is rejected because its URL lives
in configuration. Before pushing, all configuration scopes are queried for
`url.*.insteadOf`, `url.*.pushInsteadOf`, and `remote.*.pushurl`, and a
rewritten remote fails closed.

There is no force-push API. A `+`-prefixed refspec, a refspec with an empty
source, and the all-zero null object are each refused by name. Progress and state
checkpoints are one non-consumer atomic scope; final branch and tag publication
is a separate consumer scope. Post-publication reconciliation advances state in
an atomic push that includes unchanged branch, tag, and progress refspecs as
compare-and-swap leases, and refuses any reconciliation plan that would move a
consumer ref.

## Migrating from GitHub App

If upgrading from a schema v1 profile that used a GitHub App:

1. Obtain the target engine release's `go.mod` and verified `go.sum`.
2. Produce a target profile with the final dependency and compatibility policy
   but `publication.mode: manual`. Run the *target* engine binary (built from the
   approved engine candidate checkout, not the old v1 shim which lacks the
   `upgrade` command) with `soapbox upgrade -engine-version tools/v0.2.2
   -engine-mod <path/to/go.mod> -engine-sum <path/to/go.sum>
   -target-config <path/to/manual/soapbox.yaml>`.
3. Review, approve, apply, and publish that exact manifest. The target profile is
   decoded strictly and cannot retarget the existing source, destination,
   release, provenance, or vanity identity. Its exact bytes and preimage bind the
   local compatibility, copy, workflow, and committer policies together with the
   engine migration. Omitting `-target-config` performs only the safe v1-to-v2
   default migration (`manual` publication and `external` compatibility).
4. Mark the default branch protected in a way that permits job-scoped direct
   pushes, then dispatch the manual workflow with no approval and review its
   plan-only result.
5. Repeat upgrade with the final target profile, changing publication to
   `automatic`, and publish that separately approved fast-forward commit.
6. Dispatch the automatic workflow with `verify-write: true`. That path skips
   source discovery and sends the unchanged consumer branch through an atomic,
   leased no-op push. `writeVerified: true` therefore proves the job-scoped
   GITHUB_TOKEN reached receive-pack with write access while every consumer,
   state, and progress ref remained at its expected OID—even when an upstream
   release is pending. App credentials stay until this replacement has been
   live-verified under the outward-action manifest.
7. App secret removal, installation/App deletion, and local key removal belong
   in that same fresh outward-action manifest rather than a manual side step.

The `*.private-key.pem` pattern is tracked in `.gitignore` as a safety net
against accidental commits of downloaded App keys during migration.
