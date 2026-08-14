# Dependency policy

Kubernetes staging modules can be depended on normally. The question this policy
answers is whether any of their packages should be *copied* into the generated
module instead.

The default answer is no. A large module is not by itself a reason to copy code.

## Copying is implemented

The staging copy materializer reads approved packages from the module cache,
relocates them under the internal prefix preserving their full upstream path,
rewrites imports in both copied and retained files, re-tidies the module, and
verifies the post-copy module still type checks.

The `copy-approved` policy requires every correctness gate enabled and every
cost gate answered with a non-zero ceiling (or a floor with a non-zero minimum).
An unmeasured gate is refused, not scored as zero.

## When a copy is allowed

Copying is allowed only for pure leaf utilities with high measured leverage, and
only when every correctness gate passes.

### Correctness gates

These are never overridable. An override naming one is a profile validation
error.

| Gate | What it refuses |
|---|---|
| `interoperability` | A candidate that owns a defined type, interface method type, or function type crossing the generated public boundary. Go type identity is nominal, so a relocated declaration is a different type from the one a consumer already holds. The walk descends through fields, method sets, tuples, maps, channels, slices, arrays, signatures, interfaces, and type parameters; a walk that exhausts its depth bound counts as a finding rather than as a pass. |
| `globalState` | A candidate containing unexported context keys, mutable exported singletons, feature gates, scheme mutations, registry registration, or relevant `init()` side effects. |
| `diamond` | A candidate that would appear both relocated and externally reachable in the same consumer build, or whose package owns a type the generated module must satisfy. |
| `closureCompleteness` | A candidate importing a package of its own module that is not itself a candidate. |

The three booleans in `dependencies.gates` are assertions, not switches. The
gates run whatever those values are; a profile proposing a copy with any of them
`false` is rejected outright.

### Cost gates

These are overridable. Six are ceilings and one is a floor with three
components.

| Gate | Sense | Bound |
|---|---|---|
| `maxCopiedPackages` | ceiling | packages copied |
| `maxCopiedLines` | ceiling | lines copied |
| `maxGeneratedFiles` | ceiling | generated files copied |
| `maxDistinctLicenses` | ceiling | distinct licences taken on |
| `maxModuleZipBytes` | ceiling | module zip bytes avoided |
| `maxReleasesPerMinor` | ceiling | upstream release cadence of the copied code |
| `securityCritical` | ceiling, implicit 0 | candidates on a security-critical path |
| `nativeCode` | ceiling, implicit 0 | cgo and native source files |
| `minimumLeverage` | floor | `minModulesRemoved`, `minPackagesRemoved`, `minLinesRemoved`, all three of which must hold |

Two properties matter more than the individual numbers.

**Cost is measured across the whole accepted copy**, not per candidate. Sizes
accumulate; cadence and removal benefits take the maximum. The aggregate is then
reported against every candidate, so a refusal names the whole proposal rather
than an arbitrary member of it.

**Unmeasured is not zero.** A gate the caller supplied no measurement for is
refused rather than scored as zero: *"the caller supplied no measurement for
this gate, so it is refused rather than scored as zero"*. This applies to
licences, zip bytes, and cadence.

One failed gate of any kind means the dependency stays external. There is no
weighing.

### Overrides

An override relaxes exactly one cost gate for one candidate, and it must carry a
justification, an approver, and a Kubernetes minor expiry. It sets the gate to
passing outright rather than raising the ceiling to a new number, and it is
skipped entirely when the gate is unmeasured.

An expired override fails the run. It does not quietly revert to the unrelaxed
gate:

```text
override <package> gate <gate> approved by <approver> was good through v1.N,
source is v1.M: cost gate override expired
```

An override naming a candidate the resolved graph does not contain also fails,
so an override cannot outlive the thing it was written for.

### What a copy carries

When copying is approved, all files keep their complete upstream relative path
below the internal prefix, including `staging/src/k8s.io/<module>/...`, which
preserves nested Go `internal` restrictions. Provenance records the original
module path, version, source SHA, licence, patent files, and the override that
admitted it.

## The RBAC dependency decisions

### k8s.io/component-helpers: one package copied, module forbidden

The package `k8s.io/component-helpers/auth/rbac/validation` is a pure leaf
utility: it imports only `k8s.io/api/rbac/v1` and the standard library, owns
no types crossing the public boundary, registers no global state, and creates
no diamond. Copying it removes the `k8s.io/component-helpers` module from the
build entirely. The module is then added to `forbiddenModules` so it cannot
re-enter through any path.

The v1.36.1 certification measured one 173-line Go file, zero generated or
native files, one Apache-2.0 grant, a 132,582-byte module zip, and eight v0.36
releases through the v0.36.1 cutoff. The consumer module graph loses exactly
`k8s.io/component-helpers`; the compiled dependency-package count stays 413
because the local copy replaces the external package one-for-one. Two expiring
v1.36 overrides are explicit: `securityCritical` accepts ownership of this RBAC
comparison code under release-bounded regeneration and differential testing,
and `minimumLeverage` accepts a zero line-removal reading from `go/packages`
while the separately measured module and package removals are both one.

The checked-in differential test template compares upstream and copied `Covers`
over focused wildcard, subresource, resource-name, and non-resource URL cases
plus 10,000 deterministic randomized rule pairs. It passed against the exact
v0.36.1 upstream module and the generated copy.

Forbidden module enforcement is five layers deep: raw Go imports in all
retained and copied files (module-boundary exact match, not prefix), parsed
go.mod requirements, parsed go.sum entries, `go list -m all` (indirect
dependencies included, errors fatal), and typed module graph identities.

