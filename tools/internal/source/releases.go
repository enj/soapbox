package source

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// ReleaseOptions selects the upstream release tags an unattended run tracks.
type ReleaseOptions struct {
	// Minimum is the first source release the profile permits.
	Minimum string
	// IncludePrereleases includes semver prerelease tags after Minimum.
	IncludePrereleases bool
	// Policy maps source tags onto destination module tags.
	Policy string
	// Anchor, when present, must be an ancestor of every selected release.
	Anchor string
}

// Release is one selected, verified source release and its destination tag.
type Release struct {
	Source         Revision
	DestinationTag string
}

// DiscoverReleases selects verified semantic release tags from the source cache.
//
// Non-semantic tags are unrelated upstream refs and are ignored. A semantic tag
// inside the selected range is part of the release stream: if it is lightweight,
// outside the anchor, or cannot map under the configured policy, discovery fails
// rather than silently skipping a release an unattended run should have seen.
func (c *Cache) DiscoverReleases(ctx context.Context, opts ReleaseOptions) ([]Release, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("discover source releases: %w", err)
	}
	if c == nil || c.git == nil {
		return nil, errors.New("discover source releases: no source cache")
	}
	minimum, err := config.ParseSemver(opts.Minimum)
	if err != nil {
		return nil, fmt.Errorf("discover source releases: minimum release: %w", err)
	}
	if _, err := config.MapReleaseTag(opts.Policy, minimum.String()); err != nil {
		return nil, fmt.Errorf("discover source releases: minimum release: %w", err)
	}
	if opts.Anchor != "" {
		if err := gitgraph.ValidateSHA(opts.Anchor); err != nil {
			return nil, fmt.Errorf("discover source releases: anchor: %w", err)
		}
	}

	tags, err := c.ListTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover source releases: %w", err)
	}
	selected := make([]Release, 0, len(tags))
	seenDestination := make(map[string]string)
	for _, tag := range tags {
		version, err := config.ParseSemver(tag.Name)
		if err != nil {
			continue
		}
		if semver.Compare(version.String(), minimum.String()) < 0 {
			continue
		}
		if version.Prerelease != "" && !opts.IncludePrereleases {
			continue
		}
		if !tag.Annotated {
			return nil, fmt.Errorf("discover source releases: tag %s is lightweight, so it carries no reproducible tagger date", tag.Name)
		}
		destination, err := config.MapReleaseTag(opts.Policy, tag.Name)
		if err != nil {
			return nil, fmt.Errorf("discover source releases: tag %s: %w", tag.Name, err)
		}
		if previous, exists := seenDestination[destination]; exists {
			return nil, fmt.Errorf("discover source releases: tags %s and %s both map to %s", previous, tag.Name, destination)
		}
		if opts.Anchor != "" {
			descends, err := c.git.IsAncestor(ctx, opts.Anchor, tag.Commit)
			if err != nil {
				return nil, fmt.Errorf("discover source releases: tag %s ancestry: %w", tag.Name, err)
			}
			if !descends {
				return nil, fmt.Errorf("discover source releases: tag %s at %s does not descend from anchor %s", tag.Name, tag.Commit, opts.Anchor)
			}
		}
		seenDestination[destination] = tag.Name
		selected = append(selected, Release{Source: tag, DestinationTag: destination})
	}
	slices.SortFunc(selected, func(a, b Release) int {
		if order := semver.Compare(a.Source.Name, b.Source.Name); order != 0 {
			return order
		}
		return strings.Compare(a.Source.Ref, b.Source.Ref)
	})
	return selected, nil
}
