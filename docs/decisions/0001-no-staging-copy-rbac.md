# 0001. No staging copy for the RBAC authorizer

**Status:** partially superseded (2026-08-13)
**Applies to:** `monis.app/kk/rbac_authorizer`, from Kubernetes `v1.36.1`
**Profile:** `dependencies.policy: copy-approved`, one package copied

## Original decision (accepted, still holds for apiserver)

The original decision to keep `k8s.io/apiserver` and `k8s.io/component-base`
external remains in force. The five-package apiserver copy proposal was refused
by correctness gates (interoperability, diamond, globalState) even with all cost
overrides applied. See the detailed gate refusals below, which are unchanged.

## Context

`monis.app/kk/rbac_authorizer` depends on `k8s.io/apiserver`, which depends on
`k8s.io/component-base`. Both are Kubernetes staging modules. A recurring
temptation when publishing a small module is to copy the handful of packages it
actually uses and drop the dependency, so the question was put to the dependency
policy as a real proposal rather than dismissed.

The proposal was five packages:

```text
k8s.io/apiserver/pkg/authentication/user
k8s.io/apiserver/pkg/authorization/authorizer
k8s.io/apiserver/pkg/endpoints/request
k8s.io/apiserver/pkg/features
k8s.io/component-base/featuregate
```

## What the dependency costs

Measured against `v0.36.1`:

| Measurement | Value |
|---|---|
| `k8s.io/apiserver` module zip | about 2.83 MB, downloaded once |
| `k8s.io/apiserver` packages compiled | 6 of 246 |
| `k8s.io/component-base` module zip | about 1.12 MB |
| Upstream release cadence | 9 releases per minor series, both modules |
| Lines the copy would own | 143 across the five packages |
| Modules the copy would remove | 2 |
| Packages the copy would remove | 1 |
| Lines the copy would remove | 120 |

The whole benefit is one retained importer,
`k8s.io/apiserver/pkg/authorization/authorizerfactory`, at 120 lines.

## Decision

Keep both modules external. Copy nothing.

The proposal was run through the policy with cost overrides applied to
`maxCopiedLines` and `maxCopiedPackages` for every candidate, approved and
unexpired. Every candidate was still refused, and every refusal came from a
correctness gate. All nine cost gates passed, because with correctness refusing
every candidate the accepted set is empty and there is nothing to cost.

Relaxing the numbers cannot admit this copy. That is the point of running it
with the overrides in place.

### `k8s.io/apiserver/pkg/authentication/user` — interoperability, diamond

`user.Info` is reached from the generated public API:

> `k8s.io/apiserver/pkg/authentication/user.Info` (interface) reached by
> `monis.app/kk/rbac_authorizer.AuthorizationRuleResolver` → method `RulesFor`
> → parameter `subject` → `k8s.io/apiserver/pkg/authentication/user.Info`; a
> relocated interface is a distinct type, so a consumer holding the real one
> cannot pass it across the boundary.

And the module cannot leave the build anyway:

> `k8s.io/apiserver/pkg/authorization/authorizerfactory` imports
> `k8s.io/apiserver/pkg/authentication/user`; the retained package still imports
> the original, so the consumer build contains both the copy and the package it
> was copied from.

### `k8s.io/apiserver/pkg/authorization/authorizer` — interoperability, diamond

This is the interface the module exists to implement:

> `authorizer.Attributes` (interface) reached by
> `monis.app/kk/rbac_authorizer.New` → result 0 → `*` →
> `monis.app/kk/rbac_authorizer.RBACAuthorizer` → method `Authorize` →
> parameter `attrs` → `authorizer.Attributes`; the transitive method set reaches
> candidate owned type `k8s.io/apiserver/pkg/authentication/user.Info`, which
> would be relocated with it.

> `authorizer.Decision` (basic) … → method `Authorize` → result 0 →
> `authorizer.Decision`; Go type identity is nominal, so a relocated declaration
> is a different type from the one a consumer already holds.

> the generated module must satisfy `Authorizer` from this package, so the
> original cannot leave the build.

