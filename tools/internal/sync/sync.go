// Package sync orchestrates one complete synchronization.
//
// A synchronization is the whole path from an upstream release to the exact set
// of outward actions publishing it would perform: the module is generated, its
// tree is written into the destination repository, the release commit is
// replayed, the release tag is projected, the resumable state record is stored,
// and the refs that would name all of it are planned. Nothing is pushed.
//
// Planning and publishing are separate on purpose, and the separation is the
// point of the package. Every object a synchronization writes is unreachable
// until a ref names it, so a plan can be computed, compared between machines,
// attached to a review, and thrown away without anything outward having
// happened. Publication is a second call that takes the hash of the plan that
// was approved and refuses anything else.
//
// The package name shadows the standard library's sync. It is the noun this
// engine's operation is called, and no file here needs goroutine primitives; an
// importer that needs both aliases one of them.
package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/release"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/state"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// Destination describes the repository one synchronization writes into and
// would publish to.
type Destination struct {
	// Git is the runner bound to the local destination repository. Every object
	// this run writes is written there, and it is the runner the publication
	// pushes with.
	//
	// It is handed over already built rather than assembled here, because a real
	// publication needs a runner carrying a credential helper and this package
	// must never invent one: it does not know how the operator's token is
	// stored, and a runner it built would either carry no credential or carry one
	// this package had to have been told. A caller publishing over https supplies
	// a runner whose environment already authenticates.
	Git *gitcli.Runner
	// Remote is the push target: an https URL on the publication host, or an
	// absolute path when AllowLocalRemote is set.
	Remote string
	// Identity is the canonical destination repository recorded in every
	// manifest, such as github.com/enj/rbac_authorizer. An https remote derives
	// it; a local remote must state it, because the alternative is a temporary
	// directory in an approved artifact.
	Identity string
	// AllowLocalRemote permits a path or file URL destination. It is off by
	// default so a mistyped configuration cannot publish into a directory, and a
	// local dry run turns it on explicitly.
	AllowLocalRemote bool
	// Lister reads the refs the destination advertises. A nil value with
	// AllowLocalRemote uses the local reader; a nil value without it is a
	// refusal, because no plan can be made without knowing what is published.
	Lister publish.RemoteRefLister
}

// Release is the upstream release one synchronization publishes.
//
// It is carried rather than looked up, because this package writes into the
// destination repository and never opens the source one. Plan reads it out of
// the generation's own source cache and hands it here, which is what keeps the
// destination side of the pipeline testable against a repository a test built.
type Release struct {
	// Tag is the upstream release tag, such as v1.36.1.
	Tag string
	// Ref is the fully qualified source ref the release was proved against, such
	// as refs/tags/v1.36.1. It is recorded as the state anchor's ref.
	Ref string
	// Commit is the exact upstream commit the release was cut from.
	Commit string
	// Tagger is the upstream tag object's tagger. Its raw date is what the
	// destination tag records, so a regenerated tag is byte identical.
	Tagger gitcli.Signature
	// URL is the upstream release page, published verbatim inside a tag object
	// that can never be taken back.
	URL string
	// Author is the upstream author of Commit, carrying the upstream raw author
	// date. The replayed commit preserves it exactly.
	Author gitcli.Signature
	// CommitterDate is the upstream raw committer date of Commit. The replayed
	// commit records the bot as committer and this date, so a rerun reproduces
	// the object name.
	CommitterDate string
	// Message is the complete upstream commit message, replayed as written and
	// extended with exactly one provenance trailer.
	Message string
}

// Module is the generated module a synchronization publishes.
//
// It is the generation's own output rather than a description of it: the files
// are what gets written, and the report is what the manifest summarizes. Taking
// both as one value is what lets the destination half of the pipeline run
// against a module a test composed, without a generation and without the
// upstream repository a generation needs.
type Module struct {
	// Files is the complete generated module.
	Files relocate.FileSet
	// Report is what the generation found. Its digests and summaries travel into
	// the manifest, and nothing in it names a path.
	Report generate.Report
}

// Options describes one complete synchronization, generation included.
type Options struct {
	// Generate is the generation this synchronization publishes. Its Ref must
	// select a release tag, because a release is what gets published and a
	// branch names none.
	Generate generate.Options
	// Destination is where the objects are written and would be published.
	Destination Destination
	// Bot is the identity every object this run writes records. Empty adopts the
	// profile's configured committer.
	Bot config.Identity
	// BotDate is the raw date the state commit records for both of its roles.
	// Empty adopts the upstream tagger's date, which is the honest default: the
	// release is the only reason the record exists.
	BotDate string
	// StateCommit is the previous state record to resume from, empty for a
	// destination that holds none.
	//
	// It is a commit rather than a ref, so a caller that fetched a record does
	// not have to have published it anywhere. A commit and not a tree: the
	// record this run stores takes it as its parent, which is what makes the
	// state branch a history rather than a series of unrelated roots, and a tree
	// cannot be a parent.
	StateCommit string
}

