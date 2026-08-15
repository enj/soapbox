package generate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/source"
)

// runStaging pins every staging module the source commit provides.
//
// The generated module cannot copy the source's staging requirements. Upstream
// requires each of them at a placeholder version and then replaces that
// requirement with a relative directory, so the version upstream writes down
// carries no information and the directory does not exist outside the source
// tree. What the generated module has to name instead is the published staging
// module version that holds the same code, and finding it is what this phase
// does.
//
// Every required staging module is resolved rather than only the ones the
// extracted packages currently import. Tidying is what removes the unused ones,
// and it can only remove a requirement it was given: resolving the subset an
// earlier phase guessed at would let a package that starts importing a staging
// module in a later release fail the run instead of picking up a pin.
func (r *run) runStaging(ctx context.Context) error {
	commit := r.post.Report.Source.Commit

	root, err := r.readRootModule(ctx, commit)
	if err != nil {
		return err
	}
	r.root = root

	staging, cached, err := r.resolveStaging(ctx, root, commit)
	if err != nil {
		return err
	}
	r.staging = staging
	if r.opts.Ref.Kind == extract.RefTag {
		releaseTag, err := config.MapReleaseTag(r.cfg.Release.Policy, r.opts.Ref.Name)
		if err != nil {
			return policyError(stageStaging, err)
		}
		r.report.Source.ReleaseTag = releaseTag
	}
	r.report.recordStaging(root, staging, cached)
	return nil
}

// readRootModule reads the source module's own go.mod at the exact commit.
//
// It is read out of the cache rather than out of the materialized work tree so
// the phase depends on the commit rather than on which files a sparse pattern
// set happened to check out.
//
// The cache is a blobless partial clone, so the blob has to be fetched unless an
// earlier operation already pulled it. An offline run refuses that fetch, which
// makes a cache that does not already hold the file a policy failure rather than
// a phase that silently reaches the network. The runner is anonymous either way,
// so no publishing credential can travel to the source host.
func (r *run) readRootModule(ctx context.Context, commit string) (*gomodmap.RootModule, error) {
	cache, err := r.cacheRunner()
	if err != nil {
		return nil, runtimeError(stageStaging, err)
	}
	// Reading the blob is a cache and transport operation. A commit the cache
	// does not hold, an offline run that may not fetch it, and a cancelled
	// context are all conditions to retry or repair rather than findings about
	// the profile.
	data, err := cache.ReadBlob(ctx, gitcli.BlobOptions{
		Revision:       commit,
		Path:           gomodmap.RootModulePath,
		AllowLazyFetch: !r.opts.Offline,
	})
	if err != nil {
		return nil, runtimeError(stageStaging, fmt.Errorf("root module at %s: %w", commit, err))
	}
	// Parsing is where the answer becomes semantic: a root go.mod this engine
	// refuses is a statement about the source layout the profile describes.
	root, err := gomodmap.ParseRootModule(gomodmap.RootModulePath, data)
	if err != nil {
		return nil, policyError(stageStaging, fmt.Errorf("root module at %s: %w", commit, err))
	}
	// The profile's import prefix is what every relocated path and every facade
	// source is spelled against, so a source module that calls itself something
	// else means the profile describes a different repository than the one that
	// was read.
	if want := r.cfg.Source.ImportPrefix; root.Path != want {
		return nil, policyError(stageStaging, fmt.Errorf("the source commit declares module %s but the profile is written against %s", root.Path, want))
	}
	return root, nil
}

