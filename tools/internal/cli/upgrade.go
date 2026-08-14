package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
	"github.com/enj/soapbox/tools/internal/upgrade"
)

// upgradeFlags holds the parsed upgrade flags.
type upgradeFlags struct {
	dir          *string
	path         *string
	targetConfig *string
	engine       *string
	engineMod    *string
	sum          *string
	format       *string
	report       *string
	apply        *bool
	approve      *string
}

func upgradeFlagSet() (*flag.FlagSet, *upgradeFlags) {
	fs := newFlagSet("upgrade")
	return fs, &upgradeFlags{
		dir:          fs.String("dir", ".", "derived repository to upgrade"),
		path:         fs.String("config", config.DefaultFileName, "profile path relative to -dir"),
		targetConfig: fs.String("target-config", "", "validated target profile to install; identity fields must match the derived repository"),
		engine:       fs.String("engine-version", "", "target engine release, spelled v1.2.3 or "+setup.EngineTagPrefix+"v1.2.3"),
		engineMod:    fs.String("engine-mod", "", "path to the target engine release's go.mod (required)"),
		sum:          fs.String("engine-sum", "", "file holding the complete verified go.sum content for the nested tools module (required)"),
		format:       fs.String("format", runFormats[0], "output format: "+strings.Join(runFormats, ", ")),
		report:       fs.String("report", "", "write the manifest to this path, relative to -dir when not absolute"),
		apply:        fs.Bool("apply", false, "write the manifest instead of reporting it, which requires -approve"),
		approve:      fs.String("approve", "", "manifest hash a dry run produced, which -apply must match exactly"),
	}
}

// runUpgrade upgrades the setup-owned files in a derived repository.
//
// The profile is loaded with migration support: a schema v1 profile with a
// githubApp section is migrated to v2 automatically. When the profile was
// migrated and the apply succeeds, the migrated profile bytes are written back
// to soapbox.yaml so the repository carries a valid v2 profile.
//
// The engine go.mod is read from the -engine-mod flag, which must point to the
// engine's own go.mod at the target release — not the derived repository's
// shim go.mod that declares a different module path.
func runUpgrade(ctx context.Context, env Env, args []string) error {
	fs, flags := upgradeFlagSet()
	if err := parseFlags(env, upgradeCommand(), fs, args); err != nil {
		return err
	}
	usage := commandUsage(upgradeCommand(), fs)

	if !slices.Contains(runFormats, *flags.format) {
		return &usageError{
			err:   fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(runFormats, ", ")),
			usage: usage,
		}
	}
	if *flags.apply && *flags.approve == "" {
		return &usageError{err: errors.New("upgrade -apply requires -approve"), usage: usage}
	}
	if !*flags.apply && *flags.approve != "" {
		return &usageError{err: errors.New("-approve is meaningless without -apply"), usage: usage}
	}
	if *flags.engine == "" {
		return &usageError{err: errors.New("upgrade requires -engine-version"), usage: usage}
	}
	if *flags.engineMod == "" {
		return &usageError{err: errors.New("upgrade requires -engine-mod pointing to the target engine's go.mod"), usage: usage}
	}
	if *flags.sum == "" {
		return &usageError{err: errors.New("upgrade requires -engine-sum pointing to the verified go.sum for the tools module"), usage: usage}
	}

	// Resolve paths.
	root, err := filepath.Abs(env.resolve(*flags.dir))
	if err != nil {
		return fmt.Errorf("upgrade: resolve %s: %w", *flags.dir, err)
	}

	// Resolve the profile path as a clean repo-relative path. Refuse
	// absolute paths that resolve outside root, and read through os.Root
	// so symlinks cannot escape.
	repoRelProfile := *flags.path
	if filepath.IsAbs(repoRelProfile) {
		rel, relErr := filepath.Rel(root, repoRelProfile)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("upgrade: -config %q resolves outside the repository", repoRelProfile)
		}
		repoRelProfile = rel
	}
	repoRelProfile = filepath.ToSlash(filepath.Clean(repoRelProfile))
	if err := config.ValidateRelPath(repoRelProfile); err != nil {
		return fmt.Errorf("upgrade: -config: %w", err)
	}

	// Read the profile through os.Root so symlinks/traversal are contained.
	repoRoot, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("upgrade: open repository: %w", err)
	}
	profileInfo, err := repoRoot.Lstat(filepath.FromSlash(repoRelProfile))
	if err != nil {
		_ = repoRoot.Close()
		return fmt.Errorf("upgrade: stat profile: %w", err)
	}
	if !profileInfo.Mode().IsRegular() {
		_ = repoRoot.Close()
		return fmt.Errorf("upgrade: -config %q is not a regular file", repoRelProfile)
	}
	profileData, err := repoRoot.ReadFile(filepath.FromSlash(repoRelProfile))
	_ = repoRoot.Close()
	if err != nil {
		return fmt.Errorf("upgrade: read profile: %w", err)
	}

	currentConfig, migratedBytes, err := config.DecodeWithMigration(profileData)
	if err != nil {
		return profileError(env, repoRelProfile, err)
	}
	cfg := currentConfig
	if *flags.targetConfig != "" {
		targetPath := env.resolve(*flags.targetConfig)
		targetData, readErr := readUpgradeTargetProfile(ctx, targetPath)
		if readErr != nil {
			return readErr
		}
		targetConfig, decodeErr := config.Decode(targetData)
		if decodeErr != nil {
			return profileError(env, targetPath, decodeErr)
		}
		if identityErr := validateUpgradeProfileIdentity(currentConfig, targetConfig); identityErr != nil {
			return upgradeError(identityErr)
		}
		cfg = targetConfig
		migratedBytes = targetData
	}
	profileMigrated := !bytes.Equal(migratedBytes, profileData)

	// Read the engine's go.mod from the explicit flag.
	engineModPath := env.resolve(*flags.engineMod)
	engineMod, err := os.ReadFile(engineModPath)
	if err != nil {
		return fmt.Errorf("upgrade: read engine go.mod: %w", err)
	}

	// Read engine checksums.
	sumPath := env.resolve(*flags.sum)
	engineSum, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("upgrade: read engine checksums: %w", err)
	}

	// Build git runner.
	git, err := gitcli.New(ctx, gitcli.Options{Dir: root})
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}

	opts := upgrade.Options{
		Root:          root,
		Config:        cfg,
		EngineVersion: *flags.engine,
		EngineMod:     engineMod,
		EngineSum:     engineSum,
		ProfilePath:   repoRelProfile,
		Git:           git,
	}
	if profileMigrated {
		opts.MigratedProfile = migratedBytes
	}

	var result *upgrade.Result
	if *flags.apply {
		result, err = upgrade.Apply(ctx, opts, *flags.approve)
	} else {
		result, err = upgrade.Plan(ctx, opts)
	}

	// Write the result before returning an error, because a stale approval still
	// produced the current manifest. Refused runs keep stdout quiet but write a
	// requested report, matching setup, plan, generate, and sync.
	var writeErr error
	if result != nil {
		reportPath := *flags.report
		if reportPath != "" && !filepath.IsAbs(reportPath) {
			reportPath = filepath.Join(root, reportPath)
		}
		outputEnv := env
		if err != nil {
			outputEnv.Stdout = io.Discard
		}
		writeErr = writeReportOutput(ctx, outputEnv, "upgrade", reportPath, *flags.format,
			result.Report.JSON, result.Summary)
	}
	return upgradeError(errors.Join(err, writeErr))
}