// ProjectOptions describes the destination half of one synchronization: what to
// do with a module that has already been generated.
type ProjectOptions struct {
	// Config is the decoded, validated profile. It decides the destination
	// module and repository, the release policy, the ref layout, and the
	// provenance trailer key.
	Config *config.Config
	// Module is the generated module to publish.
	Module Module
	// Release is the upstream release it was generated from.
	Release Release
	// Destination is where the objects are written and would be published.
	Destination Destination
	// Bot, BotDate, and StateCommit are as they are on Options.
	Bot         config.Identity
	BotDate     string
	StateCommit string
}

// Result is one computed synchronization.
//
// It carries the manifest a person approves and the intermediate results the
// manifest was built from, so a caller that wants to print what the replay or
// the release did is reading the same values the manifest summarizes rather
// than a second rendering of them.
type Result struct {
	// Manifest is the exact outward action set, hashed. It is the artifact an
	// approval names.
	Manifest Manifest
	// Generation is what the generation produced, nil for a Project call that
	// was handed a module directly.
	Generation *generate.Result
	// Tree is the written generated tree.
	Tree treebuild.Manifest
	// Replay is what the replay produced.
	Replay *replay.Result
	// Release is what the release projection produced.
	Release *release.Result
	// Document is the state record this run would publish, and State names the
	// objects it was stored as.
	Document state.Document
	State    state.Record
	// Publish is the ref plan, which Apply executes and nothing else reads.
	Publish *publish.Plan

	// publisher is bound to the destination the plan was made against. It is
	// unexported so an apply cannot be pointed at a repository the plan was never
	// computed for.
	publisher *publish.Publisher
	// trusted is populated only by PlanFinal after discovery and completed-track
	// checks. It enables the post-consumer reconciliation in ApplyTrusted.
	trusted *trustedFinalize
}

// Plan computes one complete synchronization, generation included, and reports
// the exact outward actions publishing it would perform.
//
// Nothing is pushed and no ref is created, moved, or deleted. What the run
// produces is objects that no ref names and a manifest describing which refs
// would name them, which is a statement about a publication rather than a
// publication.
//
// The upstream release metadata is read out of the generation's own source
// cache rather than being asked for, because the two must describe one commit:
// a caller that stated a tagger date belonging to a different release would
// produce a tag object that no rerun reproduces, and the only place both facts
// exist together is the clone the generation just used.
func Plan(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("synchronization: %w", err)
	}
	if opts.Generate.Config == nil {
		return nil, errors.New("synchronization: a profile is required")
	}
	if opts.Generate.Ref.Kind != extract.RefTag {
		return nil, fmt.Errorf("%w: a synchronization publishes a release, and %s names no release",
			ErrUnsupported, opts.Generate.Ref)
	}

	generated, err := generate.Generate(ctx, opts.Generate)
	if err != nil {
		return nil, err
	}

	upstream, err := readRelease(ctx, opts.Generate.Git, sourceCacheDir(opts.Generate), opts.Generate.Config, opts.Generate.Ref.Name)
	if err != nil {
		return nil, err
	}

	result, err := Project(ctx, ProjectOptions{
		Config:      opts.Generate.Config,
		Module:      Module{Files: generated.Files, Report: generated.Report},
		Release:     upstream,
		Destination: opts.Destination,
		Bot:         opts.Bot,
		BotDate:     opts.BotDate,
		StateCommit: opts.StateCommit,
	})
	if result != nil {
		result.Generation = generated
	}
	return result, err
}

// Project turns an already generated module into the objects a destination
// repository would hold and the manifest of refs that would name them.
//
// It is exported because it is the half of a synchronization that has nothing
// to do with upstream. A generation needs a clone of the source project, a Go
// toolchain, and minutes; everything after it needs a module, a release, and a
// repository to write into. Keeping the two callable separately is what lets
// the publication rules be tested against a module a test composed in
// milliseconds, and it is what a resume of an interrupted run would use.
func Project(ctx context.Context, opts ProjectOptions) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("synchronization: %w", err)
	}
	run, err := newRun(ctx, opts)
	if err != nil {
		return nil, err
	}
	return run.execute(ctx)
}

