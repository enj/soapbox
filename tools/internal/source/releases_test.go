package source_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/source"
)

func TestDiscoverReleasesOrdersAndFiltersTags(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	tagRelease(ctx, t, up, "v1.35.9", up.base, "1699999900 +0000")
	tagRelease(ctx, t, up, "v1.36.2-alpha.1", up.merge, "1700000100 +0000")
	tagRelease(ctx, t, up, "v1.36.2", up.merge, "1700000200 +0000")
	tagRelease(ctx, t, up, "not-a-release", up.merge, "1700000300 +0000")
	cache := openCache(ctx, t, up)

	for _, test := range []struct {
		name        string
		prereleases bool
		want        []string
	}{
		{name: "stable only", want: []string{"v1.36.1=>v0.36.1", "v1.36.2=>v0.36.2"}},
		{name: "including prereleases", prereleases: true, want: []string{
			"v1.36.1=>v0.36.1", "v1.36.2-alpha.1=>v0.36.2-alpha.1", "v1.36.2=>v0.36.2",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			releases, err := cache.DiscoverReleases(t.Context(), source.ReleaseOptions{
				Minimum:            "v1.36.1",
				IncludePrereleases: test.prereleases,
				Policy:             config.ReleasePolicyV1ToV0,
				Anchor:             up.base,
			})
			if err != nil {
				t.Fatalf("discover releases: %v", err)
			}
			got := make([]string, len(releases))
			for i, release := range releases {
				got[i] = release.Source.Name + "=>" + release.DestinationTag
				if !release.Source.Annotated {
					t.Errorf("release %s is not annotated", release.Source.Name)
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("releases = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDiscoverReleasesFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(context.Context, *testing.T, *upstream)
		opts    func(*upstream) source.ReleaseOptions
		want    string
	}{
		{
			name: "lightweight selected tag",
			arrange: func(ctx context.Context, t *testing.T, up *upstream) {
				up.updateRef(ctx, t, "refs/tags/v1.36.2", up.merge)
			},
			opts: func(up *upstream) source.ReleaseOptions {
				return releaseOptions(up.base)
			},
			want: "is lightweight",
		},
		{
			name: "release outside anchor",
			arrange: func(ctx context.Context, t *testing.T, up *upstream) {
				tree, err := up.repo.Git.ResolveTree(ctx, up.merge)
				if err != nil {
					t.Fatalf("resolve tree: %v", err)
				}
				root, err := up.repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
					Tree: tree, Message: "unrelated release\n",
					Author:    gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000400 +0000"},
					Committer: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000400 +0000"},
				})
				if err != nil {
					t.Fatalf("write unrelated release: %v", err)
				}
				tagRelease(ctx, t, up, "v1.36.2", root, "1700000400 +0000")
			},
			opts: func(up *upstream) source.ReleaseOptions {
				return releaseOptions(up.base)
			},
			want: "does not descend from anchor",
		},
		{
			name: "future unsupported major",
			arrange: func(ctx context.Context, t *testing.T, up *upstream) {
				tagRelease(ctx, t, up, "v2.0.0", up.merge, "1700000500 +0000")
			},
			opts: func(up *upstream) source.ReleaseOptions {
				return releaseOptions(up.base)
			},
			want: "requires a v1 source tag",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			up := newUpstream(ctx, t)
			test.arrange(ctx, t, up)
			cache := openCache(ctx, t, up)
			_, err := cache.DiscoverReleases(ctx, test.opts(up))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDiscoverReleasesHonoursCancellation(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := cache.DiscoverReleases(cancelled, releaseOptions(up.base))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func releaseOptions(anchor string) source.ReleaseOptions {
	return source.ReleaseOptions{
		Minimum:            "v1.36.1",
		IncludePrereleases: true,
		Policy:             config.ReleasePolicyV1ToV0,
		Anchor:             anchor,
	}
}

func tagRelease(ctx context.Context, t *testing.T, up *upstream, name, commit, date string) {
	t.Helper()
	if strings.HasPrefix(name, "v") {
		if _, err := config.ParseSemver(name); err != nil {
			t.Fatalf("test tag %s: %v", name, err)
		}
	}
	if err := up.repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name: name, Commit: commit, Message: "Kubernetes " + name + "\n",
		Tagger: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: date},
	}); err != nil {
		t.Fatalf("create tag %s: %v", name, err)
	}
}