Copying this package would produce a module whose `Authorize` method cannot be
handed to any real apiserver.

### `k8s.io/apiserver/pkg/endpoints/request` — global state

> `deniedPath: k8s.io/apiserver/pkg/endpoints/request`; the package owns the
> request context keys, whose identity is the type and does not survive
> relocation.

Plus the individual keys: `requestInfoKeyType`, `userKeyType`, and two further
`context.WithValue` findings. A relocated context key is a different key. A
handler chain that stored a value under the real key would look empty to code
reading through the copy, silently.

### `k8s.io/apiserver/pkg/features` — global state

> `deniedPath: k8s.io/apiserver/pkg/features`; the package registers the
> apiserver feature gates into a process global gate at initialisation.

> `mutableSingleton: …features.DefaultMutableFeatureGate`; an exported package
> variable is shared state, and a second copy of it never sees the writes made
> to the first.

Plus an `init` that calls `featuregate.Add` at import time. A process that
enabled a gate through the real singleton would find the copy still disabled.

### `k8s.io/component-base/featuregate` — global state

> `deniedPath: k8s.io/component-base/featuregate`; the package defines the
> feature gate machinery whose instances are process global.

## Consequences

The generated module requires `k8s.io/apiserver` and, transitively,
`k8s.io/component-base`. That is the intended outcome:

1. **Type identity is real.** `RBACAuthorizer` implements the actual
   `authorizer.Authorizer` and `authorizer.RuleResolver`, and
   `zz_generated_assertions.go` asserts it at compile time against the real
   interfaces from their own module.
2. **Request context keys are the real ones.** A consumer's handler chain and
   this module agree about what is in a request context.
3. **Feature gates are the process's own.** No second copy of a global to keep
   in sync.
4. **CVE identity is preserved.** A vulnerability in `k8s.io/apiserver` is
   reported against `k8s.io/apiserver`, and a consumer's scanner sees the module
   in the graph. A copy would make the same code invisible to that tooling.
5. **Staging version coherence is preserved.** The module pins staging
   dependencies at the versions upstream tested together at that release.

The cost is a 2.83 MB download and 6 compiled packages. That is the correct
price.

## Revisiting

This decision is about the *reachable* API of this module, not about
`k8s.io/apiserver` in general. It would have to be revisited if the generated
public API stopped exposing apiserver types entirely, which for an authorizer
would mean it had stopped being an authorizer.

The refusal is pinned by a policy fixture and a golden decision record, so a
future profile that proposes this copy fails a test rather than merely
disagreeing with a document.

## Follow-on: k8s.io/component-helpers (2026-08-13)

The profile now copies one pure-leaf package from `k8s.io/component-helpers`:

```text
staging/src/k8s.io/component-helpers/auth/rbac/validation
```

This package imports only `k8s.io/api/rbac/v1` and the standard library. It
owns no types crossing the generated public boundary, registers no global
state, and creates no diamond. At v0.36.1 it is one 173-line Go file in a
132,582-byte module zip, under one Apache-2.0 grant; release-bounded cadence is
eight versions through v0.36.1. Copying it removes exactly the
`k8s.io/component-helpers` module while replacing its one compiled package
one-for-one.

The path-based `securityCritical` gate is explicitly overridden through v1.36:
this is RBAC comparison code, so Soapbox accepts CVE-response ownership only
with release-bounded regeneration and a differential test against the exact
upstream module. A second v1.36 override records that `go/packages` reports zero
external dependency lines even though measured module and package removals are
both one; the independent 200-line copy ceiling still applies. Focused cases
and 10,000 deterministic randomized rule pairs produced byte-identical `Covers`
results and uncovered-rule lists.

The module is then added to `forbiddenModules`, which is enforced across five
surfaces: raw Go imports (module-boundary exact match), go.mod, go.sum,
`go list -m all`, and typed module graph identities.

This does not weaken the apiserver decision. The five apiserver packages are
still refused by correctness gates. The component-helpers copy is a separate,
narrower proposal that passes every gate because the package is a pure leaf
utility with no API types, no global state, and no diamond path.