// resolveStaging pins the required staging modules, reusing the index when it
// already holds the answer for this source commit.
//
// The cache is keyed by source commit because that is what determines the
// answer: one source commit always maps onto the same staging commits and
// therefore onto the same versions. Reusing it is what makes a replay over many
// releases affordable, and it is also what lets an offline run produce a module
// whose pins an earlier online run established.
func (r *run) resolveStaging(ctx context.Context, root *gomodmap.RootModule, commit string) ([]gomodmap.ModuleVersion, bool, error) {
	paths := requiredStagingPaths(root)
	if len(paths) == 0 {
		return nil, false, policyError(stageStaging, fmt.Errorf("the source commit stages no module the root module requires, so there is nothing the generated module could pin"))
	}

	// The index is a cache on disk. Failing to open, read, or write it says
	// nothing about the profile, so it is a runtime failure an operator fixes by
	// repairing or deleting the file.
	if err := os.MkdirAll(filepath.Dir(r.opts.StorePath), 0o750); err != nil {
		return nil, false, runtimeError(stageStaging, fmt.Errorf("version index directory: %w", err))
	}
	store, err := gomodmap.NewStore(r.opts.StorePath)
	if err != nil {
		return nil, false, runtimeError(stageStaging, fmt.Errorf("version index: %w", err))
	}
	index, err := store.Load(ctx)
	if errors.Is(err, gomodmap.ErrIndexMissing) {
		index = gomodmap.NewIndex()
	} else if err != nil {
		return nil, false, runtimeError(stageStaging, fmt.Errorf("version index: %w", err))
	}

	tag := ""
	switch r.opts.Ref.Kind {
	case extract.RefTag:
		tag = r.opts.Ref.Name
	case extract.RefCommit:
		cache, err := r.cacheRunner()
		if err != nil {
			return nil, false, runtimeError(stageStaging, err)
		}
		if err := r.verifyIntermediateBounds(ctx, cache.WithNoLazyFetch(), commit); err != nil {
			return nil, false, policyError(stageStaging, err)
		}
	}
	if entry, ok := index.Lookup(commit); ok {
		if err := checkCachedEntry(entry, tag, paths); err != nil {
			return nil, false, policyError(stageStaging, fmt.Errorf("version index: %w", err))
		}
		return entry.Modules, true, nil
	}

	if r.opts.Offline {
		// An offline run with a cold index cannot resolve anything, which is a
		// condition of this run rather than a property of the profile.
		return nil, false, runtimeError(stageStaging, fmt.Errorf("the version index holds no entry for source commit %s and the run is offline, so no staging version can be resolved", commit))
	}
	var versions []gomodmap.ModuleVersion
	if r.opts.Ref.Kind == extract.RefCommit {
		versions, err = r.resolveIntermediateStaging(ctx, root, commit, paths)
	} else {
		resolver, resolverErr := r.resolverRunner(ctx, root)
		if resolverErr != nil {
			return nil, false, runtimeError(stageStaging, resolverErr)
		}
		// Resolution mixes proxy round trips with checks on what came back, so the
		// two are separated by the sentinel the resolver chose rather than by the
		// fact that resolution failed.
		versions, err = gomodmap.ResolveReleaseVersions(ctx, resolver, r.cfg.Release.Policy, tag, paths)
	}
	if err != nil {
		return nil, false, classify(stageStaging, err, stagingSemantic...)
	}

	if err := index.Put(gomodmap.Entry{Source: commit, Tag: tag, Modules: versions}); err != nil {
		return nil, false, runtimeError(stageStaging, fmt.Errorf("version index: %w", err))
	}
	if err := store.Save(ctx, index); err != nil {
		return nil, false, runtimeError(stageStaging, fmt.Errorf("version index: %w", err))
	}
	return versions, false, nil
}

