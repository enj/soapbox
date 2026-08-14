package sync

import (
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/release"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/state"
)

func TestObservedPublishedPreservesAndAdvancesConfirmedRefs(t *testing.T) {
	const (
		branchRef = "refs/heads/main"
		oldTagRef = "refs/tags/v0.36.1"
		newTagRef = "refs/tags/v0.36.2"
		oldSource = "1111111111111111111111111111111111111111"
		newSource = "2222222222222222222222222222222222222222"
		oldBranch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		newBranch = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		oldTag    = "cccccccccccccccccccccccccccccccccccccccc"
		newTag    = "dddddddddddddddddddddddddddddddddddddddd"
	)

	prior := []state.Published{
		{Ref: branchRef, Kind: state.KindBranch, Object: oldBranch, Source: oldSource},
		{Ref: oldTagRef, Kind: state.KindTag, Object: oldTag, Source: oldSource},
	}
	tests := []struct {
		name     string
		observed map[string]string
		want     map[string]state.Published
	}{
		{
			name: "new release plan retains prior observations",
			observed: map[string]string{
				branchRef: oldBranch,
				oldTagRef: oldTag,
			},
			want: map[string]state.Published{
				branchRef: prior[0],
				oldTagRef: prior[1],
			},
		},
		{
			name: "completed consumer push advances and adds observations",
			observed: map[string]string{
				branchRef: newBranch,
				oldTagRef: oldTag,
				newTagRef: newTag,
			},
			want: map[string]state.Published{
				branchRef: {Ref: branchRef, Kind: state.KindBranch, Object: newBranch, Source: newSource},
				oldTagRef: prior[1],
				newTagRef: {Ref: newTagRef, Kind: state.KindTag, Object: newTag, Source: newSource},
			},
		},
		{
			name: "unconfirmed prior claims are omitted",
			observed: map[string]string{
				branchRef: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				oldTagRef: "ffffffffffffffffffffffffffffffffffffffff",
			},
			want: map[string]state.Published{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := run{
				opts: ProjectOptions{
					Config:  &config.Config{Destination: config.Destination{Branch: "main"}},
					Release: Release{Commit: newSource},
				},
				observed: test.observed,
				prior:    state.Document{Published: prior},
				result: Result{
					Replay:  &replay.Result{Heads: []replay.Head{{Destination: newBranch}}},
					Release: &release.Result{Tag: "v0.36.2", Object: newTag},
				},
			}
			got := make(map[string]state.Published)
			for _, entry := range r.observedPublished() {
				if _, exists := got[entry.Ref]; exists {
					t.Fatalf("duplicate published ref %s", entry.Ref)
				}
				got[entry.Ref] = entry
			}
			if len(got) != len(test.want) {
				t.Fatalf("published refs = %#v, want %#v", got, test.want)
			}
			for ref, want := range test.want {
				if got[ref] != want {
					t.Errorf("published %s = %#v, want %#v", ref, got[ref], want)
				}
			}
		})
	}
}