// readUpgradeTargetProfile reads an explicitly selected target profile without
// following a symlink. The target may live outside the derived repository, but
// it must be a regular file whose exact bytes can be bound into the manifest.
func readUpgradeTargetProfile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("upgrade: read -target-config: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("upgrade: stat -target-config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upgrade: -target-config %q is not a regular file", path)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("upgrade: read -target-config: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("upgrade: read -target-config: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("upgrade: read -target-config: %w", err)
	}
	return data, nil
}

// validateUpgradeProfileIdentity prevents an engine/profile upgrade from
// silently retargeting an existing derived repository. Policy, compatibility,
// facade, dependency, and committer changes are allowed and remain bound by the
// manifest; source, destination, release, provenance-key, and vanity identities
// are not an upgrade surface.
func validateUpgradeProfileIdentity(current, target *config.Config) error {
	if current == nil || target == nil {
		return &upgrade.PolicyError{Err: errors.New("upgrade: current and target profiles are required")}
	}
	fields := []struct {
		name    string
		current string
		target  string
	}{
		{name: "source.repository", current: current.Source.Repository, target: target.Source.Repository},
		{name: "source.importPrefix", current: current.Source.ImportPrefix, target: target.Source.ImportPrefix},
		{name: "source.refs.minimumRelease", current: current.Source.Refs.MinimumRelease, target: target.Source.Refs.MinimumRelease},
		{name: "source.refs.anchorCommit", current: current.Source.Refs.AnchorCommit, target: target.Source.Refs.AnchorCommit},
		{name: "destination.module", current: current.Destination.Module, target: target.Destination.Module},
		{name: "destination.repository", current: current.Destination.Repository, target: target.Destination.Repository},
		{name: "destination.remote", current: current.Destination.Remote, target: target.Destination.Remote},
		{name: "destination.branch", current: current.Destination.Branch, target: target.Destination.Branch},
		{name: "destination.stateRef", current: current.Destination.StateRef, target: target.Destination.StateRef},
		{name: "destination.progressRefPrefix", current: current.Destination.ProgressRefPrefix, target: target.Destination.ProgressRefPrefix},
		{name: "destination.rootPackage", current: current.Destination.RootPackage, target: target.Destination.RootPackage},
		{name: "destination.internalPrefix", current: current.Destination.InternalPrefix, target: target.Destination.InternalPrefix},
		{name: "release.policy", current: current.Release.Policy, target: target.Release.Policy},
		{name: "release.firstTag", current: current.Release.FirstTag, target: target.Release.FirstTag},
		{name: "commit.authorPolicy", current: current.Commit.AuthorPolicy, target: target.Commit.AuthorPolicy},
		{name: "commit.trailerKey", current: current.Commit.TrailerKey, target: target.Commit.TrailerKey},
		{name: "commit.sign", current: fmt.Sprintf("%t", current.Commit.Sign), target: fmt.Sprintf("%t", target.Commit.Sign)},
		{name: "vanity.repository", current: current.Vanity.Repository, target: target.Vanity.Repository},
		{name: "vanity.path", current: current.Vanity.Path, target: target.Vanity.Path},
		{name: "vanity.importPath", current: current.Vanity.ImportPath, target: target.Vanity.ImportPath},
		{name: "vanity.repositoryURL", current: current.Vanity.RepositoryURL, target: target.Vanity.RepositoryURL},
		{name: "vanity.probeURL", current: current.Vanity.ProbeURL, target: target.Vanity.ProbeURL},
	}
	for _, field := range fields {
		if field.current == field.target {
			continue
		}
		return &upgrade.PolicyError{Err: fmt.Errorf(
			"upgrade: -target-config changes immutable profile field %s from %q to %q",
			field.name, field.current, field.target,
		)}
	}
	return nil
}

// upgradeError maps an upgrade policy error to a check error for exit codes.
func upgradeError(err error) error {
	var pe *upgrade.PolicyError
	if errors.As(err, &pe) {
		return &checkError{summary: err.Error(), err: err}
	}
	return err
}