// resolveIntermediateStaging maps one exact source commit through the publishing
// histories of every required staging module, then asks Go to name the mapped
// commits. Neither pseudo-version text nor a staging commit is constructed by
// the engine.
func (r *run) resolveIntermediateStaging(ctx context.Context, root *gomodmap.RootModule, commit string, paths []string) ([]gomodmap.ModuleVersion, error) {
	sources := r.opts.StagingSources
	if len(sources) == 0 && !localSourceRemote(r.opts.SourceRemote) {
		sources = make(map[string]StagingSource, len(paths))
		for _, modulePath := range paths {
			remote, err := gomodmap.StagingRepository(modulePath)
			if err != nil {
				return nil, err
			}
			sources[modulePath] = StagingSource{Remote: remote}
		}
	}
	if len(sources) != len(paths) {
		return nil, fmt.Errorf("intermediate staging sources name %d modules, the source commit requires %d", len(sources), len(paths))
	}
	for modulePath := range sources {
		if !slices.Contains(paths, modulePath) {
			return nil, fmt.Errorf("intermediate staging source %s is not required by the source commit", modulePath)
		}
	}

	sourceGit, err := r.cacheRunner()
	if err != nil {
		return nil, err
	}
	mainline, err := gomodmap.NewSourceMainline(ctx, sourceGit.WithNoLazyFetch(), gomodmap.MainlineOptions{
		Revision: commit, Anchor: r.opts.HistoryAnchor,
	})
	if err != nil {
		return nil, err
	}
	stagingTag, err := config.MapReleaseTag(r.cfg.Release.Policy, r.opts.ReleaseContext)
	if err != nil {
		return nil, err
	}
	stagingAnchorTag, err := config.MapReleaseTag(r.cfg.Release.Policy, r.opts.HistoryAnchorRelease)
	if err != nil {
		return nil, err
	}

	mappings := make([]gomodmap.CommitMapping, 0, len(paths))
	for _, modulePath := range paths {
		spec := sources[modulePath]
		if err := r.checkStagingSource(modulePath, spec); err != nil {
			return nil, err
		}
		cache, err := source.Open(ctx, source.Options{
			Remote:       spec.Remote,
			CacheRoot:    r.opts.CacheRoot,
			WorktreeRoot: filepath.Join(r.opts.WorkRoot, "staging-worktrees"),
			Git:          r.opts.Git,
		})
		if err != nil {
			return nil, fmt.Errorf("staging module %s: %w", modulePath, err)
		}
		tags := []string{stagingAnchorTag, stagingTag}
		if stagingAnchorTag == stagingTag {
			tags = tags[:1]
		}
		if err := cache.Fetch(ctx, source.Refs{Tags: tags}); err != nil {
			return nil, fmt.Errorf("staging module %s: fetch release bounds %v: %w", modulePath, tags, err)
		}
		revisions, err := cache.Resolve(ctx, source.Refs{Tags: tags})
		if err != nil {
			return nil, fmt.Errorf("staging module %s: resolve release bounds %v: %w", modulePath, tags, err)
		}
		if len(revisions) != len(tags) {
			return nil, fmt.Errorf("staging module %s: resolved %d revisions for %d release bounds", modulePath, len(revisions), len(tags))
		}
		byTag := make(map[string]source.Revision, len(revisions))
		for _, revision := range revisions {
			byTag[revision.Name] = revision
		}
		index, err := gomodmap.NewStagingIndex(ctx, cache.Git().WithNoLazyFetch(), gomodmap.IndexOptions{
			ModulePath: modulePath,
			Revision:   byTag[stagingTag].Commit,
			Anchor:     byTag[stagingAnchorTag].Commit,
		})
		if err != nil {
			return nil, err
		}
		mapping, err := index.Map(mainline)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}

	resolver, err := r.resolverRunner(ctx, root)
	if err != nil {
		return nil, err
	}
	return gomodmap.ResolveCommitVersions(ctx, resolver, mappings)
}

func (r *run) verifyIntermediateBounds(ctx context.Context, git *gitcli.Runner, commit string) error {
	anchor, err := cachedAnnotatedTag(ctx, git, r.opts.HistoryAnchorRelease)
	if err != nil {
		return err
	}
	if anchor.Commit != r.opts.HistoryAnchor {
		return fmt.Errorf("intermediate history anchor tag %s resolves to %s, configured anchor is %s", r.opts.HistoryAnchorRelease, anchor.Commit, r.opts.HistoryAnchor)
	}
	bound, err := cachedAnnotatedTag(ctx, git, r.opts.ReleaseContext)
	if err != nil {
		return err
	}
	withinLower, err := git.IsAncestor(ctx, anchor.Commit, commit)
	if err != nil {
		return fmt.Errorf("intermediate history lower bound: %w", err)
	}
	withinUpper, err := git.IsAncestor(ctx, commit, bound.Commit)
	if err != nil {
		return fmt.Errorf("intermediate history upper bound: %w", err)
	}
	if !withinLower || !withinUpper {
		return fmt.Errorf("intermediate source commit %s is not between %s at %s and %s at %s", commit, r.opts.HistoryAnchorRelease, anchor.Commit, r.opts.ReleaseContext, bound.Commit)
	}
	return nil
}