// run holds one synchronization's resolved inputs and the objects it wrote.
type run struct {
	opts   ProjectOptions
	git    *gitcli.Runner
	bot    config.Identity
	date   string
	format gitcli.ObjectFormat
	// publisher is bound to the destination before anything is written, so a
	// destination this run could never have published to costs no objects.
	publisher *publish.Publisher
	// observed is what the destination held when the run read it, by ref name.
	// It is the only thing this run may claim about the remote: everything else
	// it knows is an intention, and a record of intentions is what makes a
	// resume trust a publication that never happened.
	observed map[string]string
	// prior is the record this run resumes from, zero for a destination that
	// holds none. It is loaded before anything is written, because it decides
	// which commit the replayed history attaches to.
	prior state.Document
	// parent is the control-plane commit the generated history attaches to. On
	// the first run it is the setup-derived branch (remote when published, local
	// HEAD during a pre-push rehearsal); later runs recover it from state.
	parent string
	result Result
}

// newRun validates the options, binds the destination, and reads it.
//
// Everything that can refuse is decided here, before a single object is
// written. That ordering is the whole design of this constructor rather than
// tidiness: a run that discovers at publication time that the destination is
// not the profile's repository has already written a tree, a commit, a tag, and
// a state record into somebody's repository, and the tag in particular is an
// object claiming an upstream release. Objects nothing references are cheap,
// but objects nothing references in a repository the operator did not mean to
// touch are still a mess somebody has to reason about.
func newRun(ctx context.Context, opts ProjectOptions) (*run, error) {
	if opts.Config == nil {
		return nil, errors.New("synchronization: a profile is required")
	}
	if opts.Destination.Git == nil {
		return nil, errors.New("synchronization: a destination git runner is required")
	}
	if len(opts.Module.Files.Files) == 0 {
		return nil, errors.New("synchronization: the generated module holds no files")
	}
	if err := validateRelease(opts.Release); err != nil {
		return nil, err
	}
	if err := checkModuleAgrees(opts.Module.Report, opts.Release, opts.Config); err != nil {
		return nil, err
	}
	if err := checkDestination(opts.Destination, opts.Config); err != nil {
		return nil, err
	}

	bot := opts.Bot
	if bot == (config.Identity{}) {
		bot = opts.Config.Commit.Committer
	}
	if bot.Name == "" || bot.Email == "" {
		return nil, errors.New("synchronization: a bot identity with a name and an address is required")
	}
	// The bot date defaults to the upstream tagger's, which is the same rule the
	// release projection applies to its own commit. A run that took it from the
	// clock would write a state commit with a different object name every time,
	// and the property this engine exists to provide is that a rerun which did
	// the same work writes the same objects.
	date := opts.BotDate
	if date == "" {
		date = opts.Release.Tagger.Date
	}
	if date == "" {
		return nil, errors.New("synchronization: the upstream tagger carries no date, so a bot date is required")
	}

	format, err := opts.Destination.Git.ObjectFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("synchronization: %w", err)
	}
	r := &run{opts: opts, git: opts.Destination.Git, bot: bot, date: date, format: format}
	if err := r.bind(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// bind builds the publisher and reads what the destination currently holds.
//
// The read happens here, once, and everything downstream uses it: the state
// record reports it as what was observed, and the publication plan states it as
// what each update expected. Reading twice would let the two disagree, and the
// disagreement would be invisible because each half would be individually
// consistent.
func (r *run) bind(ctx context.Context) error {
	publisher, lister, err := publisherForDestination(ctx, r.git, r.opts.Destination, r.opts.Config, r.format)
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	refs, err := lister.RemoteRefs(ctx, r.opts.Destination.Remote)
	if err != nil {
		return fmt.Errorf("synchronization: read the destination: %w", err)
	}
	observed := make(map[string]string, len(refs))
	for _, ref := range refs {
		observed[ref.Name] = ref.Target
	}
	r.publisher, r.observed = publisher, observed
	return nil
}

func publisherForDestination(ctx context.Context, git *gitcli.Runner, dest Destination, cfg *config.Config, format gitcli.ObjectFormat) (*publish.Publisher, publish.RemoteRefLister, error) {
	lister := dest.Lister
	if lister == nil {
		if !isLocalRemote(dest.Remote) {
			return nil, nil, fmt.Errorf("%w: %w", ErrPublicationDisabled, publish.ErrRemoteRefsUnsupported)
		}
		lister = publish.NewLocalRemote(git)
	}
	publisher, err := publish.New(ctx, git, publish.Options{
		Remote:           dest.Remote,
		Identity:         dest.Identity,
		AllowLocalRemote: dest.AllowLocalRemote,
		Namespaces: publish.Namespaces{
			StateRef:       cfg.Destination.StateRef,
			ProgressPrefix: cfg.Destination.ProgressRefPrefix,
		},
		Lister:       lister,
		ObjectFormat: format,
	})
	if err != nil {
		return nil, nil, err
	}
	return publisher, lister, nil
}

// execute runs the destination half of the pipeline in dependency order.
//
// The order is forced rather than chosen: the record this run resumes from
// decides where the replayed history attaches, the tree has to exist before a
// commit can record it, the commit before a tag can name it, and both before a
// state record can claim they were produced. The publication plan is last
// because it is a statement about objects that already exist.
func (r *run) execute(ctx context.Context) (*Result, error) {
	if err := r.loadPrior(ctx); err != nil {
		return nil, err
	}
	if err := r.resolveParent(ctx); err != nil {
		return nil, err
	}
	if err := r.writeTree(ctx); err != nil {
		return nil, err
	}
	if err := r.replay(ctx); err != nil {
		return nil, err
	}
	if err := r.project(ctx); err != nil {
		return nil, err
	}
	if err := r.record(ctx); err != nil {
		return nil, err
	}
	if err := r.plan(ctx); err != nil {
		return nil, err
	}
	manifest, err := r.manifest()
	if err != nil {
		return nil, err
	}
	r.result.Manifest = manifest
	return &r.result, nil
}

// writeTree writes the generated module as blobs and a tree.
// resolveParent finds the setup/control-plane commit this generated history must
// preserve and descend from.
//
// State is authoritative after the first run. Before state exists, a published
// destination branch is authoritative; a local HEAD is accepted only for the
// deliberate pre-push rehearsal where setup has been committed locally but the
// destination branch does not exist yet. An entirely empty destination is
// refused because a generated-only root would discard soapbox.yaml, patches,
// the pinned shim, and the workflows that make the repository maintainable.
func (r *run) resolveParent(ctx context.Context) error {
	candidate := r.prior.Epoch.Destination
	if candidate == "" {
		candidate = r.observed[r.branchRef()]
	}
	if candidate == "" {
		hasHead, err := r.git.HasHead(ctx)
		if err != nil {
			return fmt.Errorf("synchronization: inspect the setup-derived branch: %w", err)
		}
		if !hasHead {
			return fmt.Errorf("%w: the destination has no setup-derived branch to preserve; run setup, commit its control-plane files, and synchronize from that checkout", ErrUnsupported)
		}
		candidate = "HEAD"
	}
	parent, err := r.git.ResolveCommit(ctx, candidate)
	if err != nil {
		return fmt.Errorf("synchronization: resolve the setup-derived parent %s: %w", candidate, err)
	}
	r.parent = parent
	return nil
}

func (r *run) writeTree(ctx context.Context) error {
	generated, err := composeModuleTree(ctx, r.git, r.opts.Config, r.parent, r.opts.Module.Files)
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	r.result.Tree = generated
	return nil
}

// generatedOwnedPath identifies paths replaced as a unit on every generation.
// Everything else in the parent tree is control-plane or operator-owned content
// and survives the replay. Prefix ownership is needed for shrinking closures:
// retaining only paths present in the new FileSet would leave deleted upstream
// files behind forever.
func generatedOwnedPath(cfg *config.Config, name string) bool {
	switch name {
	case "go.mod", "go.sum", "LICENSE", "NOTICE", "README.md", "doc.go",
		cfg.Facade.File, cfg.Facade.AssertionsFile:
		return true
	}
	prefix := strings.TrimSuffix(cfg.Destination.InternalPrefix, "/")
	return name == prefix || strings.HasPrefix(name, prefix+"/")
}

// checkControlPlane proves the parent is a setup-derived repository rather than
// merely whichever commit happened to be HEAD in the supplied directory.
func composeModuleTree(ctx context.Context, git *gitcli.Runner, cfg *config.Config, parent string, files relocate.FileSet) (treebuild.Manifest, error) {
	parentTree, err := git.ResolveTree(ctx, parent)
	if err != nil {
		return treebuild.Manifest{}, fmt.Errorf("resolve the control-plane tree: %w", err)
	}
	parentFiles, err := git.ListTree(ctx, parentTree)
	if err != nil {
		return treebuild.Manifest{}, fmt.Errorf("read the control-plane tree: %w", err)
	}
	if err := checkControlPlane(parentFiles); err != nil {
		return treebuild.Manifest{}, err
	}

	generated, err := treebuild.WriteFileSet(ctx, git, files)
	if err != nil {
		return treebuild.Manifest{}, err
	}
	generatedPaths := make(map[string]bool, len(generated.Files))
	entries := make([]gitcli.TreeEntry, 0, len(parentFiles)+len(generated.Files))
	for _, file := range generated.Files {
		generatedPaths[file.Path] = true
	}
	for _, file := range parentFiles {
		if generatedPaths[file.Path] || generatedOwnedPath(cfg, file.Path) {
			continue
		}
		entries = append(entries, file)
	}
	for _, file := range generated.Files {
		entries = append(entries, gitcli.TreeEntry{Mode: file.Mode, Object: file.Object, Path: file.Path})
	}
	fullTree, err := git.WriteTree(ctx, entries)
	if err != nil {
		return treebuild.Manifest{}, fmt.Errorf("compose the generated and control-plane trees: %w", err)
	}
	generated.Tree = fullTree
	return generated, nil
}

func checkControlPlane(files []gitcli.TreeEntry) error {
	present := make(map[string]bool, len(files))
	for _, file := range files {
		present[file.Path] = true
	}
	for _, required := range []string{
		".github/workflows/ci.yml",
		".github/workflows/sync.yml",
		"go.mod",
		"soapbox.yaml",
		"tools/cmd/soapbox/main.go",
		"tools/go.mod",
	} {
		if !present[required] {
			return fmt.Errorf("%w: the setup-derived parent does not contain %s", ErrUnsupported, required)
		}
	}
	return nil
}

// replay writes the destination commit the upstream release commit produces.
//
// One commit is replayed, which is the whole of a first epoch: the release is
// the only upstream commit this engine currently transforms, and the generated
// tree is what it transforms into. The replay package is still what writes it,
// rather than a commit assembled here, because the provenance trailer, the
// parent resolution, and the author and committer rules are the published
// history's shape and they must have exactly one implementation.
//
// The commit is replayed as a root of its own epoch. Its upstream parents are
// deliberately absent: they were never transformed, so resolving them would ask
// the mapping for commits it does not hold, and an epoch parent is how a later
// run attaches to history that was actually published.
func (r *run) replay(ctx context.Context) error {
	tree := r.result.Tree.Tree
	result, err := replay.Run(ctx, r.git, replay.Options{
		Commits: []replay.Commit{{
			SHA:           r.opts.Release.Commit,
			Author:        r.opts.Release.Author,
			CommitterDate: r.opts.Release.CommitterDate,
			Message:       r.opts.Release.Message,
		}},
		Epoch: replay.Epoch{
			ProfileHash: r.opts.Module.Report.Engine.ProfileHash,
			Parent:      r.parent,
		},
		Bot:           replay.Identity{Name: r.bot.Name, Email: r.bot.Email},
		ProvenanceKey: r.opts.Config.Commit.TrailerKey,
		Transform: func(_ context.Context, source replay.Commit) (replay.Transformed, error) {
			return replay.Transformed{
				Source:   source.SHA,
				Tree:     tree,
				Changed:  true,
				Evidence: []string{"generated from " + r.opts.Release.Ref},
			}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	if len(result.Heads) != 1 || result.Heads[0].Destination == "" {
		return fmt.Errorf("synchronization: the replay of %s produced no commit", r.opts.Release.Commit)
	}
	r.result.Replay = result
	return nil
}

// epochParent is the destination commit this epoch's history attaches to.
// project writes the release tag object and, if the release content differs
// from the replayed tree, the commit that carries it.
//
// The projection tree is the generated tree. For a single release epoch the two
// are the same thing by construction: the generated module already carries the
// destination dependency versions the release publishes, because there is no
// intermediate commit for it to have carried pseudo-versions from. The release
// projection therefore writes no commit of its own and the tag names the
// replayed commit directly, which is the shape release.Project documents for a
// projection equal to the replayed tree.
func (r *run) project(ctx context.Context) error {
	head := r.result.Replay.Heads[0].Destination
	result, err := release.Project(ctx, r.git, release.Options{
		Policy: r.opts.Config.Release.Policy,
		Source: release.Source{
			Tag:    r.opts.Release.Tag,
			Commit: r.opts.Release.Commit,
			Tagger: r.opts.Release.Tagger,
			URL:    r.opts.Release.URL,
		},
		Replay:     release.Replay{Commit: head, Tree: r.result.Tree.Tree},
		Projection: r.result.Tree.Tree,
		Bot:        release.Identity{Name: r.bot.Name, Email: r.bot.Email},
		BotDate:    r.date,
	})
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	r.result.Release = result
	return nil
}

// record builds and stores the resumable state record.
//
// The document is built from what this run produced rather than from what it
// intended, so a record that reached the destination describes objects that
// exist. It is stored as an unreachable commit like everything else: whether a
// branch should point at it is a publication decision made after every gate has
// passed.
func (r *run) record(ctx context.Context) error {
	head := r.result.Replay.Heads[0].Destination

	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: r.format,
		Destination: state.Destination{
			Repository: r.opts.Config.Destination.Repository,
			Module:     r.opts.Config.Destination.Module,
		},
		Anchor: state.Anchor{Source: r.opts.Release.Commit, Ref: r.opts.Release.Ref},
		Epoch: state.Epoch{
			Profile:     r.opts.Module.Report.Engine.ProfileHash,
			Source:      r.opts.Release.Commit,
			Destination: r.parent,
		},
		Cursors: []state.Cursor{{
			Ref:         r.opts.Release.Ref,
			Source:      r.opts.Release.Commit,
			Destination: head,
		}},
		// Published records what the destination was observed to hold, which is
		// what the field means and the only thing this run may honestly claim. A
		// record listing the refs this run intends to create would be published by
		// the non-consumer push before the consumer push has happened, and a
		// consumer push that then failed would leave a record asserting that a
		// release landed when it did not. A resume would believe it.
		//
		// On a first synchronization the list is therefore empty. Later plans
		// preserve every prior branch and tag claim the current remote read
		// confirmed, then advance a claim only after its consumer push is observed.
		// Tag correspondence is tracked separately from branch correspondence so
		// the annotated tag object and branch commit for one source release can both
		// be recorded without conflating their object identities.
		Published: r.observedPublished(),
		Engine: state.Engine{
			Version:   buildinfo.Version,
			Toolchain: r.opts.Config.Determinism.Toolchain,
		},
	}
	doc, err := state.New(doc)
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	if r.opts.StateCommit != "" {
		if doc, err = state.Merge(ctx, r.prior, doc, r.git); err != nil {
			return fmt.Errorf("synchronization: %w", err)
		}
		// A rerun that found nothing to do produces the record the destination
		// already holds, and appending a second commit saying the same thing
		// would advance the state ref on every scheduled run that changed
		// nothing. The record is reused rather than rewritten, which is what
		// makes a completed synchronization a no-op end to end instead of a
		// publication whose only content is that it happened again.
		if doc.Digest == r.prior.Digest {
			record, err := r.storedRecord(ctx, doc)
			if err != nil {
				return err
			}
			r.result.Document, r.result.State = doc, record
			return nil
		}
	}

	var parents []string
	if r.opts.StateCommit != "" {
		parents = []string{r.opts.StateCommit}
	}
	signature := gitcli.Signature{Name: r.bot.Name, Email: r.bot.Email, Date: r.date}
	record, err := state.Store(ctx, r.git, state.StoreOptions{
		Document:  doc,
		Parents:   parents,
		Author:    signature,
		Committer: signature,
	})
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	r.result.Document, r.result.State = doc, record
	return nil
}

// loadPrior loads the record this run resumes from, and refuses a resume this
// engine cannot honestly perform.
//
// A destination holding a record of a different release is asking for an
// incremental replay: the commits between the recorded anchor and this one have
// to be transformed and attached to published history, and this engine
// transforms exactly one release. Producing something anyway would publish a
// history with a hole in it, so the run stops instead. The same is true of a
// profile change, which re-derives every transformed commit and starts an epoch
// this engine has no way to graft.
//
// It runs before anything is written, so a resume this engine refuses leaves
// the destination repository exactly as it found it.
func (r *run) loadPrior(ctx context.Context) error {
	if r.opts.StateCommit == "" {
		return nil
	}
	prior, err := state.Load(ctx, r.git, r.opts.StateCommit)
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	if prior.Anchor.Source != r.opts.Release.Commit {
		return fmt.Errorf(
			"%w: the destination records release %s and this run publishes %s, which needs the commits between them replayed",
			ErrUnsupported, prior.Anchor.Ref, r.opts.Release.Ref)
	}
	if profile := r.opts.Module.Report.Engine.ProfileHash; prior.Epoch.Profile != profile {
		return fmt.Errorf(
			"%w: the destination records profile %s and this run generated under %s, which starts an epoch this engine cannot graft",
			ErrUnsupported, prior.Epoch.Profile, profile)
	}
	r.prior = prior
	return nil
}

// storedRecord names the objects the record this run resumes from is already
// stored as, and proves they are what they claim.
//
// It reads rather than rewrites. Rewriting would produce the same blob and the
// same tree, because the document is the one that was loaded from this very
// commit, but it would also be a second implementation of how a record is
// stored and the two could drift.
//
// Reading, though, is only safe if what is read is checked. This path reports
// the record in the manifest and plans a state ref at this commit, so a commit
// holding a second file, a non-regular entry, or bytes that decode to a
// different document would be published as this run's record. The checks
// mirror the ones Store makes on the way in: exactly one regular file at the
// record path, holding bytes that decode to the digest the document carries.
func (r *run) storedRecord(ctx context.Context, doc state.Document) (state.Record, error) {
	tree, err := r.git.ResolveTree(ctx, r.opts.StateCommit)
	if err != nil {
		return state.Record{}, fmt.Errorf("synchronization: read the stored record: %w", err)
	}
	entries, err := r.git.ListTree(ctx, tree)
	if err != nil {
		return state.Record{}, fmt.Errorf("synchronization: read the stored record: %w", err)
	}
	if len(entries) != 1 {
		return state.Record{}, fmt.Errorf(
			"synchronization: the stored record %s holds %d files, want exactly one at %s",
			r.opts.StateCommit, len(entries), state.File)
	}
	entry := entries[0]
	if entry.Path != state.File {
		return state.Record{}, fmt.Errorf("synchronization: the stored record %s holds %q, want %s",
			r.opts.StateCommit, entry.Path, state.File)
	}
	if entry.Mode != gitcli.ModeRegular {
		return state.Record{}, fmt.Errorf(
			"synchronization: the stored record %s holds %s as mode %q, want a regular file",
			r.opts.StateCommit, state.File, string(entry.Mode))
	}

	stored, err := r.git.ReadBlob(ctx, gitcli.BlobOptions{Revision: r.opts.StateCommit, Path: state.File})
	if err != nil {
		return state.Record{}, fmt.Errorf("synchronization: read the stored record: %w", err)
	}
	encoded, err := doc.Encode()
	if err != nil {
		return state.Record{}, fmt.Errorf("synchronization: %w", err)
	}
	if !bytes.Equal(stored, encoded) {
		return state.Record{}, fmt.Errorf(
			"synchronization: the stored record %s holds %d bytes and this run's record encodes to %d",
			r.opts.StateCommit, len(stored), len(encoded))
	}
	readBack, err := state.Decode(stored)
	if err != nil {
		return state.Record{}, fmt.Errorf("synchronization: read the stored record: %w", err)
	}
	if readBack.Digest != doc.Digest {
		return state.Record{}, fmt.Errorf(
			"synchronization: the stored record %s holds digest %s and this run's record digests to %s",
			r.opts.StateCommit, readBack.Digest, doc.Digest)
	}
	return state.Record{
		Format: r.format,
		Blob:   entry.Object,
		Tree:   tree,
		Commit: r.opts.StateCommit,
		Digest: doc.Digest,
		Bytes:  int64(len(encoded)),
	}, nil
}

// observedPublished reports the destination refs this run actually read.
//
// The consumer branch is reported when the destination holds it at the commit
// this run can account for. The release tag is reported when the destination
// holds it at the tag object this run projected. Tags are excluded from the
// source-to-destination correspondence check in state.images(), so recording
// both the branch and the tag for the same source commit does not conflict.
func (r *run) observedPublished() []state.Published {
	byRef := make(map[string]state.Published, len(r.prior.Published)+2)

	// Preserve every prior claim that the current remote read confirmed. A new
	// release is planned before its consumer refs move, so the honest state for
	// that plan still describes the previously published branch and tags.
	for _, prior := range r.prior.Published {
		if object, ok := r.observed[prior.Ref]; ok && object == prior.Object {
			byRef[prior.Ref] = prior
		}
	}

	// When a prior consumer push succeeded, the observed branch may already be
	// the newly projected head. Record that advancement instead of the older
	// claim retained above.
	if object, ok := r.observed[r.branchRef()]; ok && object == r.result.Replay.Heads[0].Destination {
		byRef[r.branchRef()] = state.Published{
			Ref:    r.branchRef(),
			Kind:   state.KindBranch,
			Object: object,
			Source: r.opts.Release.Commit,
		}
	}

	tagRef := "refs/tags/" + r.result.Release.Tag
	if object, ok := r.observed[tagRef]; ok && object == r.result.Release.Object {
		byRef[tagRef] = state.Published{
			Ref:    tagRef,
			Kind:   state.KindTag,
			Object: object,
			Source: r.opts.Release.Commit,
		}
	}

	published := make([]state.Published, 0, len(byRef))
	for _, entry := range byRef {
		published = append(published, entry)
	}
	return published
}

// plan asks the publisher what publishing this run's objects would do.
//
// The three refs are stated with their kinds rather than inferred from their
// names, because the state branch is indistinguishable from a consumer branch
// by name alone and getting that wrong would push engine bookkeeping in the
// same atomic batch as a release.
//
// Each update states what the run observed, including that it observed nothing.
// "I saw no such ref" and "I did not look" have to be told apart: only the
// first can be contradicted by the destination, and this run did look, once,
// before it wrote anything. The publisher reads the destination again and
// refuses a plan that disagrees, which turns a destination that moved during
// the run into a refusal rather than into a plan built on a stale read.
func (r *run) plan(ctx context.Context) error {
	head := r.result.Replay.Heads[0].Destination
	updates := []publish.Update{{
		Ref:       r.opts.Config.Destination.StateRef,
		Kind:      publish.KindState,
		NewObject: r.result.State.Commit,
		Evidence:  "state:" + r.opts.Release.Tag,
	}, {
		Ref:       r.branchRef(),
		Kind:      publish.KindBranch,
		NewObject: head,
		Evidence:  "replay:" + r.opts.Release.Ref,
	}, {
		Ref:       "refs/tags/" + r.result.Release.Tag,
		Kind:      publish.KindTag,
		NewObject: r.result.Release.Object,
		Evidence:  "release:" + r.opts.Release.Tag,
	}}
	for i := range updates {
		if object, ok := r.observed[updates[i].Ref]; ok {
			updates[i].ExpectedOld = object
			continue
		}
		updates[i].ExpectAbsent = true
	}
	plan, err := r.publisher.Plan(ctx, updates)
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	r.result.Publish, r.result.publisher = plan, r.publisher
	return nil
}

// branchRef is the fully qualified consumer branch.
//
// The profile records the branch by its short name, because that is how an
// operator writes one and how the destination repository's default is spelled.
// Every ref this package states has to be fully qualified: a publication that
// said "main" would describe whatever the destination happened to resolve that
// to, and a state record recording it would be refused as living outside the
// branch namespace.
func (r *run) branchRef() string {
	return "refs/heads/" + r.opts.Config.Destination.Branch
}

// validateRelease checks the upstream release before anything is written.
func validateRelease(rel Release) error {
	for _, field := range []struct{ name, value string }{
		{"tag", rel.Tag},
		{"ref", rel.Ref},
		{"commit", rel.Commit},
		{"url", rel.URL},
		{"message", rel.Message},
		{"committer date", rel.CommitterDate},
	} {
		if field.value == "" {
			return fmt.Errorf("synchronization: the upstream release %s is required", field.name)
		}
	}
	if !strings.HasPrefix(rel.Ref, "refs/") {
		return fmt.Errorf("synchronization: the upstream release ref %q must be fully qualified", rel.Ref)
	}
	if rel.Author.Name == "" || rel.Author.Email == "" || rel.Author.Date == "" {
		return errors.New("synchronization: the upstream author needs a name, an address, and a date")
	}
	if err := gitcli.ValidateRawDate(rel.Author.Date); err != nil {
		return fmt.Errorf("synchronization: upstream author date: %w", err)
	}
	if err := gitcli.ValidateRawDate(rel.CommitterDate); err != nil {
		return fmt.Errorf("synchronization: upstream committer date: %w", err)
	}
	return nil
}

// sourceCacheDir is the bare source clone a generation used.
//
// The cache root holds one directory per remote rather than being a repository
// itself, so the name has to be derived exactly the way the extraction derives
// it. It is derived rather than reported because a generation's result names
// the root: pointing a runner at the root would let git discover whatever
// repository happens to be above it, and a release read out of the wrong
// repository is a tag object claiming an upstream commit that never produced
// this module.
func sourceCacheDir(opts generate.Options) string {
	remote := opts.SourceRemote
	if remote == "" && opts.Config != nil {
		remote = opts.Config.Source.Repository
	}
	return filepath.Join(opts.CacheRoot, source.CacheDirName(remote))
}

// readRelease reads one upstream release out of a generation's source cache.
//
// The cache is opened through the generation's own anonymous runner, so the
// read talks to the public source host and to nothing else, and it is opened
// read only in the sense that matters: every call here reports objects and none
// writes one.
func readRelease(ctx context.Context, git *gitcli.Runner, cache string, cfg *config.Config, tag string) (Release, error) {
	if git == nil {
		return Release{}, errors.New("synchronization: a source git runner is required")
	}
	source, err := git.WithDir(cache)
	if err != nil {
		return Release{}, fmt.Errorf("synchronization: read release %s: %w", tag, err)
	}
	info, err := source.TagInfo(ctx, tag)
	if err != nil {
		return Release{}, fmt.Errorf("synchronization: read release %s: %w", tag, err)
	}
	commit, err := source.CommitInfo(ctx, info.Target)
	if err != nil {
		return Release{}, fmt.Errorf("synchronization: read release %s: %w", tag, err)
	}
	url, err := releaseURL(cfg.Source.Repository, tag)
	if err != nil {
		return Release{}, err
	}
	return Release{
		Tag:    tag,
		Ref:    "refs/tags/" + tag,
		Commit: commit.SHA,
		Tagger: info.Tagger,
		URL:    url,
		Author: gitcli.Signature{
			Name:  commit.AuthorName,
			Email: commit.AuthorEmail,
			Date:  commit.AuthorDateRaw,
		},
		CommitterDate: commit.CommitterDateRaw,
		Message:       commit.RawMessage,
	}, nil
}

// releaseURL derives the upstream release page from the source repository.
//
// It is derived rather than configured because it is the same page for every
// release of one project and a profile field for it would be a second place the
// host could be wrong. The derivation is deliberately narrow: only an https
// repository produces one, because the value is written verbatim into a tag
// object that can never be taken back.
func releaseURL(repository, tag string) (string, error) {
	trimmed := strings.TrimSuffix(repository, ".git")
	if !strings.HasPrefix(trimmed, "https://") {
		return "", fmt.Errorf(
			"%w: the release page is derived from the source repository, and %q is not an https URL",
			ErrUnsupported, repository)
	}
	return strings.TrimSuffix(trimmed, "/") + "/releases/tag/" + tag, nil
}
