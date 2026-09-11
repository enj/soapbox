package gomodmap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/gocli"
)

// ErrUnresolvedModule reports a staging module version the go command could not
// resolve.
var ErrUnresolvedModule = errors.New("staging module version did not resolve")

// ErrVersionMismatch reports a resolved version that does not name what was
// asked for. It is the check that makes asking the toolchain safer than
// computing the answer here.
var ErrVersionMismatch = errors.New("resolved version does not name the requested commit")

// ResolveReleaseVersions pins the staging modules of a tagged Kubernetes
// release.
//
// At a release tag the answer is arithmetic rather than a search: the staging
// repositories are tagged in lockstep with the release, so v1.36.1 of the source
// is v0.36.1 of every staging module. The mapping is still put to the go command
// rather than assumed, because a tag this engine believes in but that was never
// pushed would otherwise become a pin nothing can resolve.
//
// The runner must sit in an isolated scratch module: see checkResolverModule,
// which refuses anything else before a version is resolved.
func ResolveReleaseVersions(ctx context.Context, runner *gocli.Runner, policy, sourceTag string, modulePaths []string) ([]ModuleVersion, error) {
	tag, err := config.MapReleaseTag(policy, sourceTag)
	if err != nil {
		return nil, fmt.Errorf("staging versions at %s: %w", sourceTag, err)
	}
	paths, err := uniqueSorted(modulePaths)
	if err != nil {
		return nil, fmt.Errorf("staging versions at %s: %w", sourceTag, err)
	}

	queries := make([]string, len(paths))
	for i, modulePath := range paths {
		queries[i] = modulePath + "@" + tag
	}
	resolved, err := resolveQueries(ctx, runner, paths, queries)
	if err != nil {
		return nil, fmt.Errorf("staging versions at %s: %w", sourceTag, err)
	}

	versions := make([]ModuleVersion, len(paths))
	for i, modulePath := range paths {
		found := resolved[modulePath]
		// The go command answers a tag query with the tag's own version, so
		// anything else means the tag names something other than the release.
		if found.Version != tag {
			return nil, fmt.Errorf("staging versions at %s: %w: %s@%s resolved to %s", sourceTag, ErrVersionMismatch, modulePath, tag, found.Version)
		}
		commit, err := assertNamesTag(found, tag)
		if err != nil {
			return nil, fmt.Errorf("staging versions at %s: %w", sourceTag, err)
		}
		versions[i] = ModuleVersion{Path: modulePath, Version: tag, Commit: commit}
	}
	return versions, nil
}

// tagRefPrefix is where an annotated or lightweight tag lives in a repository.
const tagRefPrefix = "refs/tags/"

// assertNamesTag checks that a resolved release version was found through the
// tag it was asked for, and reports the commit that tag names.
//
// The version alone does not establish this. A module whose repository moved a
// tag, or a proxy answering from a stale record, can report the version that was
// requested while having resolved it through some other reference, and the
// generated module would then pin a release built from code the tag no longer
// names.
//
// Both ends of the reference are checked. The prefix has to be refs/tags/,
// because a branch is mutable and a release pin resolved through one is not
// reproducible. The suffix has to be the tag itself, compared with a separator
// so that v0.36.1 does not match v1.0.36.1, and left as a suffix rather than a
// whole-string comparison because a module published from a subdirectory carries
// a tag prefix ahead of the version.
func assertNamesTag(resolved gocli.Module, tag string) (string, error) {
	origin, err := originOf(resolved)
	if err != nil {
		return "", err
	}
	ref := tagRefPrefix + tag
	if origin.VCS != "git" || origin.Subdir != "" || origin.Ref != ref {
		return "", fmt.Errorf(
			"%w: %s %s was resolved through VCS %q, subdirectory %q, and ref %q rather than root Git tag %s",
			ErrVersionMismatch, resolved.Path, resolved.Version, origin.VCS, origin.Subdir, origin.Ref, ref,
		)
	}
	return origin.Hash, nil
}