func cachedAnnotatedTag(ctx context.Context, git *gitcli.Runner, tag string) (gitcli.Ref, error) {
	name := "refs/tags/" + tag
	refs, err := git.ListRefs(ctx, name)
	if err != nil {
		return gitcli.Ref{}, fmt.Errorf("intermediate source tag %s: %w", tag, err)
	}
	if len(refs) != 1 || refs[0].Name != name {
		return gitcli.Ref{}, fmt.Errorf("intermediate source tag %s has %d exact cached refs, want 1", tag, len(refs))
	}
	if !refs[0].Annotated() {
		return gitcli.Ref{}, fmt.Errorf("intermediate source tag %s is not annotated", tag)
	}
	return refs[0], nil
}

func (r *run) checkStagingSource(modulePath string, spec StagingSource) error {
	if spec.Remote == "" {
		return fmt.Errorf("intermediate staging source %s has no repository", modulePath)
	}
	if localSourceRemote(r.opts.SourceRemote) && localSourceRemote(spec.Remote) {
		return nil
	}
	expected, err := gomodmap.StagingRepository(modulePath)
	if err != nil {
		return err
	}
	if spec.Remote != expected {
		return fmt.Errorf("intermediate staging source %s repository %q does not match canonical %q", modulePath, spec.Remote, expected)
	}
	return nil
}

func localSourceRemote(remote string) bool {
	if filepath.IsAbs(remote) {
		return true
	}
	parsed, err := url.Parse(remote)
	return err == nil && parsed.Scheme == "file"
}

// checkCachedEntry proves a cached answer is the answer this run is asking for.
//
// A hit is keyed on the source commit alone, so the entry still has to agree
// about the release it describes and about which modules were staged. An entry
// recorded for a different tag, or one recorded before upstream staged a module
// this run needs, would otherwise silently pin fewer modules than the generated
// module requires.
func checkCachedEntry(entry gomodmap.Entry, tag string, paths []string) error {
	if entry.Tag != tag {
		return fmt.Errorf("the cached entry for this source commit describes release %q rather than %q", entry.Tag, tag)
	}
	cached := make([]string, len(entry.Modules))
	for i, module := range entry.Modules {
		cached[i] = module.Path
	}
	slices.Sort(cached)
	if !slices.Equal(cached, paths) {
		return fmt.Errorf("the cached staging modules %v do not match the source commit's required modules %v, so the index predates a change to the staging layout", cached, paths)
	}
	return nil
}

// requiredStagingPaths lists the staging modules the root module actually
// requires, sorted.
//
// A staged module with no requirement is skipped rather than resolved. Upstream
// stages modules it publishes but never builds against, so it resolved no
// version for them, and turning such a replacement into a requirement would have
// the generated module depend on code the source commit was never built against
// at a version this engine chose rather than one upstream did.
func requiredStagingPaths(root *gomodmap.RootModule) []string {
	paths := make([]string, 0, len(root.Staging))
	for _, module := range root.Staging {
		if module.Required {
			paths = append(paths, module.Path)
		}
	}
	slices.Sort(paths)
	return paths
}

// resolverRunner prepares the isolated scratch module the version resolver runs
// in.
//
// go list -m answers in the context of a main module, so the module the resolver
// sits in is part of the question rather than a detail of where it runs. The
// scratch module therefore declares nothing: no requirement to raise a version
// through selection, no replacement to redirect a query onto a directory, and no
// exclusion to remove a version from consideration. Its path is under a reserved
// invalid domain so it can never be one of the modules being asked about.
//
// The language version is the source module's rather than the engine's, because
// the go command reads it when deciding how to interpret a module graph, and a
// resolver running under different language semantics than the module being
// generated is answering a slightly different question.
func (r *run) resolverRunner(ctx context.Context, root *gomodmap.RootModule) (*gocli.Runner, error) {
	dir := r.paths.Resolver
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("resolver module: %w", err)
	}
	goDirective := root.Go
	if goDirective == "" {
		return nil, fmt.Errorf("resolver module: the source commit declares no go directive, so the resolver has no language version to run under")
	}
	contents := fmt.Sprintf("module %s\n\ngo %s\n", resolverModulePath, goDirective)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(contents), 0o600); err != nil {
		return nil, fmt.Errorf("resolver module: %w", err)
	}
	runner, err := r.opts.Go.WithDir(dir)
	if err != nil {
		return nil, fmt.Errorf("resolver module: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return runner, nil
}
