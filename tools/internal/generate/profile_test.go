package generate_test

// The fixture profile is a complete, valid extraction profile written against
// the miniature upstream repository this package builds. It is deliberately not
// the repository's own soapbox.yaml: a test that read the real profile would
// fail whenever the real profile changed for reasons that have nothing to do
// with the generation engine, and it could not describe the small upstream tree
// these tests can build in a temporary directory.
//
// It mirrors the real profile's shape exactly where the shape is what is being
// exercised: an external dependency policy with no copies, a prefer-external
// type policy with one pairing, a prune list that removes the file registering
// the internal API, a deny list naming the internal API package, and a curated
// facade with an interface assertion against a real external interface.
const fixtureProfile = `
version: 2

source:
  repository: REPOSITORY
  importPrefix: k8s.io/kubernetes
  project: Kubernetes
  license: Apache-2.0
  refs:
    minimumRelease: v1.36.1
    includePrereleases: true
    branches:
      - master
    anchorCommit: ""

destination:
  module: monis.app/kk/rbac_authorizer
  repository: enj/rbac_authorizer
  remote: https://github.com/enj/rbac_authorizer.git
  branch: main
  stateRef: refs/heads/soapbox-state
  progressRefPrefix: refs/soapbox/progress/
  rootPackage: rbacauthorizer
  internalPrefix: internal/kk
  summary: the Kubernetes RBAC authorizer as an independently consumable Go module.

packages:
  roots:
    - plugin/pkg/auth/authorizer/rbac
  recursive: false
  assetGlobs: []

prune:
  # Removing the registration file drops the unversioned internal API package
  # from the closure entirely, which is what the type policy is asked to prove
  # is safe.
  files:
    - pkg/apis/rbac/v1/register.go
    - pkg/apis/rbac/v1/zz_generated.conversion.go
  required:
    - pkg/apis/rbac/v1/doc.go
    - pkg/registry/rbac/validation/rule.go
    - plugin/pkg/auth/authorizer/rbac/rbac.go

deny:
  imports:
    - k8s.io/kubernetes/pkg/apis/rbac

closure:
  includeTests: false
  # The limits admit the UNPRUNED closure, not just the pruned one. The baseline
  # pass runs the same limits against a strictly larger package set, so a profile
  # whose ceilings only fit the pruned result refuses before it ever reaches the
  # facade comparison. Growth is 4 here: one root reaches validation, the
  # versioned API helpers, the component-helpers validation package, and the
  # unversioned API package the prune removes.
  limits:
    maxPackages: 10
    maxFiles: 40
    maxNonTestLines: 5000
    maxPackageGrowth: 5
  # The golden is deliberately not written by these tests. An absent golden is
  # an advisory notice rather than a refusal, which is exactly the signal the
  # strict mode test needs: a notice that has to stop the run before any output
  # is written.
  golden: testdata/closure/fixture.json

types:
  policy: prefer-external
  pairs:
    - internal: k8s.io/kubernetes/pkg/apis/rbac
      external: soapbox.test/api/rbac/v1

dependencies:
  policy: external
  copyPackages: []
  forbiddenModules: []
  gates:
    interoperability: true
    globalState: true
    diamond: true
    cost:
      maxCopiedPackages: 0
      maxCopiedLines: 0
      maxGeneratedFiles: 0
      maxDistinctLicenses: 0
      maxModuleZipBytes: 0
      maxReleasesPerMinor: 0
      minModulesRemoved: 0
      minPackagesRemoved: 0
      minLinesRemoved: 0
  overrides: []

patches: []

facade:
  package: rbacauthorizer
  file: authorizer.go
  assertionsFile: zz_generated_assertions.go
  exports:
    - name: New
      kind: func
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.New
    - name: RBACAuthorizer
      kind: type
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RBACAuthorizer
    - name: AuthorizationRuleResolver
      kind: interface
      source: k8s.io/kubernetes/pkg/registry/rbac/validation.AuthorizationRuleResolver
    - name: NewDefaultRuleResolver
      kind: func
      source: k8s.io/kubernetes/pkg/registry/rbac/validation.NewDefaultRuleResolver
    - name: DefaultRuleResolver
      kind: type
      source: k8s.io/kubernetes/pkg/registry/rbac/validation.DefaultRuleResolver
    # RoleGetter is reachable from NewDefaultRuleResolver's signature. The
    # generator refuses any named type of the generated module that the public
    # API spells but a consumer cannot name, so leaving it out would be a
    # facade that does not compile against itself.
    - name: RoleGetter
      kind: interface
      source: k8s.io/kubernetes/pkg/registry/rbac/validation.RoleGetter
  aliases: []
  interfaceAssertions:
    - type: RBACAuthorizer
      pointer: true
      interface: soapbox.test/apiserver/pkg/authorization/authorizer.Authorizer

release:
  policy: v1-to-v0
  firstTag: v0.36.1

commit:
  authorPolicy: preserve-upstream
  committer:
    name: soapbox[bot]
    email: soapbox[bot]@users.noreply.github.com
  trailerKey: Kubernetes-commit
  sign: false

vanity:
  repository: enj/enj.github.io
  path: kk/rbac_authorizer/index.html
  importPath: monis.app/kk/rbac_authorizer
  repositoryURL: https://github.com/enj/rbac_authorizer
  probeURL: https://monis.app/kk/rbac_authorizer?go-get=1

publication:
  mode: automatic
compatibility:
  apiserver: external

determinism:
  toolchain: go1.26.5
  chunkSize: 200
`
