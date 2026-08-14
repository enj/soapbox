# Configuration reference

`soapbox.yaml` is a versioned, strictly decoded profile. Unknown fields,
duplicate keys, and multiple YAML documents are rejected, so a profile written
for a different engine cannot silently lose meaning. Every value is validated
before any network operation and before anything is written.

Run `soapbox validate` after every edit. `-format canonical` prints the
normalized profile; `-format profile` prints the configuration portion of the
replay profile hash, which is how to see whether a profile edit starts a new
epoch. The released engine version is framed into the hash separately.

Two rules apply everywhere and are not repeated below. Every configured path is
relative, traversal free, and checked after symlink resolution to remain inside
its permitted root. Every URL must be `https`, must carry no user information,
no explicit port, and no fragment, and its host must be on the allowlist for
that field.

## `version`

The schema version. The only accepted value is `2`.

## `source`

| Field | Rules |
|---|---|
| `repository` | Host `github.com`, must end in `.git`. |
| `importPrefix` | The upstream module path, `k8s.io/kubernetes`. Only imports rooted here are eligible for relocation. |
| `project` | The upstream project's display name, rendered verbatim into `LICENSE`, `NOTICE`, `README`, and the root doc comment. |
| `license` | SPDX identifier of the upstream grant. One of `Apache-2.0`, `BSD-3-Clause`, `ISC`, `MIT` — the set whose text the engine can actually read and check. Naming a licence makes a legal claim on the operator's behalf, so the vocabulary is deliberately small. |

### `source.refs`

| Field | Rules |
|---|---|
| `minimumRelease` | The first upstream release, `v1.36.1`. Used as the default ref by `plan`, `generate`, and `sync`, and as the baseline for override expiry. |
| `includePrereleases` | Whether later prerelease tags are tracked. |
| `branches` | Tracked upstream branches. Patch branch selectors must name one of these. |
| `anchorCommit` | The immutable source-history anchor, persisted before automatic operation. New repositories use a common transformed ancestor of every tracked release branch. Legacy state may instead be anchored at `minimumRelease`; in that case discovery is deliberately limited to that release's major/minor line because a later minor may have branched before the patch anchor. |

## `destination`

| Field | Rules |
|---|---|
| `module` | The generated module path, `monis.app/kk/rbac_authorizer`. |
| `repository` | `owner/name` slug. |
| `remote` | Must equal `https://github.com/<repository>.git` exactly. |
| `branch` | The consumer branch. A valid branch name. |
| `stateRef` | Must live under `refs/heads/` and must differ from `branch`. |
| `progressRefPrefix` | Must start with `refs/`, end with `/`, and must not shadow `refs/heads/` or `refs/tags/`. |
| `rootPackage` | The public package name. |
| `internalPrefix` | Where relocated upstream packages live. Must contain an `internal` path element, and may not be `tools` or below it. |
| `summary` | A noun phrase completing "Package `<rootPackage>` provides ...". No analysis of code can say what code is for, so a human writes it. |

## `packages`

| Field | Meaning |
|---|---|
| `roots` | Upstream package paths that seed the closure. Package granularity, not recursive directory copying: `plugin/pkg/auth/authorizer/rbac` does not pull in its sibling `bootstrappolicy`. |
| `recursive` | Whether subdirectories of a root are included. |
| `assetGlobs` | Runtime data that static analysis cannot discover. |

Subpackages enter the closure only when imported or explicitly configured.

## `prune`

| Field | Meaning |
|---|---|
| `files` | Exact files removed before imports are parsed. Never globs, never directories. |
| `required` | Files that must survive pruning. |

An absent prune path fails closed, as does a prune outside the materialized
closure, a prune of a file that is not one of the package's build inputs, and a
prune that would take a package's last Go file. A package that should leave the
closure leaves it by losing its importers.

`required` is checked against the post-prune copy plan rather than against disk.
A required file whose package dropped out of the closure is not retained, and
treating that as success is exactly the silent shrink the check exists to
prevent. An upstream rename therefore fails the run instead of shrinking the
generated module.

## `deny`

`imports` lists exact import paths that may never reenter the closure. The match
is exact: denying `k8s.io/kubernetes/pkg/apis/rbac` does not deny its retained
`/v1` subpackage.

Deny is enforced on the post-prune pass only. Running it before pruning would
reject the very profile pruning exists to express, where a pruned file is the
sole importer of a denied package.

## `closure`

