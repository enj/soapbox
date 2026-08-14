// This test is copied into a generated RBAC module during release certification.
// Run it with a temporary modfile that adds k8s.io/component-helpers at the exact
// source release version; the published go.mod remains forbidden-module clean.
package validation_test

import (
	"math/rand"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	upstream "k8s.io/component-helpers/auth/rbac/validation"
	local "monis.app/kk/rbac_authorizer/internal/kk/staging/src/k8s.io/component-helpers/auth/rbac/validation"
)

func TestCoversDifferential(t *testing.T) {
	tests := []struct {
		name    string
		owner   []rbacv1.PolicyRule
		servant []rbacv1.PolicyRule
	}{
		{name: "empty"},
		{name: "resource wildcard", owner: []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}, servant: []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments/status"}, Verbs: []string{"update"}}}},
		{name: "subresource wildcard", owner: []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"*/status"}, Verbs: []string{"get"}}}, servant: []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments/status"}, Verbs: []string{"get"}}}},
		{name: "resource names", owner: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"a", "b"}, Verbs: []string{"get"}}}, servant: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"b"}, Verbs: []string{"get"}}}},
		{name: "nonresource prefix", owner: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/api/*"}, Verbs: []string{"get"}}}, servant: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/api/v1"}, Verbs: []string{"get"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { compareCovers(t, test.owner, test.servant) })
	}

	rng := rand.New(rand.NewSource(0x5a17c0de))
	for i := 0; i < 10000; i++ {
		compareCovers(t, randomRules(rng), randomRules(rng))
	}
}

func compareCovers(t *testing.T, owner, servant []rbacv1.PolicyRule) {
	t.Helper()
	upstreamOK, upstreamMissing := upstream.Covers(owner, servant)
	localOK, localMissing := local.Covers(owner, servant)
	if upstreamOK != localOK || !reflect.DeepEqual(upstreamMissing, localMissing) {
		t.Fatalf("Covers differs\nowner: %#v\nservant: %#v\nupstream: %v %#v\nlocal: %v %#v", owner, servant, upstreamOK, upstreamMissing, localOK, localMissing)
	}
}

func randomRules(rng *rand.Rand) []rbacv1.PolicyRule {
	count := rng.Intn(5)
	rules := make([]rbacv1.PolicyRule, count)
	for i := range rules {
		if rng.Intn(3) == 0 {
			rules[i] = rbacv1.PolicyRule{
				Verbs:           choose(rng, []string{"get", "post", "*"}),
				NonResourceURLs: choose(rng, []string{"/api", "/api/*", "/healthz", "/metrics"}),
			}
			continue
		}
		rules[i] = rbacv1.PolicyRule{
			APIGroups:     choose(rng, []string{"", "apps", "rbac.authorization.k8s.io", "*"}),
			Resources:     choose(rng, []string{"pods", "pods/log", "deployments", "deployments/status", "*/status", "*"}),
			Verbs:         choose(rng, []string{"get", "list", "watch", "create", "update", "delete", "*"}),
			ResourceNames: choose(rng, []string{"one", "two", "three"}),
		}
	}
	return rules
}

func choose(rng *rand.Rand, values []string) []string {
	count := rng.Intn(4)
	chosen := make([]string, count)
	for i := range chosen {
		chosen[i] = values[rng.Intn(len(values))]
	}
	return chosen
}
