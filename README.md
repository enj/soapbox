# Soapbox

Soapbox is a Go GitHub template and deterministic generator for publishing selected packages from [`kubernetes/kubernetes`](https://github.com/kubernetes/kubernetes) as independently consumable Go modules.

A generated repository can:

1. compute and bound the production package closure;
2. apply exact pruning and ref-scoped patches;
3. prefer public `k8s.io/api` types only after equivalence proofs;
4. relocate retained source beneath `internal/kk` while preserving paths;
5. generate a curated, type-checked public facade and provenance files;
6. map Kubernetes staging modules to exact published versions;
7. replay relevant upstream DAG history with `Kubernetes-commit` trailers; and
8. plan append-only, compare-and-swap publication behind an approval hash.

The first target is `monis.app/kk/rbac_authorizer`, sourced from `plugin/pkg/auth/authorizer/rbac` at Kubernetes `v1.36.1` and mapped to module tag `v0.36.1`.

## Status

The deterministic engine is implemented, and `monis.app/kk/rbac_authorizer@v0.36.1` is published and immutable. The follow-on engine now discovers HTTPS destination refs and pending releases, maps release-bounded exact commits to Go-resolved staging pseudo-versions, replays relevant DAG history in resumable chunks, grafts profile epochs, and reconciles append-only publication with the repository-scoped `GITHUB_TOKEN`. Current migration changes remain local until a new exact outward-action manifest is reviewed and approved.

The durable requirements and approved design are in [`plans/`](plans/).

Known limitations are listed in [docs/setup.md](docs/setup.md#current-limitations). The public `generate` command still accepts reviewed release tags rather than moving branch names; exact-commit generation is an engine-only surface bounded by discovered releases. Remote repository creation, vanity changes, engine tags, and module tags remain outside the unattended engine and require a separately approved outward-action manifest.

## Architecture

`enj/soapbox` is both a GitHub template and the source of a versioned Go engine.

1. The template has no root `go.mod`. The engine is the nested module
   `github.com/enj/soapbox/tools`.
2. `tools/soapbox.go` is the entire public surface. Everything else lives under
   `tools/internal/`, so the engine can evolve without changing the contract a
   derived repository compiles against.
3. `soapbox setup` creates the derived repository's root module, replaces the
   copied engine with a small nested `tools` module and command shim, and pins
   that shim plus its indirect graph roots to an immutable `tools/vX.Y.Z`
   release. Tool dependencies never enter the generated library's module graph.
4. Generated source, `soapbox.yaml`, patches, the shim, and workflows coexist on
   the default branch. Replay commits modify only generated paths. Configuration
   or engine changes form explicit profile epochs and never rewrite published
   history.

```text
soapbox/
├── soapbox.yaml            the extraction profile
├── plans/                  the durable goal and approved design
├── docs/                   this documentation
├── patches/                ordered unified diffs
├── .github/workflows/      ci.yml, template-selftest.yml
└── tools/                  the engine module
    ├── soapbox.go          the public entry point
    ├── cmd/soapbox/        the command
    └── internal/           config, gitcli, source, closure, patchset,
                            relocate, rewrite, gomodmap, modgen, deppolicy,
                            typeswap, facade, provenance, treebuild, replay,
                            release, publish, state, ghapi, extract,
                            generate, sync, setup, upgrade, doctor, cli
```

A derived repository keeps its facade, assertions, `internal/kk/<upstream
paths>/`, `soapbox.yaml`, `patches/`, the nested shim, and two generated
workflows. `refs/heads/soapbox-state` carries resumable state without entering
the module tree.

## Commands

Run the nested engine from `tools/`:

```text
go run ./cmd/soapbox validate -dir ..
go run ./cmd/soapbox doctor -dir ..
go run ./cmd/soapbox plan -dir .. -tag v1.36.1
go run ./cmd/soapbox generate -dir .. -cache /absolute/cache -tag v1.36.1
go run ./cmd/soapbox setup -dir .. -engine-version tools/v0.2.1 -engine-sum ...
go run ./cmd/soapbox upgrade -engine-version tools/v0.2.1 -engine-mod ... -engine-sum ... -target-config ../soapbox.yaml
go run ./cmd/soapbox sync ...
```

`plan`, `generate`, `setup`, `upgrade`, and `sync` are dry-run oriented. Operations that write an approved local transformation or move refs require the exact manifest hash produced by the corresponding plan. Generation supports exact release tags and already-fetched, release-bounded commit OIDs; intermediate staging versions are resolved by Go from bounded publishing history, and approved staging copies are materialized only after their policy gates pass.

Exit codes are part of the workflow contract and are stable: `0` success, `1`
runtime failure, `2` usage, `3` the command ran and found policy violations, and
`4` canceled. Stdout carries the machine-readable artifact and nothing else;
every diagnostic goes to stderr. A refused run still writes its report before
reporting the failure, so a finding is always reviewable. Full flag
documentation is in [docs/setup.md](docs/setup.md#the-commands).

## The first profile

`soapbox.yaml` extracts the Kubernetes RBAC authorizer:

1. source package `plugin/pkg/auth/authorizer/rbac` at `v1.36.1`, at package
   granularity, which excludes the sibling package `bootstrappolicy`;
2. eight exact prune files that reduce a four-package, 3,289-line internal
   closure to three packages and about 978 lines, and drop
   `k8s.io/kubernetes/pkg/apis/rbac` entirely;
3. one denied import, the exact unversioned `pkg/apis/rbac`, leaving its
   retained `/v1` helper subpackage in place;
4. type policy `prefer-external`, which for RBAC prunes rather than rewrites,
   because the retained code already uses `k8s.io/api/rbac/v1`; this is a
   reachability proof, not a claim that the intentionally different internal and
   public declarations have identical tags or methods;
5. dependency policy `copy-approved`: one pure-leaf package copied from
   `k8s.io/component-helpers/auth/rbac/validation`, removing and then forbidding
   the `k8s.io/component-helpers` module; and
6. `compatibility.apiserver: local`: generated module-local authentication and
   authorization declarations, explicit no-escalation identity inputs, 34 facade
   entries, and a forbidden `k8s.io/apiserver` module.

The already-published `v0.36.1` artifact remains external and immutable. The
local profile applies only to the next eligible release, which begins a new
profile epoch without moving or regenerating that tag. The two modes and their
control-pair measurements are documented in
[docs/apiserver-compatibility.md](docs/apiserver-compatibility.md).

Pruning the registration, conversion, and defaulting files stops import-time
mutation of the `k8s.io/api/rbac/v1` scheme builder. That is an intentional
behaviour change, and it is recorded as one in
[docs/behavior-changes.md](docs/behavior-changes.md).

## Documentation

- [Completed RBAC v1.36.1 local proof](docs/rbac-v1.36.1-proof.md)
- [Setup and derived repositories](docs/setup.md)
- [Configuration reference](docs/config-reference.md)
- [Replay and profile epochs](docs/replay-model.md)
- [Determinism model](docs/determinism.md)
- [Provenance and licence evidence](docs/provenance.md)
- [Intentional RBAC behavior changes](docs/behavior-changes.md)
- [Dependency copy policy](docs/dependency-policy.md)
- [Apiserver compatibility modes](docs/apiserver-compatibility.md)
- [GitHub token and publishing](docs/github-token.md)
- [Vanity import bootstrap](docs/vanity.md)
- [Conflict and recovery runbook](docs/conflict-runbook.md)
- [Original RBAC staging-copy decision and follow-on](docs/decisions/0001-no-staging-copy-rbac.md)

## Development checks

From `tools/`, use the writable caches documented in [`CLAUDE.md`](CLAUDE.md), then run:

```text
gofmt and goimports with no diff
go vet ./...
go test ./...
go test -race ./...
go build ./...
golangci-lint run
```

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs exactly these.
[`.github/workflows/template-selftest.yml`](.github/workflows/template-selftest.yml)
additionally plans the template transformation against the real checkout and
exercises setup, generation, and publication against real temporary Git
repositories. Neither workflow holds a credential, neither can write, and both
pin every action to a full commit object name.

## Hard boundaries

All maintained executable logic is Go. The engine invokes installed `git` and `go` executables only through typed subprocess boundaries; it contains no shell command construction.

GitHub credentials are the built-in `GITHUB_TOKEN` provided by GitHub Actions, scoped to the repository and expired at job end. Credentials never appear in command arguments, remote URLs, reports, or artifacts.

Remote creation, vanity metadata changes, and immutable module publication require a separately reviewed outward-action manifest and a fresh approval of its hash.

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