| Field | Meaning |
|---|---|
| `includeTests` | Whether `_test.go` files enter the closure. |
| `limits.maxPackages` | Ceiling on post-prune package count. |
| `limits.maxFiles` | Ceiling on post-prune file count. |
| `limits.maxNonTestLines` | Ceiling on post-prune non-test lines. |
| `limits.maxPackageGrowth` | Ceiling on packages beyond the configured roots, not an absolute count. |
| `golden` | Path to the measured closure report the run is compared against. |

A limit of `0` disables that check. Limits are evaluated in a fixed order, so a
tree over several ceilings always fails the same way.

These are observational publication gates. They never change generated bytes,
which is why they stay out of the replay profile hash and why a golden may
compare measurements with tolerance while comparing the exact shape literally.

## `types`

| Field | Rules |
|---|---|
| `policy` | `prefer-external` or `keep-internal`. |
| `pairs[].internal` | An upstream internal API package. |
| `pairs[].external` | The public package that replaces it. |

`prefer-external` is a proof obligation, not a textual rewrite. See
[dependency-policy.md](dependency-policy.md#public-api-type-preference) for the
reachability check and the proofs that apply to an actual rewrite versus an
already-unreachable internal package.

## `dependencies`

| Field | Rules |
|---|---|
| `policy` | `external` or `copy-approved`. |
| `copyPackages` | Staging packages proposed for copying. Empty under `external`. |
| `forbiddenModules` | Module paths the generated module must never depend on. Enforced across five surfaces: raw Go imports (module-boundary exact match), go.mod, go.sum, `go list -m all`, and typed module graph identities. Feeds the profile hash. |
| `gates.interoperability` | Correctness gate. |
| `gates.globalState` | Correctness gate. |
| `gates.diamond` | Correctness gate. |
| `gates.cost.*` | Nine cost gates. See below. |
| `overrides` | Relaxations of exactly one cost gate for one candidate. |

The three correctness booleans are assertions, not switches. The gates run
unconditionally; a profile proposing a copy with any of them `false` is
rejected.

### Cost gates

Ceilings: `maxCopiedPackages`, `maxCopiedLines`, `maxGeneratedFiles`,
`maxDistinctLicenses`, `maxModuleZipBytes`, `maxReleasesPerMinor`.

Floors: `minModulesRemoved`, `minPackagesRemoved`, `minLinesRemoved`. These ask
what the copy is for. The usual outcome of copying some packages of a module
that stays in the build for the others is that nothing leaves at all, and a
profile states the benefit it expects so a copy that does not deliver it is
refused however cheap it looks.

No value may be negative. A ceiling of `0` admits nothing rather than admitting
everything.

### Overrides

| Field | Rules |
|---|---|
| `package` | Must be listed in `copyPackages`. |
| `gate` | One of `maxCopiedLines`, `maxCopiedPackages`, `maxDistinctLicenses`, `maxGeneratedFiles`, `maxModuleZipBytes`, `maxReleasesPerMinor`, `minimumLeverage`. A correctness gate name is rejected. |
| `justification` | Required, non-blank. |
| `approver` | Required, non-blank. |
| `expiresAfter` | A Kubernetes minor series, `vMAJOR.MINOR`, e.g. `v1.40`. Must be a v1 minor and must not already have expired at `source.refs.minimumRelease`. |

`minimumLeverage` is one name for all three minimum removal floors, because a
copy either delivers the benefit the profile asked for or it does not, and
relaxing one of the three alone would approve a copy on a benefit nobody stated.

An override that names no candidate in the resolved graph fails the run, as does
one that has expired relative to the source minor. An expired override does not
quietly revert to the unrelaxed gate.

## `patches`

Ordered unified diffs applied before relocation and import rewriting.

| Field | Rules |
|---|---|
| `file` | Must live under `patches/` and end in `.patch` or `.diff`. |
| `since` | Ancestry selector: an object name, a tag such as `v1.36.1`, or a fully qualified ref. |
| `until` | Same. Must differ from `since`, which would otherwise select an empty range. |
| `branches` | Each must be a tracked `source.refs.branches` entry. |

A tag run derives the branch a patch selector is matched against from upstream's
own convention, `v1.36.1` from `release-1.36`, and only when the profile
actually tracks the branch that produces. A profile that carries patches and
tracks several branches has no defensible default, so `-patch-branch` becomes
required.

## `facade`

| Field | Rules |
|---|---|
| `package` | Must match `destination.rootPackage`. |
| `file` | The generated facade file. |
| `assertionsFile` | The generated assertions file. Must differ from `file`. It is written even when no assertion is specified, so the published tree has one shape regardless of the profile. |
| `exports[]` | `name`, `kind`, `source`. |
| `aliases[]` | Same fields, for symbols republished under a different name. |
| `interfaceAssertions[]` | `type`, `pointer`, `interface`. |

`kind` is one of `func`, `type`, `interface`, `const`. Exported variables are
deliberately absent: forwarding one would create a second mutable global that
any importer could change for every other importer in the process.

`source` must name a symbol in a relocated source package. External types keep
their upstream identity and are never redeclared. An export must keep its
upstream name; renaming requires an alias, and an alias that does not rename is
rejected. This is how the four lister-backed adapters that collide with the
validation interfaces of the same name become `RoleGetterFromLister` and its
siblings.

An interface assertion's `type` must be a concrete exported type, and its
`interface` is the real upstream interface path — an assertion against a copy of
an interface would prove nothing about the real one.

## `release`

| Field | Rules |
|---|---|
| `policy` | Only `v1-to-v0` is implemented. |
| `firstTag` | The first generated tag, `v0.36.1`. |

`v1-to-v0` requires a major of `1` and sets it to `0`, carrying minor, patch,
and prerelease through unchanged: `v1.36.1` becomes `v0.36.1`, and
`v1.37.0-alpha.1` becomes `v0.37.0-alpha.1`. Build metadata is rejected.

## `commit`

| Field | Rules |
|---|---|
| `authorPolicy` | Only `preserve-upstream`. |
| `committer.name` / `committer.email` | The bot identity that commits generated history. |
| `trailerKey` | The provenance trailer key, `Kubernetes-commit`. |
| `sign` | Must be `false`. Generated commits are never signed. |

## `vanity`

| Field | Rules |
|---|---|
| `repository` | `owner/name` slug of the site repository. |
| `path` | Must end with `/index.html`. |
| `importPath` | Must equal `destination.module`. |
| `repositoryURL` | Must equal `https://github.com/<destination.repository>`, with no `.git` suffix. |
| `probeURL` | Must equal `https://<destination.module>?go-get=1`. Its host is the module domain, not `github.com`. |

These fields are validated and nothing else reads them. Vanity page generation
is not implemented — see [vanity.md](vanity.md).

## `publication`

| Field | Rules |
|---|---|
| `mode` | Either `manual` or `automatic`. Manual plans when the dispatch `approve` input is empty and applies only when that environment-supplied hash exactly matches a deterministic replan. Automatic adds `-unattended` so trusted schedule/dispatch runs self-apply each in-memory plan. |

Publication mode is operational and does not feed the profile hash.

## `compatibility`

| Field | Rules |
|---|---|
| `apiserver` | Either `external` or `local`. `external` preserves real `k8s.io/apiserver` type identities and keeps the module in the dependency graph. `local` generates Soapbox-local equivalents that intentionally break that identity and remove the `k8s.io/apiserver` module dependency. |

Compatibility mode feeds the profile hash because it determines which
dependencies are acceptable and changes the generated module graph. In `local`
mode Soapbox rewrites retained apiserver imports, exports generated local types,
replaces external interface assertions, and gives `ConfirmNoEscalation` explicit
user and namespace inputs. Profiles selecting that mode should forbid
`k8s.io/apiserver`; the RBAC profile also forbids the independently copied
`k8s.io/component-helpers` module. See
[apiserver-compatibility.md](apiserver-compatibility.md) for the exact boundary
and certification evidence.

## `determinism`

| Field | Rules |
|---|---|
| `toolchain` | An exact patch pin, `goX.Y.Z`. Must equal the engine's own toolchain. Feeds the profile hash. |
| `chunkSize` | Must be positive. Bounds each resumable relevant-DAG replay checkpoint. Operational only; does not feed generated bytes or the profile hash. |

## What feeds the profile hash

A change to any of these starts a new profile epoch:

```text
released engine version, profile version, source.repository,
source.importPrefix, source.project, source.license, source.refs.anchorCommit,
destination.module,
destination.rootPackage, destination.internalPrefix, destination.summary,
packages, prune, deny, closure.includeTests, types, dependencies,
patches, facade, release, commit, compatibility.apiserver,
determinism.toolchain
```

Deliberately excluded, because they cannot change a generated byte:

```text
ref discovery and publication layout, discovered branches,
determinism.chunkSize, publication.mode, vanity, closure limits and golden
```

See [determinism.md](determinism.md) and [replay-model.md](replay-model.md).
