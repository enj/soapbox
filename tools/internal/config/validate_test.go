package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
)

// mutation replaces one exact substring of the base profile.
type mutation struct {
	old string
	new string
}

func TestDecodeRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name      string
		mutations []mutation
		want      string
	}{
		{
			name:      "unknown field",
			mutations: []mutation{{old: "version: 2\n", new: "version: 2\nunknownField: true\n"}},
			want:      "field unknownField not found",
		},
		{
			name:      "unknown nested field",
			mutations: []mutation{{old: "  importPrefix: k8s.io/kubernetes\n", new: "  importPrefix: k8s.io/kubernetes\n  mirror: https://example.com\n"}},
			want:      "field mirror not found",
		},
		{
			name:      "unsupported schema version",
			mutations: []mutation{{old: "version: 2\n", new: "version: 3\n"}},
			want:      "unsupported schema version 3",
		},
		{
			name:      "insecure source scheme",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes", new: "repository: http://github.com/kubernetes"}},
			want:      "must use https",
		},
		{
			name:      "credentials in source URL",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes", new: "repository: https://token@github.com/kubernetes"}},
			want:      "must not embed credentials",
		},
		{
			name:      "unallowed source host",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes/kubernetes.git", new: "repository: https://evil.example.com/kubernetes/kubernetes.git"}},
			want:      "which is not one of",
		},
		{
			name:      "explicit port",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes/kubernetes.git", new: "repository: https://github.com:8443/kubernetes/kubernetes.git"}},
			want:      "must not set an explicit port",
		},
		{
			name:      "source URL without git suffix",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes/kubernetes.git", new: "repository: https://github.com/kubernetes/kubernetes"}},
			want:      `must end with ".git"`,
		},
		{
			name:      "incomplete semantic version",
			mutations: []mutation{{old: "minimumRelease: v1.36.1", new: "minimumRelease: v1.36"}},
			want:      "must be vMAJOR.MINOR.PATCH",
		},
		{
			name:      "leading zero in version",
			mutations: []mutation{{old: "minimumRelease: v1.36.1", new: "minimumRelease: v1.036.1"}},
			want:      "must not have a leading zero",
		},
		{
			name:      "build metadata in version",
			mutations: []mutation{{old: "minimumRelease: v1.36.1", new: "minimumRelease: v1.36.1+meta"}},
			want:      "must not carry build metadata",
		},
		{
			name:      "release tag does not match minimum release",
			mutations: []mutation{{old: "firstTag: v0.36.1", new: "firstTag: v0.36.2"}},
			want:      "does not match source.refs.minimumRelease",
		},
		{
			name:      "unsupported release policy",
			mutations: []mutation{{old: "  policy: v1-to-v0", new: "  policy: v1-to-v2"}},
			want:      "release.policy: unsupported value",
		},
		{
			name:      "duplicate tracked branch",
			mutations: []mutation{{old: "      - master\n", new: "      - master\n      - master\n"}},
			want:      "duplicate branch",
		},
		{
			name:      "malformed anchor commit",
			mutations: []mutation{{old: `anchorCommit: ""`, new: `anchorCommit: "not-a-sha"`}},
			want:      "must be 40 or 64 hexadecimal characters",
		},
		{
			name:      "uppercase anchor commit",
			mutations: []mutation{{old: `anchorCommit: ""`, new: `anchorCommit: "A1B2C3D4E5F60718293A4B5C6D7E8F90A1B2C3D4"`}},
			want:      "must be lower case hexadecimal",
		},
		{
			name:      "absolute prune path",
			mutations: []mutation{{old: "    - pkg/apis/rbac/v1/register.go", new: "    - /etc/shadow"}},
			want:      "path must be relative",
		},
		{
			name:      "prune path traversal",
			mutations: []mutation{{old: "    - pkg/apis/rbac/v1/register.go", new: "    - ../../../etc/shadow"}},
			want:      "must not traverse parent directories",
		},
		{
			name:      "prune pattern",
			mutations: []mutation{{old: "    - pkg/apis/rbac/v1/register.go", new: "    - pkg/apis/rbac/v1/*.go"}},
			want:      "must be an exact file path, not a pattern",
		},
		{
			name:      "prune entry is not clean",
			mutations: []mutation{{old: "    - pkg/apis/rbac/v1/register.go", new: "    - pkg/apis/rbac/v1/./register.go"}},
			want:      "clean slash form",
		},
		{
			name:      "duplicate prune entry",
			mutations: []mutation{{old: "    - pkg/apis/rbac/v1/register.go\n", new: "    - pkg/apis/rbac/v1/register.go\n    - pkg/apis/rbac/v1/register.go\n"}},
			want:      "duplicate entry",
		},
		{
			name:      "prune and require the same file",
			mutations: []mutation{{old: "    - pkg/apis/rbac/v1/doc.go", new: "    - pkg/apis/rbac/v1/register.go"}},
			want:      "is both pruned and required",
		},
		{
			name:      "deny a configured package root",
			mutations: []mutation{{old: "    - k8s.io/kubernetes/pkg/apis/rbac\ncl", new: "    - k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac\ncl"}},
			want:      "denies configured package root",
		},
		{
			name:      "deny the whole source module",
			mutations: []mutation{{old: "    - k8s.io/kubernetes/pkg/apis/rbac\ncl", new: "    - k8s.io/kubernetes\ncl"}},
			want:      "denies the whole source module",
		},
		{
			name:      "internal prefix without an internal element",
			mutations: []mutation{{old: "internalPrefix: internal/kk", new: "internalPrefix: vendor/kk"}},
			want:      "must contain an internal element",
		},
		{
			name:      "absolute internal prefix",
			mutations: []mutation{{old: "internalPrefix: internal/kk", new: "internalPrefix: /internal/kk"}},
			want:      "path must be relative",
		},
		{
			name:      "remote does not match repository",
			mutations: []mutation{{old: "remote: https://github.com/enj/rbac_authorizer.git", new: "remote: https://github.com/enj/other.git"}},
			want:      "does not match destination.repository",
		},
		{
			name:      "state ref outside refs/heads",
			mutations: []mutation{{old: "stateRef: refs/heads/soapbox-state", new: "stateRef: refs/soapbox/state"}},
			want:      "must live under refs/heads/",
		},
		{
			name:      "state ref equals the consumer branch",
			mutations: []mutation{{old: "stateRef: refs/heads/soapbox-state", new: "stateRef: refs/heads/main"}},
			want:      "must differ from the consumer branch",
		},
		{
			name:      "progress refs shadow branches",
			mutations: []mutation{{old: "progressRefPrefix: refs/soapbox/progress/", new: "progressRefPrefix: refs/heads/progress/"}},
			want:      "must not shadow branches or tags",
		},
		{
			name:      "progress refs without a trailing slash",
			mutations: []mutation{{old: "progressRefPrefix: refs/soapbox/progress/", new: "progressRefPrefix: refs/soapbox/progress"}},
			want:      "must end with a slash",
		},
		{
			name:      "root package is not a Go package name",
			mutations: []mutation{{old: "rootPackage: rbacauthorizer", new: "rootPackage: RBACAuthorizer"}},
			want:      "must be lower case letters and digits",
		},
		{
			name:      "empty upstream project name",
			mutations: []mutation{{old: "  project: Kubernetes\n", new: "  project: \"\"\n"}},
			want:      "source.project: project name must not be empty",
		},
		{
			name:      "upstream project name is not single spaced",
			mutations: []mutation{{old: "  project: Kubernetes\n", new: "  project: \"The  Kubernetes Project\"\n"}},
			want:      "must be single spaced",
		},
		{
			name:      "unverifiable licence identifier",
			mutations: []mutation{{old: "  license: Apache-2.0\n", new: "  license: GPL-3.0-only\n"}},
			want:      "source.license: unsupported value \"GPL-3.0-only\"",
		},
		{
			name:      "missing licence identifier",
			mutations: []mutation{{old: "  license: Apache-2.0\n", new: "  license: \"\"\n"}},
			want:      "source.license: unsupported value \"\"",
		},
		{
			name:      "empty destination summary",
			mutations: []mutation{{old: "  summary: the Kubernetes RBAC authorizer as an independently consumable Go module.\n", new: "  summary: \"\"\n"}},
			want:      "destination.summary: summary must not be empty",
		},
		{
			name:      "destination summary starts a new sentence",
			mutations: []mutation{{old: "  summary: the Kubernetes", new: "  summary: The Kubernetes"}},
			want:      "must start with a lower case letter",
		},
		{
			name:      "destination summary is not a sentence",
			mutations: []mutation{{old: " consumable Go module.\n", new: " consumable Go module\n"}},
			want:      "must end with a full stop",
		},
		{
			name:      "destination summary has trailing space",
			mutations: []mutation{{old: "  summary: the Kubernetes", new: "  summary: \"  the Kubernetes"}, {old: " consumable Go module.\n", new: " consumable Go module.\"\n"}},
			want:      "must be single spaced and free of leading or trailing space",
		},
		{
			name:      "closure limit is zero",
			mutations: []mutation{{old: "maxPackages: 8", new: "maxPackages: 0"}},
			want:      "closure.limits.maxPackages: must be greater than zero",
		},
		{
			name: "closure limit cannot hold the package roots",
			mutations: []mutation{
				{old: "    - plugin/pkg/auth/authorizer/rbac\n", new: "    - plugin/pkg/auth/authorizer/rbac\n    - pkg/registry/rbac/validation\n"},
				{old: "maxPackages: 8", new: "maxPackages: 1"},
			},
			want: "cannot hold 2 configured package roots",
		},
		{
			name:      "unsupported type policy",
			mutations: []mutation{{old: "  policy: prefer-external", new: "  policy: rewrite-everything"}},
			want:      "types.policy: unsupported value",
		},
		{
			name:      "keep internal policy with pairs",
			mutations: []mutation{{old: "  policy: prefer-external", new: "  policy: keep-internal"}},
			want:      "cannot declare type pairs",
		},
		{
			name:      "external pair target inside the source module",
			mutations: []mutation{{old: "external: k8s.io/api/rbac/v1", new: "external: k8s.io/kubernetes/pkg/apis/rbac/v1"}},
			want:      "must be an external module path",
		},
		{
			name:      "internal pair target outside the source module",
			mutations: []mutation{{old: "internal: k8s.io/kubernetes/pkg/apis/rbac", new: "internal: k8s.io/api/rbac/v1"}},
			want:      "is not part of source.importPrefix",
		},
		{
			name:      "external dependency policy with copied packages",
			mutations: []mutation{{old: "  copyPackages: []", new: "  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"}},
			want:      "cannot copy staging packages",
		},
		{
			name: "copy package outside staging",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - pkg/util/sets"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
			},
			want: "must be a staging/src path",
		},
		{
			name:      "correctness gate disabled",
			mutations: []mutation{{old: "    interoperability: true", new: "    interoperability: false"}},
			want:      "correctness gate cannot be disabled",
		},
		{
			name:      "negative cost gate",
			mutations: []mutation{{old: "maxCopiedLines: 0", new: "maxCopiedLines: -1"}},
			want:      "must not be negative",
		},
		{
			name:      "negative cadence gate",
			mutations: []mutation{{old: "maxReleasesPerMinor: 0", new: "maxReleasesPerMinor: -1"}},
			want:      "dependencies.gates.cost.maxReleasesPerMinor: must not be negative",
		},
		{
			name:      "negative leverage gate",
			mutations: []mutation{{old: "minPackagesRemoved: 0", new: "minPackagesRemoved: -3"}},
			want:      "dependencies.gates.cost.minPackagesRemoved: must not be negative",
		},
		{
			name: "copy approved without a cadence ceiling",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
				{old: "minLinesRemoved: 0", new: "minLinesRemoved: 500"},
			},
			want: "dependencies.gates.cost.maxReleasesPerMinor: policy \"copy-approved\" requires a non zero cap",
		},
		{
			name: "copy approved states no benefit",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
				{old: "maxReleasesPerMinor: 0", new: "maxReleasesPerMinor: 6"},
			},
			want: "so the copy states the benefit it delivers",
		},
		{
			name: "override relaxes an unsupported gate",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
				{old: "maxReleasesPerMinor: 0", new: "maxReleasesPerMinor: 6"},
				{old: "minLinesRemoved: 0", new: "minLinesRemoved: 500"},
				{old: "  overrides: []", new: "  overrides:\n    - package: staging/src/k8s.io/apiserver/pkg/authorization/authorizer\n      gate: minPackagesRemoved\n      justification: because\n      approver: Monis Khan\n      expiresAfter: v1.40"},
			},
			want: "unsupported gate \"minPackagesRemoved\"",
		},
		{
			name: "override relaxes a correctness gate",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
				{old: "  overrides: []", new: "  overrides:\n    - package: staging/src/k8s.io/apiserver/pkg/authorization/authorizer\n      gate: interoperability\n      justification: because\n      approver: Monis Khan\n      expiresAfter: v1.40"},
			},
			want: "correctness gate \"interoperability\" cannot be overridden",
		},
		{
			name: "override expired before the minimum release",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
				{old: "  overrides: []", new: "  overrides:\n    - package: staging/src/k8s.io/apiserver/pkg/authorization/authorizer\n      gate: maxCopiedLines\n      justification: because\n      approver: Monis Khan\n      expiresAfter: v1.30"},
			},
			want: "already expired",
		},
		{
			name: "override without a justification",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 4"},
				{old: "  overrides: []", new: "  overrides:\n    - package: staging/src/k8s.io/apiserver/pkg/authorization/authorizer\n      gate: maxCopiedLines\n      justification: \"\"\n      approver: Monis Khan\n      expiresAfter: v1.40"},
			},
			want: "needs a justification",
		},
		{
			name:      "patch outside the patches directory",
			mutations: []mutation{{old: "patches: []", new: "patches:\n  - file: hack/0001-export.patch\n    since: \"\"\n    until: \"\"\n    branches: []"}},
			want:      "must live under patches/",
		},
		{
			name:      "patch selects an untracked branch",
			mutations: []mutation{{old: "patches: []", new: "patches:\n  - file: patches/0001-export.patch\n    since: \"\"\n    until: \"\"\n    branches:\n      - release-1.35"}},
			want:      "is not a tracked source branch",
		},
		{
			name:      "patch range is empty",
			mutations: []mutation{{old: "patches: []", new: "patches:\n  - file: patches/0001-export.patch\n    since: v1.36.1\n    until: v1.36.1\n    branches: []"}},
			want:      "selects an empty range",
		},
		{
			name:      "duplicate patch file",
			mutations: []mutation{{old: "patches: []", new: "patches:\n  - file: patches/0001-export.patch\n    since: \"\"\n    until: \"\"\n    branches: []\n  - file: patches/0001-export.patch\n    since: \"\"\n    until: \"\"\n    branches: []"}},
			want:      "duplicate patch file",
		},
		{
			name:      "facade export renames its symbol",
			mutations: []mutation{{old: "    - name: New\n", new: "    - name: Create\n"}},
			want:      "must keep upstream name",
		},
		{
			name:      "facade alias does not rename its symbol",
			mutations: []mutation{{old: "    - name: RoleGetterFromLister", new: "    - name: RoleGetter"}},
			want:      "does not rename upstream symbol",
		},
		{
			name:      "duplicate facade name",
			mutations: []mutation{{old: "  aliases:\n", new: "    - name: New\n      kind: func\n      source: k8s.io/kubernetes/pkg/registry/rbac/validation.New\n  aliases:\n"}},
			want:      "duplicate public name",
		},
		{
			name:      "facade exports an external type",
			mutations: []mutation{{old: "source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RBACAuthorizer", new: "source: k8s.io/api/rbac/v1.PolicyRule"}},
			want:      "must be a relocated source package",
		},
		{
			name:      "facade exports a variable",
			mutations: []mutation{{old: "      kind: func\n", new: "      kind: var\n"}},
			want:      "facade.exports.kind: unsupported value \"var\"",
		},
		{
			name:      "facade export name is not exported",
			mutations: []mutation{{old: "    - name: New\n", new: "    - name: new\n"}},
			want:      "must be exported",
		},
		{
			name:      "assertion names an unknown facade type",
			mutations: []mutation{{old: "    - type: RBACAuthorizer\n      pointer: true", new: "    - type: MissingType\n      pointer: true"}},
			want:      "is not an exported facade type",
		},
		{
			name:      "assertion interface is a bare identifier",
			mutations: []mutation{{old: "      interface: k8s.io/apiserver/pkg/authorization/authorizer.Authorizer", new: "      interface: Authorizer"}},
			want:      "must be <import path>.<Name>",
		},
		{
			// A facade export is a name this module publishes, so asserting
			// against it would compare the type to a copy of the interface
			// rather than to the one consumers pass it to.
			name: "assertion interface names a facade export",
			mutations: []mutation{
				{old: "  interfaceAssertions:\n    - type: RBACAuthorizer\n      pointer: true\n      interface: k8s.io/apiserver/pkg/authorization/authorizer.Authorizer",
					new: "  interfaceAssertions:\n    - type: RBACAuthorizer\n      pointer: true\n      interface: RBACAuthorizer"},
			},
			want: "must be <import path>.<Name>",
		},
		{
			name:      "assertion interface has no domain qualified package",
			mutations: []mutation{{old: "      interface: k8s.io/apiserver/pkg/authorization/authorizer.Authorizer", new: "      interface: authorizer.Authorizer"}},
			want:      "must name a package qualified interface",
		},
		{
			name:      "assertion interface is inside the generated module",
			mutations: []mutation{{old: "      interface: k8s.io/apiserver/pkg/authorization/authorizer.Authorizer", new: "      interface: monis.app/kk/rbac_authorizer/internal/kk/authorizer.Authorizer"}},
			want:      "proves nothing about the real one",
		},
		{
			name:      "facade package does not match the root package",
			mutations: []mutation{{old: "facade:\n  package: rbacauthorizer", new: "facade:\n  package: other"}},
			want:      "must match destination.rootPackage",
		},
		{
			name:      "facade file outside the root package",
			mutations: []mutation{{old: "  file: authorizer.go", new: "  file: pkg/authorizer.go"}},
			want:      "must live in the module root package",
		},
		{
			name:      "generated commits may not be signed",
			mutations: []mutation{{old: "  sign: false", new: "  sign: true"}},
			want:      "generated commits are never signed",
		},
		{
			name:      "unsupported author policy",
			mutations: []mutation{{old: "authorPolicy: preserve-upstream", new: "authorPolicy: rewrite"}},
			want:      "commit.authorPolicy: unsupported value",
		},
		{
			name:      "malformed trailer key",
			mutations: []mutation{{old: "trailerKey: Kubernetes-commit", new: "trailerKey: 1nvalid"}},
			want:      "must be a Git trailer token",
		},
		{
			name:      "malformed committer email",
			mutations: []mutation{{old: "email: soapbox[bot]@users.noreply.github.com", new: "email: soapbox-bot"}},
			want:      "must be local@domain",
		},
		{
			name:      "vanity path is not an index page",
			mutations: []mutation{{old: "path: kk/rbac_authorizer/index.html", new: "path: kk/rbac_authorizer/page.html"}},
			want:      "must end with /index.html",
		},
		{
			name:      "vanity import path does not match the module",
			mutations: []mutation{{old: "importPath: monis.app/kk/rbac_authorizer", new: "importPath: monis.app/kk/other"}},
			want:      "must match destination.module",
		},
		{
			name:      "vanity probe query is wrong",
			mutations: []mutation{{old: "probeURL: https://monis.app/kk/rbac_authorizer?go-get=1", new: "probeURL: https://monis.app/kk/rbac_authorizer?go-get=2"}},
			want:      `must carry the query "go-get=1"`,
		},
		{
			name:      "vanity probe host is not the module domain",
			mutations: []mutation{{old: "probeURL: https://monis.app/kk/rbac_authorizer?go-get=1", new: "probeURL: https://github.com/kk/rbac_authorizer?go-get=1"}},
			want:      "which is not one of monis.app",
		},

		{
			name:      "toolchain is not an exact patch release",
			mutations: []mutation{{old: "toolchain: go1.26.5", new: "toolchain: go1.26"}},
			want:      "must pin an exact patch release",
		},
		{
			name:      "chunk size is zero",
			mutations: []mutation{{old: "chunkSize: 200", new: "chunkSize: 0"}},
			want:      "determinism.chunkSize: must be greater than zero",
		},
		{
			name:      "unsupported publication mode",
			mutations: []mutation{{old: "mode: manual", new: "mode: scheduled"}},
			want:      "publication.mode: unsupported value",
		},
		{
			name:      "unsupported compatibility apiserver",
			mutations: []mutation{{old: "apiserver: external", new: "apiserver: hybrid"}},
			want:      "compatibility.apiserver: unsupported value",
		},
		{
			name: "recursive root cannot deny a package below itself",
			mutations: []mutation{
				{old: "  recursive: false", new: "  recursive: true"},
				{old: "    - k8s.io/kubernetes/pkg/apis/rbac\ncl", new: "    - k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac/bootstrappolicy\ncl"},
			},
			want: "denies a package below recursive root",
		},
		{
			name: "copy package cap cannot admit the copy list",
			mutations: []mutation{
				{old: "  policy: external\n  copyPackages: []", new: "  policy: copy-approved\n  copyPackages:\n    - staging/src/k8s.io/apiserver/pkg/authorization/authorizer\n    - staging/src/k8s.io/apiserver/pkg/authentication/user"},
				{old: "maxCopiedPackages: 0", new: "maxCopiedPackages: 1"},
			},
			want: "cannot admit 2 copy packages",
		},
		{
			name:      "assertion implementing type is an interface",
			mutations: []mutation{{old: "    - type: RBACAuthorizer\n      pointer: true", new: "    - type: RequestToRuleMapper\n      pointer: true"}, {old: "    - name: RBACAuthorizer\n      kind: type", new: "    - name: RequestToRuleMapper\n      kind: interface"}, {old: "source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RBACAuthorizer", new: "source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RequestToRuleMapper"}},
			want:      "an assertion needs the concrete type",
		},
		{
			name:      "committer name with angle brackets",
			mutations: []mutation{{old: "    name: soapbox[bot]", new: "    name: soapbox <bot>"}},
			want:      "must not contain angle brackets",
		},
		{
			name:      "committer name with surrounding space",
			mutations: []mutation{{old: "    name: soapbox[bot]", new: `    name: " soapbox[bot] "`}},
			want:      "must not have leading or trailing space",
		},
		{
			name:      "committer email with two at signs",
			mutations: []mutation{{old: "email: soapbox[bot]@users.noreply.github.com", new: "email: soapbox@bot@users.noreply.github.com"}},
			want:      "must contain exactly one @",
		},
		{
			name:      "root package is a Go keyword",
			mutations: []mutation{{old: "rootPackage: rbacauthorizer", new: "rootPackage: package"}, {old: "facade:\n  package: rbacauthorizer", new: "facade:\n  package: package"}},
			want:      "is a Go keyword",
		},
		{
			name:      "recursive asset glob",
			mutations: []mutation{{old: "  assetGlobs: []", new: "  assetGlobs:\n    - pkg/**/testdata/*.json"}},
			want:      "recursive ** syntax",
		},
		{
			name:      "source URL with percent encoded traversal",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes/kubernetes.git", new: "repository: https://github.com/kubernetes/%2e%2e/other.git"}},
			want:      "must not traverse parent directories",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := mutate(t, baseProfile, test.mutations)
			_, err := config.Decode([]byte(profile))
			if err == nil {
				t.Fatalf("expected an error containing %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error does not contain %q:\n%v", test.want, err)
			}
		})
	}
}

