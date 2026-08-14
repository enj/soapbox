package generate

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

func testCompatibilityLayout() compatibilityLayout {
	return compatibilityLayout{
		apiserverModule:  "k8s.io/apiserver",
		root:             "internal/kk/compat/apiserver",
		userImport:       "example.com/generated/internal/kk/compat/apiserver/user",
		authorizerImport: "example.com/generated/internal/kk/compat/apiserver/authorizer",
		serviceImport:    "example.com/generated/internal/kk/compat/apiserver/serviceaccount",
	}
}

func TestRewriteCompatibilityFileUsesExplicitIdentityInputs(t *testing.T) {
	file := relocate.File{
		Path: "internal/kk/pkg/registry/rbac/validation/rule.go",
		Mode: relocate.ModeRegular,
		Contents: []byte(`package validation

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apiserver/pkg/authentication/serviceaccount"
	"k8s.io/apiserver/pkg/authentication/user"
	authorizer "k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
)

var _ authorizer.Decision
var _ = serviceaccount.MatchesUsername

func ConfirmNoEscalation(ctx context.Context, ruleResolver AuthorizationRuleResolver, rules []rbacv1.PolicyRule) error {
	user, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return fmt.Errorf("no user on context")
	}
	namespace, _ := genericapirequest.NamespaceFrom(ctx)

	ownerRules, err := ruleResolver.RulesFor(ctx, user, namespace)
	_ = ownerRules
	return err
}
`),
	}
	got, changes, err := rewriteCompatibilityFile(file, testCompatibilityLayout())
	if err != nil {
		t.Fatalf("rewrite compatibility: %v", err)
	}
	text := string(got.Contents)
	for _, forbidden := range []string{
		"k8s.io/apiserver/", "genericapirequest", "UserFrom(ctx)", "NamespaceFrom(ctx)", "no user on context",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("rewritten source retains %q:\n%s", forbidden, text)
		}
	}
	for _, want := range []string{
		compatibilityNotice,
		`"example.com/generated/internal/kk/compat/apiserver/user"`,
		`authorizer "example.com/generated/internal/kk/compat/apiserver/authorizer"`,
		`"example.com/generated/internal/kk/compat/apiserver/serviceaccount"`,
		"user user.Info, namespace string",
		"ownerRules, err := ruleResolver.RulesFor(ctx, user, namespace)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rewritten source does not contain %q:\n%s", want, text)
		}
	}
	if !slicesContainChange(changes, rewrite.ChangeCompatibility) || !slicesContainChange(changes, rewrite.ChangeImport) || !slicesContainChange(changes, rewrite.ChangeNotice) {
		t.Errorf("changes = %#v, want compatibility, import, and notice records", changes)
	}
}

func TestRewriteCompatibilityFileRefusesPrivateContextWithoutTransformTarget(t *testing.T) {
	file := relocate.File{
		Path: "internal/kk/pkg/other/other.go", Mode: relocate.ModeRegular,
		Contents: []byte("package other\n\nimport genericapirequest \"k8s.io/apiserver/pkg/endpoints/request\"\n"),
	}
	if _, _, err := rewriteCompatibilityFile(file, testCompatibilityLayout()); err == nil || !strings.Contains(err.Error(), "ConfirmNoEscalation") {
		t.Fatalf("rewrite = %v, want missing ConfirmNoEscalation refusal", err)
	}
}

func slicesContainChange(changes []rewrite.Change, kind rewrite.ChangeKind) bool {
	for _, change := range changes {
		if change.Kind == kind {
			return true
		}
	}
	return false
}
