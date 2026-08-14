# Behavior changes

A generated module is not a copy of upstream. Files are removed, and removing a
file that ran code at import time changes what the remaining code does while
leaving every remaining line identical. That is invisible in a diff, so it is
written down.

This document records the changes the RBAC profile makes and how the engine
keeps the record honest.

## How a behavior change is found

The type policy analysis inventories the import-time effects of a package before
it is pruned. Five kinds of effect are detected:

| Effect | How it is found |
|---|---|
| Scheme registration | A package-scope symbol named `AddToScheme`, `SchemeBuilder`, `SchemeGroupVersion`, or `localSchemeBuilder`. Matched by name because the Kubernetes API packages all declare their own, so there is no single upstream package to match against. |
| `init` functions | Any `init` in the package. |
| Package-level initializers | A package-level variable whose initializer calls something. `var _ = registerTypes()` and `var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)` are invisible to a scope-name scan, which is why this is a separate pass. |
| Mutable exported globals | Every exported package-scope variable except the predeclared `error` sentinel case. |
| Dangling generator directives | A retained file still carrying a directive that names the pruned package. |

Each effect is then classified. An effect that retained code can reach through
the generated public API is a **blocker**: the substitution is refused. An
effect that nothing reachable can observe is a **documented behavior change**:
the run proceeds and the change is recorded.

Observable effects are converted into records anyway, so a profile that
overrode a blocker cannot also omit the disclosure.

## How the record is kept honest

Behavior changes are derived from the analysis, not authored in the profile.
There is no `behaviorChanges` key in `soapbox.yaml`.

Each record renders as three parts:

```text
summary: <Symbol> no longer performs its <kind> effect at import time.
cause:   type policy <action> for <internal package> paired with <external package>
detail:  <what the analysis found>
         It is not reachable through the generated public API.
```

The file and line the effect was detected at are deliberately excluded from the
summary, because they name a checkout the published record must not depend on.

Before the module is written, the rendered `NOTICE` is checked against the
analysis. Every effect the analysis found must appear in the notice. A profile
that ran the analysis, acted on its decision, and rendered a notice without the
effects that decision removed would have published a module whose documented
behaviour is not its behaviour, and nothing else in the pipeline would notice.

## The RBAC profile

### What is pruned

Eight exact files:

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

The pre-prune internal closure is four packages and 3,289 non-test lines. After
pruning it is three packages and about 978 lines:

```text
plugin/pkg/auth/authorizer/rbac
pkg/registry/rbac/validation
pkg/apis/rbac/v1        only doc.go and evaluation_helpers.go
```

`pkg/apis/rbac` disappears entirely. The deny rule matches the exact unversioned
import `k8s.io/kubernetes/pkg/apis/rbac`; its retained `/v1` helper subpackage is
not denied.

### Why this is safe

The retained authorizer and validation code already uses `k8s.io/api/rbac/v1`
types. No retained type is rewritten — the internal package is pruned rather
than substituted, and the whole argument is that no consumer can tell.

That argument is checked rather than asserted. The facade is generated from both
the pre-prune and the post-prune tree, and any difference at all in the public
API manifest refuses the run. The manifest compares names, kinds, targets,
types, underlying types, constant values, struct fields, and interface method
sets. It deliberately does not compare parameter names, because an upstream
parameter rename changes nothing a consumer can call and reporting it would fail
the comparison for a change that is not one.

`evaluation_helpers.go` is retained because its matcher functions and
`CompactString` do not exist in staging.

### The change: scheme registration

Removing the registration, conversion, and defaulting files stops import-time
mutation of the `k8s.io/api/rbac/v1` scheme builder.

Upstream, importing `k8s.io/kubernetes/pkg/apis/rbac/v1` registers the RBAC
types into a scheme as a side effect of the import. In the generated module that
no longer happens, because the files that did it are gone.