// ResolveCommitVersions pins the staging modules of an intermediate source
// commit.
//
// Between releases a module is normally asked for by the staging commit it was
// mapped onto and the go command answers with the version that names it. When an
// adjacent-release mapping proves the previous tag package-equivalent, it instead
// asks for that exact tag and proves the tag still names the carried commit.
// Versions are never assembled here: every answer and origin
// comes from the Go command and is checked against the mapped object.
//
// The answer is then checked against the commit it was supposed to describe,
// both through the revision the version itself encodes and through the version
// control origin the go command reports, so a version resolved from some other
// commit is caught here rather than published.
//
// Every mapping has to name the same source commit, and the versions returned
// carry no source of their own. They are recorded under one source commit, so a
// set drawn from two of them would cache one commit's staging versions under
// another commit's name, and nothing downstream holds enough to notice.
//
// The runner must sit in an isolated scratch module: see checkResolverModule,
// which refuses anything else before a version is resolved.
func ResolveCommitVersions(ctx context.Context, runner *gocli.Runner, mappings []CommitMapping) ([]ModuleVersion, error) {
	if len(mappings) == 0 {
		return nil, errors.New("staging versions: at least one mapped module is required")
	}
	ordered := slices.Clone(mappings)
	slices.SortFunc(ordered, func(a, b CommitMapping) int { return strings.Compare(a.ModulePath, b.ModulePath) })

	// The shared source commit is validated once rather than only compared, so a
	// set of mappings that agree on an unusable name is refused here instead of
	// becoming the key an entry is cached under.
	source := ordered[0].Source
	if err := gitgraph.ValidateSHA(source); err != nil {
		return nil, fmt.Errorf("staging versions: source commit: %w", err)
	}

	paths := make([]string, len(ordered))
	queries := make([]string, len(ordered))
	for i, mapping := range ordered {
		if i > 0 && ordered[i-1].ModulePath == mapping.ModulePath {
			return nil, fmt.Errorf("staging versions: module %s is mapped twice", mapping.ModulePath)
		}
		if mapping.Source != source {
			return nil, fmt.Errorf("staging versions: module %s is mapped from source %s rather than %s", mapping.ModulePath, mapping.Source, source)
		}
		if mapping.Staging == "" {
			return nil, fmt.Errorf("staging versions: module %s has no mapped commit", mapping.ModulePath)
		}
		if mapping.Carried && mapping.Version == "" {
			return nil, fmt.Errorf("staging versions: module %s is carried without an exact previous version", mapping.ModulePath)
		}
		if !mapping.Carried && mapping.Version != "" {
			return nil, fmt.Errorf("staging versions: module %s has carried version %s without a carried mapping", mapping.ModulePath, mapping.Version)
		}
		query := mapping.Staging
		if mapping.Version != "" {
			if err := ValidateExactVersion(mapping.Version); err != nil {
				return nil, fmt.Errorf("staging versions: module %s carried version: %w", mapping.ModulePath, err)
			}
			query = mapping.Version
		}
		paths[i] = mapping.ModulePath
		queries[i] = mapping.ModulePath + "@" + query
	}

	resolved, err := resolveQueries(ctx, runner, paths, queries)
	if err != nil {
		return nil, fmt.Errorf("staging versions: %w", err)
	}

	versions := make([]ModuleVersion, len(ordered))
	for i, mapping := range ordered {
		found := resolved[mapping.ModulePath]
		if mapping.Version == "" {
			if err := assertNamesCommit(found, mapping.Staging); err != nil {
				return nil, fmt.Errorf("staging versions: module %s at %s: %w", mapping.ModulePath, mapping.Staging, err)
			}
		} else {
			if found.Version != mapping.Version {
				return nil, fmt.Errorf("staging versions: %w: %s@%s resolved to %s", ErrVersionMismatch, mapping.ModulePath, mapping.Version, found.Version)
			}
			commit, err := assertNamesTag(found, mapping.Version)
			if err != nil {
				return nil, fmt.Errorf("staging versions: module %s at %s: %w", mapping.ModulePath, mapping.Version, err)
			}
			if commit != mapping.Staging {
				return nil, fmt.Errorf("staging versions: %w: %s@%s resolves to %s, mapped commit is %s", ErrVersionMismatch, mapping.ModulePath, mapping.Version, commit, mapping.Staging)
			}
		}
		versions[i] = ModuleVersion{
			Path:    mapping.ModulePath,
			Version: found.Version,
			Commit:  mapping.Staging,
		}
	}
	return versions, nil
}

