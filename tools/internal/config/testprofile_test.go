package config_test

// baseProfile is a complete, valid profile. Validation tests apply one targeted
// mutation at a time so each case proves exactly one rule.
const baseProfile = `version: 2
source:
  repository: https://github.com/kubernetes/kubernetes.git
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
  files:
    - pkg/apis/rbac/v1/register.go
  required:
    - pkg/apis/rbac/v1/doc.go
deny:
  imports:
    - k8s.io/kubernetes/pkg/apis/rbac
closure:
  includeTests: false
  limits:
    maxPackages: 8
    maxFiles: 40
    maxNonTestLines: 5000
    maxPackageGrowth: 2
  golden: testdata/closure/rbac-v1.36.1.json
types:
  policy: prefer-external
  pairs:
    - internal: k8s.io/kubernetes/pkg/apis/rbac
      external: k8s.io/api/rbac/v1
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
  aliases:
    - name: RoleGetterFromLister
      kind: type
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RoleGetter
  interfaceAssertions:
    - type: RBACAuthorizer
      pointer: true
      interface: k8s.io/apiserver/pkg/authorization/authorizer.Authorizer
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
  mode: manual
compatibility:
  apiserver: external
determinism:
  toolchain: go1.26.5
  chunkSize: 200
`