The recorded effects are the package's `SchemeBuilder`, its `AddToScheme`, and
its `init` function. The analysis confirms none of them is reachable through the
generated public API: the facade exposes the authorizer, the rule resolver, the
lister-backed adapters, and the rule evaluation helpers, none of which consults a
scheme.

**What this means for a consumer.** A program that imported the upstream
internal package and relied on that import to populate a scheme must register
the RBAC types itself. A program that uses this module's public API to authorize
requests is unaffected — authorization does not go through a scheme.

### The change: dangling generator directives

Retained `pkg/apis/rbac/v1/doc.go` carries generator directives that name
packages the profile pruned. A directive pointing at code that is no longer
there is not documentation, so it is stripped, and every stripped directive is
recorded.

Stripping follows the same exact-key allowlist the rewriter uses, so the
analysis and the rewrite cannot disagree about what a directive is or whether it
is removed:

| Directive | Outcome |
|---|---|
| `+k8s:conversion-gen` | rewritten, removed when dangling |
| `+k8s:defaulter-gen` | kept, removed when dangling |
| `+k8s:defaulter-gen-input` | rewritten, removed when dangling |
| `+k8s:deepcopy-gen` | kept, removed when dangling |
| `+k8s:validation-gen` / `-input` | kept / rewritten, removed when dangling |
| `+k8s:protobuf-gen` | rewritten, removed when dangling |
| `+k8s:openapi-gen` | kept, removed when dangling |
| `+k8s:conversion-gen-external-types` | kept, never removed |
| `+groupName`, `+groupGoName` | kept |
| `go:generate` | removed |

The external-types marker is kept even when it looks dangling, because it points
at a package that is never relocated. `+groupName` is kept explicitly rather
than by fallthrough, because keeping it is a decision. Package documentation is
otherwise untouched.

For the RBAC profile the stripped directives in `doc.go` are
`+k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac`, `+k8s:deepcopy-gen=package`,
and `+k8s:defaulter-gen=TypeMeta`; `+k8s:defaulter-gen-input` is rewritten to the
relocated path; `+k8s:conversion-gen-external-types=k8s.io/api/rbac/v1` and
`+groupName=rbac.authorization.k8s.io` are unchanged.

### The change: `go:generate` removal

`go:generate` directives are removed from every relocated file, because the
generated module contains neither the generators nor the build harness the
command names. A directive that appears after code on the same line is left
alone; only a directive that owns its line is removed.

### The change: local apiserver compatibility

`compatibility.apiserver: local` deliberately changes the boundary rather than
pretending a copied declaration has the identity of an apiserver declaration.
The generated user, authorization, decision, and rule-info types are
module-local and are not assignable to `k8s.io/apiserver` types. Facade exports
and compile-time assertions point at those local declarations.

The mode also removes dependence on apiserver's private request-context keys.
`ConfirmNoEscalation` receives the authenticated user and namespace explicitly;
it does not call `request.UserFrom` or `request.NamespaceFrom`, and Soapbox does
not reproduce incompatible private context keys. The incompatible
internal-version adapter is pruned.

Both changes are engine-derived provenance records whose cause is
`compatibility.apiserver: local`, so they appear in the JSON report and
`NOTICE` without an author-maintained behavior list. The checked-in local and
external certification tests apply the same authorization, rule-resolution,
subject-location, service-account, and escalation cases to the two generated
control trees. See [apiserver-compatibility.md](apiserver-compatibility.md).

## What is not a behavior change

Relocation is not. Import rewriting is not. Both preserve the semantics of the
code, and the per-file notice records them as modifications rather than as
behaviour changes.

The facade itself is not, either. It aliases internal types and interfaces so a
caller's implementation satisfies the selected contract, and forwards functions
with real declarations. In external mode, external module types keep their
upstream identity and an assertion against
`k8s.io/apiserver/pkg/authorization/authorizer.Authorizer` is about the real
interface. In local mode, the assertion names the generated local interface and
the identity break is recorded by the compatibility change above, not hidden by
the facade.