// assertNamesCommit checks that a resolved version really describes one commit.
//
// Origin.Hash is the gate. It is the go command's own record of the revision it
// resolved, it is a full object name, and comparing it against the requested
// commit is the direct proof that the toolchain answered about what it was asked
// about.
//
// The form of the version is deliberately not part of that gate. A mapped
// staging commit can be exactly the commit a staging repository tagged, in which
// case the go command answers a commit query with the tag rather than with a
// pseudo-version. That is a correct answer naming the requested commit, and
// refusing it would fail intermediate mapping for repositories that tag often,
// kubernetes/kms among them. When the answer is a pseudo-version its embedded
// revision is checked too, because that abbreviated name is what gets written
// into the generated go.mod while the origin stays behind in the resolution.
func assertNamesCommit(resolved gocli.Module, commit string) error {
	if err := ValidateExactVersion(resolved.Version); err != nil {
		return fmt.Errorf("%w: %w", ErrVersionMismatch, err)
	}
	origin, err := originOf(resolved)
	if err != nil {
		return err
	}
	if origin.Hash != commit {
		return fmt.Errorf("%w: %s was resolved from %s", ErrVersionMismatch, resolved.Version, origin.Hash)
	}
	if !module.IsPseudoVersion(resolved.Version) {
		return nil
	}
	revision, err := module.PseudoVersionRev(resolved.Version)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrVersionMismatch, resolved.Version, err)
	}
	if revision == "" || !strings.HasPrefix(commit, revision) {
		return fmt.Errorf("%w: %s names revision %s", ErrVersionMismatch, resolved.Version, revision)
	}
	return nil
}

// originOf reports the version control provenance of a resolved version.
//
// A missing origin is refused rather than tolerated. The go command reports one
// for every version it resolves from version control, so its absence means the
// answer came from somewhere that cannot say which commit it describes, and a
// pin this engine cannot tie back to a commit is one nothing can reproduce.
// Publication is append-only, so an unverifiable pin has to fail here rather
// than become permanent.
//
// The revision is required to be a full object name, because it is recorded
// alongside the version and is what a later run re-checks the pin against. An
// abbreviated one would name whichever commit happened to share its prefix.
func originOf(resolved gocli.Module) (*gocli.ModuleOrigin, error) {
	if resolved.Origin == nil {
		return nil, fmt.Errorf("%w: %s %s reported no version control origin", ErrVersionMismatch, resolved.Path, resolved.Version)
	}
	if resolved.Origin.Hash == "" {
		return nil, fmt.Errorf("%w: %s %s reported an origin with no revision", ErrVersionMismatch, resolved.Path, resolved.Version)
	}
	if err := gitgraph.ValidateSHA(resolved.Origin.Hash); err != nil {
		return nil, fmt.Errorf("%w: %s %s: origin: %w", ErrVersionMismatch, resolved.Path, resolved.Version, err)
	}
	return resolved.Origin, nil
}

// resolveQueries runs one batched resolution and indexes the answer by module
// path.
//
// Batching is what keeps a replay affordable: resolving a staging module per
// release means dozens of queries, and the go command amortises proxy round
// trips across a single invocation.
func resolveQueries(ctx context.Context, runner *gocli.Runner, paths, queries []string) (map[string]gocli.Module, error) {
	if err := checkResolverModule(runner, paths); err != nil {
		return nil, err
	}
	modules, err := runner.ListModules(ctx, queries...)
	if err != nil {
		return nil, err
	}
	return indexResolved(paths, modules)
}