// TestDecodeNeverEchoesCredentials proves that a URL carrying a token cannot
// reach a diagnostic, however it is malformed.
func TestDecodeNeverEchoesCredentials(t *testing.T) {
	const token = "ghs_PROFILETOKEN"
	tests := []struct {
		name      string
		mutations []mutation
	}{
		{
			name:      "source repository",
			mutations: []mutation{{old: "repository: https://github.com/kubernetes/kubernetes.git", new: "repository: https://" + token + "@github.com/kubernetes/kubernetes.git"}},
		},
		{
			name:      "destination remote",
			mutations: []mutation{{old: "remote: https://github.com/enj/rbac_authorizer.git", new: "remote: https://" + token + "@github.com/enj/rbac_authorizer.git"}},
		},
		{
			name:      "unparseable URL with credentials",
			mutations: []mutation{{old: "repositoryURL: https://github.com/enj/rbac_authorizer", new: "repositoryURL: https://" + token + "@github.com/enj/rbac_authorizer/%zz"}},
		},
		{
			name:      "control character in a credential URL",
			mutations: []mutation{{old: "probeURL: https://monis.app/kk/rbac_authorizer?go-get=1", new: `probeURL: "https://` + token + "@monis.app/\u007f?go-get=1\""}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Decode([]byte(mutate(t, baseProfile, test.mutations)))
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error echoes the credential:\n%v", err)
			}
		})
	}
}

func TestDecodeReportsEveryProblemAtOnce(t *testing.T) {
	profile := mutate(t, baseProfile, []mutation{
		{old: "chunkSize: 200", new: "chunkSize: 0"},
		{old: "  sign: false", new: "  sign: true"},
	})
	_, err := config.Decode([]byte(profile))
	if err == nil {
		t.Fatal("expected an error")
	}
	var invalid *config.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error %v is not a validation error", err)
	}
	if len(invalid.Problems) != 2 {
		t.Fatalf("got %d problems, want 2:\n%v", len(invalid.Problems), err)
	}
}

// mutate applies every replacement and fails when one does not match.
func mutate(t *testing.T, profile string, mutations []mutation) string {
	t.Helper()
	for _, m := range mutations {
		if count := strings.Count(profile, m.old); count != 1 {
			t.Fatalf("mutation %q matched %d times, want exactly 1", m.old, count)
		}
		profile = strings.Replace(profile, m.old, m.new, 1)
	}
	return profile
}
