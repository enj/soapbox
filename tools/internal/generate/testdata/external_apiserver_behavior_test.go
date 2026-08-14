// This test is copied into a generated RBAC module during release certification.
// It exercises the external apiserver mode through the generated facade and
// proves that the retained request-context API still uses real apiserver types.
package rbacauthorizer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	rbacauthorizer "monis.app/kk/rbac_authorizer"
	validation "monis.app/kk/rbac_authorizer/internal/kk/pkg/registry/rbac/validation"
)

type staticStore struct {
	roles               []*rbacv1.Role
	roleBindings        []*rbacv1.RoleBinding
	clusterRoles        []*rbacv1.ClusterRole
	clusterRoleBindings []*rbacv1.ClusterRoleBinding
}

func (s *staticStore) GetRole(_ context.Context, namespace, name string) (*rbacv1.Role, error) {
	for _, role := range s.roles {
		if role.Namespace == namespace && role.Name == name {
			return role, nil
		}
	}
	return nil, errors.New("role not found")
}

func (s *staticStore) ListRoleBindings(_ context.Context, namespace string) ([]*rbacv1.RoleBinding, error) {
	var bindings []*rbacv1.RoleBinding
	for _, binding := range s.roleBindings {
		if binding.Namespace == namespace {
			bindings = append(bindings, binding)
		}
	}
	return bindings, nil
}

func (s *staticStore) GetClusterRole(_ context.Context, name string) (*rbacv1.ClusterRole, error) {
	for _, role := range s.clusterRoles {
		if role.Name == name {
			return role, nil
		}
	}
	return nil, errors.New("cluster role not found")
}

func (s *staticStore) ListClusterRoleBindings(context.Context) ([]*rbacv1.ClusterRoleBinding, error) {
	return s.clusterRoleBindings, nil
}

func TestExternalApiserverBehavior(t *testing.T) {
	ctx := t.Context()
	podRule := rbacv1.PolicyRule{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods"}}
	serviceAccount := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "builder"}
	store := &staticStore{
		roles: []*rbacv1.Role{{
			ObjectMeta: objectMeta("reader", "team"),
			Rules:      []rbacv1.PolicyRule{podRule},
		}},
		roleBindings: []*rbacv1.RoleBinding{{
			ObjectMeta: objectMeta("readers", "team"),
			RoleRef:    rbacv1.RoleRef{Kind: "Role", Name: "reader"},
			Subjects:   []rbacv1.Subject{serviceAccount},
		}},
	}

	userInfo := &user.DefaultInfo{Name: "system:serviceaccount:team:builder"}
	attributes := authorizer.AttributesRecord{
		User: userInfo, Verb: "get", Namespace: "team", Resource: "pods", ResourceRequest: true,
	}
	rbac := rbacauthorizer.New(store, store, store, store)
	decision, reason, err := rbac.Authorize(ctx, attributes)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if decision != authorizer.DecisionAllow || !strings.Contains(reason, "readers/team") {
		t.Fatalf("authorize = %s, %q; want Allow by readers/team", decision, reason)
	}

	attributes.Verb = "delete"
	decision, _, err = rbac.Authorize(ctx, attributes)
	if err != nil {
		t.Fatalf("authorize denied request: %v", err)
	}
	if decision != authorizer.DecisionNoOpinion {
		t.Fatalf("authorize denied request = %s, want NoOpinion", decision)
	}

	resourceRules, nonResourceRules, incomplete, err := rbac.RulesFor(ctx, userInfo, "team")
	if err != nil {
		t.Fatalf("resolve rules: %v", err)
	}
	if incomplete || len(nonResourceRules) != 0 || len(resourceRules) != 1 {
		t.Fatalf("resolved rules = resource:%d nonresource:%d incomplete:%v", len(resourceRules), len(nonResourceRules), incomplete)
	}
	if got := resourceRules[0]; len(got.GetVerbs()) != 1 || got.GetVerbs()[0] != "get" || len(got.GetResources()) != 1 || got.GetResources()[0] != "pods" {
		t.Fatalf("resolved resource rule = %#v", got)
	}

	attributes.Verb = "get"
	locator := rbacauthorizer.NewSubjectAccessEvaluator(store, store, store, store, "root")
	subjects, err := locator.AllowedSubjects(ctx, attributes)
	if err != nil {
		t.Fatalf("locate subjects: %v", err)
	}
	for _, want := range []rbacv1.Subject{
		{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:masters"},
		{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "root"},
		serviceAccount,
	} {
		if !containsSubject(subjects, want) {
			t.Errorf("subjects %#v do not contain %#v", subjects, want)
		}
	}

	resolver := rbacauthorizer.NewDefaultRuleResolver(store, store, store, store)
	escalationContext := genericapirequest.WithNamespace(genericapirequest.WithUser(ctx, userInfo), "team")
	if err := validation.ConfirmNoEscalation(escalationContext, resolver, []rbacv1.PolicyRule{podRule}); err != nil {
		t.Fatalf("confirm held permission: %v", err)
	}
	unheld := rbacv1.PolicyRule{Verbs: []string{"delete"}, APIGroups: []string{""}, Resources: []string{"secrets"}}
	if err := validation.ConfirmNoEscalation(escalationContext, resolver, []rbacv1.PolicyRule{unheld}); err == nil || !strings.Contains(err.Error(), userInfo.Name) {
		t.Fatalf("confirm unheld permission = %v, want named escalation refusal", err)
	}
}

func objectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace}
}

func containsSubject(subjects []rbacv1.Subject, want rbacv1.Subject) bool {
	for _, subject := range subjects {
		if subject == want {
			return true
		}
	}
	return false
}