This supersedes the original zero-copy decision recorded in
[decisions/0001-no-staging-copy-rbac.md](decisions/0001-no-staging-copy-rbac.md),
which was correct at the time: the materializer did not exist. Now that it
does, and the component-helpers package passes every gate, the copy delivers
real module removal with no correctness cost.

### k8s.io/apiserver: mode-dependent identity

Copying `k8s.io/apiserver` remains prohibited in both modes. Under
`compatibility.apiserver: external`, the generated module preserves real
apiserver user, attribute, decision, rule-info, and authorizer identities. The
facade's external interface assertions feed `IdentityRequired`, so dependency
policy keeps the module through the diamond gate.

Under `compatibility.apiserver: local`, compatibility transformation runs before
module and dependency policy. It generates only the declarations retained RBAC
code reads, rewrites those imports, replaces external assertions with local
ones, and changes `ConfirmNoEscalation` to explicit user and namespace inputs.
The profile then forbids `k8s.io/apiserver`; the same five-layer check used for
component-helpers proves it is absent from the published build. This is an
intentional API break rather than a claim that copied interfaces preserve Go
type identity.

The v1.36.1 control pair dropped the loaded module count from 138 in external
mode to 66 in local mode and the compiled dependency-package count from 413 to
335. Full mode semantics, behavior tests, and measurements are in
[apiserver-compatibility.md](apiserver-compatibility.md).

## Public API type preference

A separate policy, but the same posture: substitution is a proof obligation, not
a textual rewrite. `types.policy: prefer-external` first decides whether any
retained reference actually needs substitution, then runs the proofs applicable
to that outcome.

| Analysis | What it proves |
|---|---|
| `markers` | Upstream itself records the pairing: `+k8s:conversion-gen=<internal>` and `+k8s:conversion-gen-external-types=<external>` in the same file of the same package. A shared `groupName` corroborates; differing group names block. |
| `reachability` | Either retained package-scope references are enumerated for rewriting, or the internal package is absent from the retained closure while retained code already imports the configured external package. A retained blank import blocks because it depends on import-time effects a type rewrite cannot preserve. This prevents an empty use set from becoming a vacuous pass. |
| `conversions` | For a real rewrite, generated `Convert_X_To_Y` bodies are mechanical — assignment, cast, `unsafe.Pointer` reinterpretation, a nested conversion, or the error check around one — and every field of the output type is assigned. Finding zero conversions is a blocker, not a pass. |
| `methodSets` | For a real rewrite, every exported method of each paired internal type exists on the external type with the same signature, and every internal symbol retained code names exists externally. Extra external methods are compatible growth, not a blocker. |
| `fieldIdentity` | For a real rewrite, recursive structural equality covers field names, field order, embeddedness, exportedness, `json` and `protobuf` tags, container kinds, array lengths, channel direction, signature arity and variadicity, and interface method sets. Zero comparisons is a vacuous pass and is treated as a blocker. |
| `globalEffects` | Every import-time effect of the internal package is inventoried. A reachable effect blocks; an unreachable one becomes a documented behaviour change. |

Any applicable blocker refuses the change. So does any difference in the
generated public API.

When reachability proves that retained code names no internal symbol, the outcome
is `prune-internal`: no Go value changes type, so conversion bodies, method sets,
and field or serialization identity are explicitly reported as inapplicable
rather than falsely reported as equal. That is the RBAC outcome. Retained code
already uses `k8s.io/api/rbac/v1`; the unversioned internal declarations omit
public wire tags and carry helpers the public types do not, but none of those
declarations is substituted.

## Module composition

The dependency policy runs against a real module graph, which means a
provisional module has to exist first.

1. The source commit's root `go.mod` is parsed. A module is a staging module
   exactly when the root replaces it with a directory under `staging/src`. Every
   replacement must be a staging replacement, a staging module that is also
   required must sit at the `v0.0.0` placeholder, and `exclude` directives are
   refused outright. A staged-but-unrequired module is normal.
2. At a Kubernetes tag `v1.X.Y[-pre]`, staging modules pin to `v0.X.Y[-pre]`.
   The arithmetic result is still put to the Go toolchain, and three things are
   checked: the resolved version equals the computed tag exactly, it was reached
   through a `refs/tags/` ref rather than a branch, and the module was not
   answered as the main module, through a replacement, or at a non-canonical
   version.
3. Between tags, an engine-selected exact source commit maps to each canonical
   staging repository through `Kubernetes-commit` trailers. Both source and
   staging walks are bounded inclusively by the last published release and the
   pending release. The Go toolchain resolves each mapped commit to a
   pseudo-version; no pseudo-version is ever constructed by hand.
4. Mappings are cached in an append-only index keyed by source commit. Every
   checkpoint stores the canonical index as a blob reachable from the state
   commit and records its digest, object name, and entry count. Resume restores
   those exact bytes before resolving more commits; conflicting entries and
   unreachable evidence are refused.
5. The generated `go.mod` is verified by tidying it in a scratch directory: no
   requirement may be raised by minimal version selection, none may be added,
   the module path and the `go`, `toolchain`, and `godebug` directives must be
   unchanged, and there must be zero `replace` and zero `exclude` directives.
   Modules that dropped out and direct/indirect reclassifications are reported
   rather than refused, because the generated module is a subset of Kubernetes.
6. After the facade is installed, a second tidy runs in diff mode and refuses
   any change at all: *"the generated facade needs module requirements the
   tidied go.mod does not state, so the published metadata would not describe
   the published tree"*.
