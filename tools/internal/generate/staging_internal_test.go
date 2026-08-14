package generate

import (
	"strings"
	"testing"
)

func TestCheckStagingSourceRequiresCanonicalProductionRepository(t *testing.T) {
	tests := []struct {
		name         string
		sourceRemote string
		module       string
		remote       string
		want         string
	}{
		{
			name:   "canonical production repository",
			module: "k8s.io/api",
			remote: "https://github.com/kubernetes/api.git",
		},
		{
			name:   "wrong production repository",
			module: "k8s.io/api",
			remote: "https://github.com/kubernetes/apimachinery.git",
			want:   "does not match canonical",
		},
		{
			name:   "local staging with production source",
			module: "k8s.io/api",
			remote: "file:///tmp/api.git",
			want:   "does not match canonical",
		},
		{
			name:         "local fixture repositories",
			sourceRemote: "file:///tmp/kubernetes.git",
			module:       "soapbox.test/api",
			remote:       "file:///tmp/api.git",
		},
		{
			name:   "missing repository",
			module: "k8s.io/api",
			want:   "has no repository",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := run{opts: Options{SourceRemote: test.sourceRemote}}
			err := r.checkStagingSource(test.module, StagingSource{Remote: test.remote})
			if test.want == "" {
				if err != nil {
					t.Fatalf("check staging source: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

func TestLocalSourceRemote(t *testing.T) {
	tests := []struct {
		remote string
		local  bool
	}{
		{remote: "/tmp/repo.git", local: true},
		{remote: "file:///tmp/repo.git", local: true},
		{remote: "https://github.com/kubernetes/api.git"},
		{remote: "origin"},
		{remote: ""},
	}
	for _, test := range tests {
		t.Run(test.remote, func(t *testing.T) {
			if got := localSourceRemote(test.remote); got != test.local {
				t.Errorf("localSourceRemote(%q) = %v, want %v", test.remote, got, test.local)
			}
		})
	}
}