// checkResolverModule refuses a runner whose own module could answer for the
// modules being resolved.
//
// go list -m answers in the context of a main module, so the module the runner
// sits in is part of the question rather than a detail of where it runs. A
// replace directive there redirects a version query onto a directory, an exclude
// removes a version from selection, and a main module that is itself one of the
// modules being asked about answers out of its own working tree.
//
// indexResolved catches the replacement and the main module as symptoms, but
// only once a query has already gone out, and an exclude leaves no trace in the
// response at all: the answer is simply a different version, indistinguishable
// from the one the proxy would otherwise have given. So the requirement is
// stated up front instead. The resolver runs in an isolated scratch module that
// declares nothing, and a runner pointed anywhere else is refused before a
// single version is resolved.
func checkResolverModule(runner *gocli.Runner, paths []string) error {
	dir := runner.Dir()
	if dir == "" {
		return errors.New("resolver module: the runner needs a scratch module directory")
	}
	// The path is the runner's own working directory rather than anything an
	// input named, and it is the file the go command is about to read as its main
	// module.
	data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // the directory is the runner's own
	if err != nil {
		return fmt.Errorf("resolver module: %w", err)
	}
	parsed, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return fmt.Errorf("resolver module %s: %w", dir, err)
	}
	if parsed.Module == nil {
		return fmt.Errorf("resolver module %s: a module directive is required", dir)
	}
	if len(parsed.Replace) > 0 {
		return fmt.Errorf("resolver module %s: carries %d replace directives, which would resolve a staging version out of a directory", dir, len(parsed.Replace))
	}
	if len(parsed.Exclude) > 0 {
		return fmt.Errorf("resolver module %s: carries %d exclude directives, which silently change which version a query selects", dir, len(parsed.Exclude))
	}
	if main := parsed.Module.Mod.Path; slices.Contains(paths, main) {
		return fmt.Errorf("resolver module %s: is itself %s, which it would answer for out of its own tree", dir, main)
	}
	return nil
}

// indexResolved matches a batched response back onto the modules it was asked
// about.
//
// The match is by module path rather than by position, because a query does not
// always produce exactly one record and the go command groups the response its
// own way. Every requested module has to appear exactly once with a usable
// version: a module the go command could not resolve is reported in its own
// Error field rather than by failing the batch, so a caller that did not read it
// would treat a failed resolution as an answer.
func indexResolved(paths []string, modules []gocli.Module) (map[string]gocli.Module, error) {
	found := make(map[string]gocli.Module, len(modules))
	for _, resolved := range modules {
		if resolved.Error != nil {
			return nil, fmt.Errorf("%w: %s: %s", ErrUnresolvedModule, resolved.Path, resolved.Error.Err)
		}
		if existing, duplicate := found[resolved.Path]; duplicate {
			return nil, fmt.Errorf("%w: %s resolved to both %s and %s", ErrUnresolvedModule, resolved.Path, existing.Version, resolved.Version)
		}
		found[resolved.Path] = resolved
	}
	for _, modulePath := range paths {
		resolved, ok := found[modulePath]
		if !ok {
			return nil, fmt.Errorf("%w: %s is missing from the response", ErrUnresolvedModule, modulePath)
		}
		// A staging module answered as the main module, or answered through a
		// replacement, is the caller's module context substituting itself for the
		// published module being resolved. Either would pin a version that names
		// code from the resolving module's own directory rather than from the
		// staging repository.
		if resolved.Main {
			return nil, fmt.Errorf("%w: %s answered as the main module of the resolving directory", ErrVersionMismatch, modulePath)
		}
		if resolved.Replace != nil {
			return nil, fmt.Errorf("%w: %s answered through a replacement pointing at %s", ErrVersionMismatch, modulePath, resolved.Replace.Path)
		}
		if resolved.Version == "" {
			return nil, fmt.Errorf("%w: %s resolved to no version", ErrUnresolvedModule, modulePath)
		}
		if canonical := module.CanonicalVersion(resolved.Version); canonical != resolved.Version {
			return nil, fmt.Errorf("%w: %s resolved to the non canonical %s", ErrVersionMismatch, modulePath, resolved.Version)
		}
	}
	return found, nil
}

// uniqueSorted returns the module paths in sorted order, refusing duplicates.
func uniqueSorted(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("at least one staging module is required")
	}
	ordered := slices.Clone(paths)
	slices.Sort(ordered)
	for i, modulePath := range ordered {
		if modulePath == "" {
			return nil, errors.New("a staging module path is required")
		}
		if i > 0 && ordered[i-1] == modulePath {
			return nil, fmt.Errorf("staging module %s is listed twice", modulePath)
		}
	}
	return ordered, nil
}
