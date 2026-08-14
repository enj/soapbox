// Package gitcli is the only Git subprocess and credential boundary in the
// engine.
//
// Every command is built as an explicit argument vector and executed with
// exec.CommandContext. No command is ever composed through sh, bash, or -c, so
// there is no string that a ref name, path, or upstream commit message could
// escape into. Credentials never appear in an argument vector or a remote URL.
// They are supplied through the controlled process environment and seeded into
// a redactor before the first subprocess starts.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultBinary is the Git executable looked up on PATH when none is given.
const DefaultBinary = "git"

// PublishHost is the only host the engine may push to over the network.
const PublishHost = "github.com"

// redactedUser replaces user information in a rendered remote URL.
const redactedUser = "redacted"

// The oldest Git release every command in this package is proved against.
//
// The floor is set by GIT_NO_LAZY_FETCH, which the object probes rely on to
// answer "do I already have this object" without letting a partial clone
// silently download the history it is being asked about. The variable is only a
// documented, generally honoured knob from 2.45; it was backported into the
// 2024-04-10 maintenance releases (2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4,
// 2.44.1), but on any older release it is read by nothing and the probe would
// quietly reach the network instead of failing. A feature whose absence is
// silent has to be pinned to the release that documents it.
//
// The other version sensitive commands this package issues are all older:
// sparse-checkout set --no-cone is 2.35 and is load bearing because cone mode
// became the default in 2.37, clone and fetch --filter are 2.17,
// fetch --no-write-fetch-head is 2.29, the worktree config extension and
// config --worktree are 2.20, worktree add --no-checkout is 2.9,
// worktree list --porcelain is 2.7, merge-base --octopus --all is 1.7.3,
// rev-list --missing and --filter-print-omitted are 2.16, and the
// compare-and-swap form of update-ref predates all of them.
//
// TestMinimumVersionCapabilities exercises each of these against the Git the
// tests actually run, so the claim is checked rather than asserted.
const (
	minimumVersionMajor = 2
	minimumVersionMinor = 45
)

// MinimumVersion reports the oldest Git release this package supports. It is
// exported so the preflight check and the engine's doctor report a single
// number rather than two that can drift apart.
//
// The floor is set by GIT_NO_LAZY_FETCH, so it is that capability's version
// rather than a number of its own.
func MinimumVersion() Version {
	return MinimumNoLazyFetchVersion()
}

// MinimumNoLazyFetchVersion is the oldest Git release that documents and
// honours GIT_NO_LAZY_FETCH.
//
// It is exported because more than one caller has to enforce it and they must
// not each carry a number of their own: offline materialisation refuses to run
// below it, and the doctor reports it. The provenance is in the package's
// version comment above.
//
// The number matters less than the behaviour, which is why
// TestNoLazyFetchIsHonoured exercises the variable against the git the tests
// actually run. An older git ignores it rather than rejecting it, so a floor
// that is wrong fails silently, by reaching the network, rather than loudly.
func MinimumNoLazyFetchVersion() Version {
	return Version{Major: minimumVersionMajor, Minor: minimumVersionMinor}
}

// defaultInherit lists the process environment variables a Git subprocess may
// see. Everything else is dropped so runs do not depend on ambient state.
var defaultInherit = []string{"PATH", "HOME", "SYSTEMROOT"}

// fixedEnv is applied to every Git subprocess. Global and system configuration
// are neutralised so ambient developer or runner settings can never change what
// the engine produces, terminal prompts are disabled so a missing credential
// fails instead of hanging, and the C locale keeps parsed output stable.
var fixedEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_SYSTEM=" + os.DevNull,
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"LC_ALL=C",
	"LANG=C",
}

// fixedConfig is prepended to every Git command. Repository local hooks are
// disabled because the engine materialises untrusted upstream content and a
// checked out hook must never execute.
var fixedConfig = []string{"-c", "core.hooksPath=" + os.DevNull}

// commandWaitDelay bounds how long a killed subprocess and its descendants may
// keep the output pipes open. Without it a grandchild holding a pipe would
// block cancellation forever.
const commandWaitDelay = 5 * time.Second

// Options configures a Runner.
type Options struct {
	// Binary is the Git executable. Empty means look up DefaultBinary on PATH.
	Binary string
	// Dir is the working directory for every command. Empty means the process
	// working directory.
	Dir string
	// Inherit names the process environment variables to pass through. Empty
	// means the default set. The values are read once, here, so a later change
	// to the process environment cannot alter what an already built runner does.
	Inherit []string
	// Isolation holds KEY=VALUE entries that decide where Git looks for state
	// rather than granting access to anything, such as HOME or TMPDIR. They
	// carry no credential, so they survive Anonymous: a run that redirected HOME
	// to keep a subprocess away from operator configuration must stay redirected
	// when it talks to the source host. They are not seeded into the redactor,
	// because a path is not a secret and redacting it would corrupt output.
	Isolation []string
	// Env holds additional KEY=VALUE entries applied after the inherited, fixed,
	// and isolation entries, which is how credentials reach Git. Every non-empty
	// value is seeded into the redactor, so an entry cannot leak by being
	// forgotten in Secrets. A runner with any entry here is treated as possibly
	// carrying a credential and is refused by the source commands until
	// Anonymous strips them.
	Env []string
	// Secrets holds additional exact values that must never appear in captured
	// output.
	Secrets []string
	// gitConfig is typed per-command configuration assembled by package-owned
	// credential helpers. Keeping it private prevents arbitrary callers from
	// bypassing validation or forgetting redaction.
	gitConfig []gitConfig
	// OutputLimit bounds the bytes one command may return on a stream. Zero
	// means DefaultOutputLimit and a negative value is rejected.
	OutputLimit int64
}

// Runner executes Git commands with a controlled environment.
type Runner struct {
	binary string
	dir    string
	// ceiling stops repository discovery from ascending above dir. It is set by
	// WithDir, where the caller has named the exact repository to operate on, so
	// a command can never silently act on a repository that happens to sit above
	// a directory that lost its own.
	ceiling string
	env     []string
	// inherited is the snapshot of the process entries taken in New. Anonymous
	// rebuilds from it rather than from the process, so stripping credentials
	// cannot also pick up an environment that changed in between.
	inherited []string
	// isolation is retained for the same reason: it has to survive Anonymous.
	isolation []string
	anonymous bool
	// noLazyFetch pins GIT_NO_LAZY_FETCH onto every command. It is a field
	// rather than an environment entry so that it survives Anonymous and cannot
	// be shadowed by a caller entry.
	noLazyFetch bool
	// outputLimit bounds what one command may return on a stream.
	outputLimit int64
	redactor    *Redactor
}

// New resolves the Git executable and builds the subprocess environment.
func New(ctx context.Context, opts Options) (*Runner, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("git runner: %w", err)
	}
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if !strings.ContainsRune(binary, filepath.Separator) && !strings.ContainsRune(binary, '/') {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			return nil, fmt.Errorf("git executable lookup: %w", err)
		}
		binary = resolved
	}
	if opts.Dir != "" {
		if err := validateDirectory("git working directory", opts.Dir); err != nil {
			return nil, err
		}
	}
	for i, entry := range opts.Isolation {
		if err := validateEnvEntry("isolation entry", i, entry); err != nil {
			return nil, fmt.Errorf("git runner: %w", err)
		}
	}
	for i, entry := range opts.Env {
		if err := validateEnvEntry("environment entry", i, entry); err != nil {
			return nil, fmt.Errorf("git runner: %w", err)
		}
	}
	if err := validateGitConfig(opts.gitConfig); err != nil {
		return nil, fmt.Errorf("git runner: %w", err)
	}
	limit := opts.OutputLimit
	switch {
	case limit < 0:
		return nil, fmt.Errorf("git runner: output limit %d must not be negative", limit)
	case limit == 0:
		limit = DefaultOutputLimit
	}
	inherited := inheritedEnv(opts.Inherit)
	isolation := slices.Clone(opts.Isolation)
	runtimeEnv := append(slices.Clone(opts.Env), gitConfigEnv(opts.gitConfig)...)
	return &Runner{
		binary:      binary,
		dir:         opts.Dir,
		env:         assembleEnv(inherited, isolation, runtimeEnv),
		inherited:   inherited,
		isolation:   isolation,
		anonymous:   len(opts.Env) == 0 && len(opts.gitConfig) == 0,
		outputLimit: limit,
		redactor:    NewRedactor(append(envValues(opts.Env), opts.Secrets...)...),
	}, nil
}

// validateDirectory checks that a working directory exists and is a directory.
func validateDirectory(kind, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: %q is not a directory", kind, dir)
	}
	return nil
}

// validateEnvEntry rejects an entry that is not a KEY=VALUE pair, which would
// otherwise reach the subprocess as an unnamed value or silently shadow nothing.
//
// An entry with no separator is reported by its position rather than quoted.
// The whole entry is the value in that case, an Env entry is where credentials
// arrive, and this runs before the redactor that would have masked it exists, so
// quoting it is the one way a token could reach a log through this package. Once
// a name is known it is safe to name: the name is chosen by the caller and the
// value is never echoed.
func validateEnvEntry(kind string, index int, entry string) error {
	name, _, ok := strings.Cut(entry, "=")
	switch {
	case !ok:
		return fmt.Errorf("%s %d must be KEY=VALUE", kind, index)
	case name == "":
		return fmt.Errorf("%s %d must name a variable", kind, index)
	case strings.ContainsRune(entry, '\x00'):
		return fmt.Errorf("%s %q must not contain a null byte", kind, name)
	}
	return nil
}

// envValues reports the value half of every KEY=VALUE entry. Redaction is
// seeded from them so a credential passed through Env is covered even when the
// caller forgets to list it in Secrets.
func envValues(entries []string) []string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, value, ok := strings.Cut(entry, "="); ok && value != "" {
			values = append(values, value)
		}
	}
	return values
}

// Binary reports the resolved Git executable path.
func (r *Runner) Binary() string { return r.binary }

// Dir reports the working directory used for every command.
func (r *Runner) Dir() string { return r.dir }

// Redactor reports the redactor seeded with this runner's secrets.
func (r *Runner) Redactor() *Redactor { return r.redactor }

// WithDir returns a copy of the runner that operates in dir.
//
// The directory is checked exactly as New checks one, because a runner pointed
// at a path that was never created would otherwise run its first command in the
// process working directory. The path must be absolute for the same reason.
//
// Discovery is also pinned: a command run through the returned runner may find
// the repository at dir but may never ascend to one above it. Without that, a
// cache directory that was removed or never finished being written would hand
// every later command whatever repository happens to contain it.
func (r *Runner) WithDir(dir string) (*Runner, error) {
	if err := validateAbsolutePath("git working directory", dir); err != nil {
		return nil, err
	}
	if err := validateDirectory("git working directory", dir); err != nil {
		return nil, err
	}
	ceiling, err := discoveryCeiling(dir)
	if err != nil {
		return nil, err
	}
	clone := *r
	clone.dir = dir
	clone.ceiling = ceiling
	return &clone, nil
}

// discoveryCeiling reports the directory Git must not ascend past to keep
// repository discovery inside dir.
//
// Git compares the ceiling against the physical path it walks, so a ceiling
// that still contains a symbolic link is ignored, and it stops before entering
// a listed directory rather than at it: confining discovery to dir means naming
// dir's parent.
func discoveryCeiling(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("git working directory: %w", err)
	}
	parent := filepath.Dir(resolved)
	if parent == resolved {
		// A filesystem root has nothing above it to discover.
		return "", nil
	}
	return parent, nil
}

// Anonymous returns a copy of the runner with every caller supplied environment
// entry dropped, which is how a credential that reached this runner through
// Options.Env is kept away from a subprocess that talks to the public source
// host. The redactor is preserved, because output must still be scrubbed even
// when this particular command cannot see the secret.
//
// Isolation entries are kept. They decide where Git looks for state rather than
// granting access to anything, and a run that redirected HOME away from
// operator configuration must stay redirected when it reaches the network. A
// no lazy fetch pin is kept for the same reason: dropping credentials must not
// also drop a refusal to reach the network.
func (r *Runner) Anonymous() *Runner {
	clone := *r
	clone.env = assembleEnv(r.inherited, r.isolation, nil)
	clone.anonymous = true
	return &clone
}

// WithNoLazyFetch returns a copy of the runner that refuses to download objects
// from a promisor remote.
//
// This is the intrinsic form of the guarantee. Passing GIT_NO_LAZY_FETCH to one
// command protects that command; pinning it to a runner protects every command
// the runner will ever issue, including the ones that reach the object store
// without looking like it. A checkout, a reset, or a diff in a blobless clone
// will each happily fetch what they are missing, and none of them takes an
// option that says otherwise.
//
// The pin is a property of the runner rather than an environment entry a caller
// supplies, so there is no general override to reopen: it survives Anonymous,
// it cannot be shadowed by a later Env entry, and it is applied after everything
// else when the command is built. A call that explicitly asks for a lazy fetch
// on a pinned runner is refused rather than quietly ignored.
//
// It requires a Git that honours the variable. See MinimumNoLazyFetchVersion.
func (r *Runner) WithNoLazyFetch() *Runner {
	clone := *r
	clone.noLazyFetch = true
	return &clone
}

// IsNoLazyFetch reports whether the runner refuses promisor fetches.
func (r *Runner) IsNoLazyFetch() bool { return r.noLazyFetch }

// IsAnonymous reports whether the runner carries no caller supplied environment
// entries. Source acquisition asserts this before it reaches the network.
func (r *Runner) IsAnonymous() bool { return r.anonymous }

// inheritedEnv snapshots the named process environment variables as KEY=VALUE
// entries, in the order they were named.
func inheritedEnv(inherit []string) []string {
	names := inherit
	if len(names) == 0 {
		names = defaultInherit
	}
	env := make([]string, 0, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// assembleEnv joins the inherited snapshot, the fixed entries, the isolation
// entries, and the caller supplied entries. Later entries win, which is how a
// caller overrides HOME for an isolated run and how per commit identity is
// supplied without touching an argument vector.
func assembleEnv(inherited, isolation, extra []string) []string {
	env := make([]string, 0, len(inherited)+len(fixedEnv)+len(isolation)+len(extra))
	env = append(env, inherited...)
	env = append(env, fixedEnv...)
	env = append(env, isolation...)
	return append(env, extra...)
}

// ExecError reports a Git command that exited non-zero or could not run. Every
// field is redacted.
type ExecError struct {
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

// Error renders the failing command, its exit status, and the first line of its
// diagnostic output.
func (e *ExecError) Error() string {
	message := fmt.Sprintf("git %s", strings.Join(e.Args, " "))
	if e.ExitCode > 0 {
		message += ": exit status " + strconv.Itoa(e.ExitCode)
	} else if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	if line := firstLine(e.Stderr); line != "" {
		message += ": " + line
	}
	return message
}

// Unwrap exposes the underlying execution error.
func (e *ExecError) Unwrap() error { return e.Err }

// ExitCodeOf reports the process exit code carried by err, or zero.
func ExitCodeOf(err error) int {
	var execErr *ExecError
	if errors.As(err, &execErr) {
		return execErr.ExitCode
	}
	return 0
}

// run executes one Git command and returns its redacted standard output.
func (r *Runner) run(ctx context.Context, args ...string) (string, error) {
	return r.runWith(ctx, nil, args...)
}

// runWith executes one Git command with additional environment entries. The
// entries are appended last so they win over the runner environment, which is
// how per commit identity is supplied without touching an argument vector.
func (r *Runner) runWith(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	return r.runInput(ctx, nil, extraEnv, args...)
}

// runInput executes one Git command, optionally feeding it standard input, and
// returns only its standard output.
func (r *Runner) runInput(ctx context.Context, stdin []byte, extraEnv []string, args ...string) (string, error) {
	out, _, err := r.runCapture(ctx, stdin, extraEnv, args...)
	return out, err
}

// runRaw executes one Git command and returns its standard output exactly as
// git produced it.
//
// It exists for the commands that read upstream content rather than
// diagnostics. A commit message, an author identity, and a trailer are replayed
// into published history byte for byte, so passing them through the redactor
// would rewrite the history the engine exists to reproduce, and it would do it
// silently: the run would succeed and the published commit would simply not be
// the upstream one. The same reasoning keeps ReadBlob's bytes untouched.
//
// Standard error and every error this returns are still redacted, because those
// are diagnostics rather than content.
//
// The output limit still applies. Not redacting is a decision about what the
// bytes say, not about how many of them this process will hold: a blob and a tag
// message are upstream input, so reading them unbounded would let the repository
// being replayed decide how much memory the engine allocates. A stream that
// reaches the limit is reported rather than returned short, for the reason
// runCapture gives.
func (r *Runner) runRaw(ctx context.Context, stdin []byte, args ...string) (string, error) {
	stdout := &boundedBuffer{limit: r.outputLimit}
	_, err := r.runOutput(ctx, stdin, nil, stdout, args...)
	if err == nil {
		if err = stdout.overflow("standard output"); err != nil {
			err = fmt.Errorf("git %s: %w", strings.Join(r.redactor.Strings(args), " "), err)
		}
	}
	return stdout.String(), err
}

// runCapture executes one Git command and returns both redacted streams.
//
// Standard error is returned on success as well as on failure, because some of
// git's most consequential verdicts are warnings: a server that ignored the
// object filter and sent a complete history says so there and exits zero.
func (r *Runner) runCapture(ctx context.Context, stdin []byte, extraEnv []string, args ...string) (string, string, error) {
	stdout := &boundedBuffer{limit: r.outputLimit}
	outWriter := r.redactor.Writer(stdout)
	stderr, runErr := r.runOutput(ctx, stdin, extraEnv, outWriter, args...)
	closeErr := outWriter.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("git output capture: %w", closeErr)
	}
	if runErr == nil {
		// A truncated stream is reported rather than parsed, because a parser
		// handed the first half of a response would report a shape error and
		// hide what actually happened.
		runErr = stdout.overflow("standard output")
		if runErr != nil {
			runErr = fmt.Errorf("git %s: %w", strings.Join(r.redactor.Strings(args), " "), runErr)
		}
	}
	return stdout.String(), stderr, errors.Join(runErr, closeErr)
}

// DefaultOutputLimit bounds one stream of one command. An upstream repository
// decides how much a git command prints, so an unbounded capture would let it
// decide how much memory the engine spends. It is generous enough for a full
// history walk over a repository the size of Kubernetes and small enough that a
// runaway command fails instead of exhausting memory.
const DefaultOutputLimit = 512 << 20

// boundedBuffer accumulates output up to a limit and remembers that it stopped.
//
// It keeps accepting writes after the limit so the pipe drains and the command
// exits on its own; what it stops doing is retaining them. gocli carries its own
// copy for the same reason it carries its own ExecError: the two boundaries are
// deliberately parallel rather than sharing a private type across a package
// edge that only goes one way.
type boundedBuffer struct {
	limit   int64
	written int64
	buffer  bytes.Buffer
}

// Write retains what fits and counts the rest.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.written
	b.written += int64(len(p))
	if remaining >= int64(len(p)) {
		return b.buffer.Write(p)
	}
	if remaining > 0 {
		if _, err := b.buffer.Write(p[:remaining]); err != nil {
			return 0, fmt.Errorf("buffer git output: %w", err)
		}
	}
	return len(p), nil
}

// String reports what was retained.
func (b *boundedBuffer) String() string { return b.buffer.String() }

// overflow reports the limit being passed, naming the stream.
func (b *boundedBuffer) overflow(stream string) error {
	if b.written <= b.limit {
		return nil
	}
	return fmt.Errorf("%s reached %d bytes, past the %d byte limit", stream, b.written, b.limit)
}

// runOutput executes one Git command, writing its standard output to out, and
// returns the redacted standard error.
//
// Standard output reaches out exactly as git produced it. Redaction is the
// caller's decision because the two kinds of output need opposite handling:
// runCapture wraps out in a redacting writer because it is reading diagnostics,
// while ReadBlob does not, because rewriting bytes that merely happen to match a
// secret would corrupt the file the engine is about to parse.
//
// The input is an in memory slice rather than an inherited descriptor, so a
// subprocess can never read from the operator's terminal and a command that
// exits early cannot leave the engine blocked on a pipe.
func (r *Runner) runOutput(ctx context.Context, stdin []byte, extraEnv []string, out io.Writer, args ...string) (string, error) {
	stderr := &boundedBuffer{limit: r.outputLimit}
	errWriter := r.redactor.Writer(stderr)

	full := append(append([]string{}, fixedConfig...), args...)
	cmd := exec.CommandContext(ctx, r.binary, full...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	if r.ceiling != "" || r.noLazyFetch || len(extraEnv) > 0 {
		env := slices.Clone(r.env)
		if r.ceiling != "" {
			env = append(env, "GIT_CEILING_DIRECTORIES="+r.ceiling)
		}
		env = append(env, extraEnv...)
		// The pin is applied last so no per command entry can shadow it.
		if r.noLazyFetch {
			env = append(env, noLazyFetch)
		}
		cmd.Env = env
	}
	cmd.Stdout = out
	cmd.Stderr = errWriter
	cmd.Stdin = nil
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.WaitDelay = commandWaitDelay

	runErr := cmd.Run()
	closeErr := errWriter.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("git output capture: %w", closeErr)
	}
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stderr.String(), errors.Join(fmt.Errorf("git %s: %w", strings.Join(r.redactor.Strings(args), " "), ctxErr), closeErr)
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return stderr.String(), errors.Join(&ExecError{
			Args:     r.redactor.Strings(args),
			ExitCode: exitCode,
			Stderr:   stderr.String(),
			Err:      runErr,
		}, closeErr)
	}
	return stderr.String(), closeErr
}

// Version is a parsed Git or Go release version.
type Version struct {
	Major int
	Minor int
	Patch int
}

// String renders the version as major.minor.patch.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AtLeast reports whether v is not older than other.
func (v Version) AtLeast(other Version) bool {
	switch {
	case v.Major != other.Major:
		return v.Major > other.Major
	case v.Minor != other.Minor:
		return v.Minor > other.Minor
	default:
		return v.Patch >= other.Patch
	}
}

// ParseVersion parses a dotted numeric version, ignoring any vendor suffix.
func ParseVersion(value string) (Version, error) {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == '.' })
	if len(fields) == 0 {
		return Version{}, fmt.Errorf("version %q has no numeric components", value)
	}
	var version Version
	targets := []*int{&version.Major, &version.Minor, &version.Patch}
	for i, target := range targets {
		if i >= len(fields) {
			break
		}
		digits := leadingDigits(fields[i])
		if digits == "" {
			if i == 0 {
				return Version{}, fmt.Errorf("version %q has no numeric components", value)
			}
			break
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", value, err)
		}
		*target = n
	}
	return version, nil
}

// Version reports the version of the resolved Git executable.
func (r *Runner) Version(ctx context.Context) (Version, error) {
	out, err := r.run(ctx, "--version")
	if err != nil {
		return Version{}, fmt.Errorf("git version: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return Version{}, fmt.Errorf("git version: unexpected output %q", firstLine(out))
	}
	version, err := ParseVersion(fields[2])
	if err != nil {
		return Version{}, fmt.Errorf("git version: %w", err)
	}
	return version, nil
}

// RequireMinimumVersion fails when the resolved Git is older than the release
// every command in this package is proved against.
//
// It is a preflight rather than a nicety. The oldest capability this package
// depends on, GIT_NO_LAZY_FETCH, is ignored rather than rejected by a Git that
// does not know it, so an old binary would turn a local object probe into a
// silent network fetch instead of an error.
func (r *Runner) RequireMinimumVersion(ctx context.Context) error {
	version, err := r.Version(ctx)
	if err != nil {
		return err
	}
	if !version.AtLeast(MinimumVersion()) {
		return fmt.Errorf("git %s is older than the required %s", version, MinimumVersion())
	}
	return nil
}

// InitRepository creates a repository in the runner's working directory.
func (r *Runner) InitRepository(ctx context.Context, initialBranch string) error {
	if err := ValidateBranchName(initialBranch); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	if _, err := r.run(ctx, "init", "--quiet", "--initial-branch="+initialBranch); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	return nil
}

// notARepository is the exact prefix git uses when discovery simply found no
// repository. Every other fatal error, such as dubious ownership or a broken
// gitdir pointer, also exits 128 and must be reported rather than swallowed.
const notARepository = "not a git repository (or any"

// IsRepository reports whether the working directory is inside a Git work tree.
// A directory that is not a repository yet is a supported state; a repository
// that cannot be read is an error.
func (r *Runner) IsRepository(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		var execErr *ExecError
		if errors.As(err, &execErr) && strings.Contains(execErr.Stderr, notARepository) {
			return false, nil
		}
		return false, fmt.Errorf("git work tree probe: %w", err)
	}
	return strings.TrimSpace(out) == "true", nil
}

// RepositoryRoot reports the absolute path of the work tree root.
func (r *Runner) RepositoryRoot(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git repository root: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// HasHead reports whether HEAD resolves to an existing object. A freshly
// initialized repository has no HEAD commit and is not an error, while a HEAD
// that points at a missing or unreadable object is.
//
// show-ref is used rather than rev-parse because rev-parse --quiet reports both
// states with exit status 1, which would hide a corrupt repository.
func (r *Runner) HasHead(ctx context.Context) (bool, error) {
	_, err := r.run(ctx, "show-ref", "--head", "--verify", "--quiet", "--", "HEAD")
	if err == nil {
		return true, nil
	}
	if ExitCodeOf(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git head probe: %w", err)
}

// ResolveCommit resolves a revision to a full commit object name.
func (r *Runner) ResolveCommit(ctx context.Context, revision string) (string, error) {
	if err := validateArgument("revision", revision); err != nil {
		return "", fmt.Errorf("git revision: %w", err)
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("git revision %q: %w", r.redactor.String(revision), err)
	}
	return strings.TrimSpace(out), nil
}

// ConfigLocal reads one repository local configuration value. A missing key is
// reported through found rather than as an error.
func (r *Runner) ConfigLocal(ctx context.Context, key string) (value string, found bool, err error) {
	if err := validateArgument("config key", key); err != nil {
		return "", false, fmt.Errorf("git config: %w", err)
	}
	out, err := r.run(ctx, "config", "--local", "--get", "--end-of-options", key)
	if err != nil {
		if ExitCodeOf(err) == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return strings.TrimRight(out, "\n"), true, nil
}

// SetConfigLocal writes one repository local configuration value. Values that
// look like options are rejected because git config has no option terminator.
func (r *Runner) SetConfigLocal(ctx context.Context, key, value string) error {
	if err := validateArgument("config key", key); err != nil {
		return fmt.Errorf("git config: %w", err)
	}
	if err := validateArgument("config value", value); err != nil {
		return fmt.Errorf("git config %s: %w", r.redactor.String(key), r.redactor.Error(err))
	}
	if _, err := r.run(ctx, "config", "--local", "--replace-all", key, value); err != nil {
		return fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return nil
}

// UnsetConfigLocal removes one repository local configuration key. Removing a
// key that is already absent is not an error.
func (r *Runner) UnsetConfigLocal(ctx context.Context, key string) error {
	if err := validateArgument("config key", key); err != nil {
		return fmt.Errorf("git config: %w", err)
	}
	_, err := r.run(ctx, "config", "--local", "--unset-all", "--end-of-options", key)
	if err != nil && ExitCodeOf(err) != 5 {
		return fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return nil
}

// Trailer is one parsed commit message trailer.
type Trailer struct {
	Key   string
	Value string
}

// Commit is the metadata the engine needs about one commit.
type Commit struct {
	SHA string
	// Parents are the parent object names in git's order, so Parents[0] is the
	// first parent and defines the mainline. A root commit has none.
	Parents     []string
	AuthorName  string
	AuthorEmail string
	// AuthorDate is Git's strict ISO 8601 rendering for display and comparison.
	AuthorDate string
	// AuthorDateRaw is Git's original "<seconds> <offset>" form for replay.
	AuthorDateRaw  string
	CommitterName  string
	CommitterEmail string
	// CommitterDate is Git's strict ISO 8601 rendering.
	CommitterDate string
	// CommitterDateRaw is Git's original raw form for deterministic replay.
	CommitterDateRaw string
	SignatureStatus  string
	SignerKey        string
	Signer           string
	Subject          string
	// RawMessage is the complete commit message including the subject line, so
	// replaying it must not append the subject again.
	RawMessage string
	Trailers   []Trailer
}

// Identity renders a name and email in Git identity form.
func Identity(name, email string) string {
	return fmt.Sprintf("%s <%s>", name, email)
}

// AuthorIdentity renders the commit author identity.
func (c Commit) AuthorIdentity() string { return Identity(c.AuthorName, c.AuthorEmail) }

// CommitterIdentity renders the commit committer identity.
func (c Commit) CommitterIdentity() string { return Identity(c.CommitterName, c.CommitterEmail) }

// TrailerValues reports every value recorded under key.
func (c Commit) TrailerValues(key string) []string {
	var values []string
	for _, trailer := range c.Trailers {
		if strings.EqualFold(trailer.Key, key) {
			values = append(values, trailer.Value)
		}
	}
	return values
}

// One commit is rendered as a fixed number of null separated fields. Null is
// the separator because every other candidate can appear inside a commit
// message, and the count is fixed so a record boundary is arithmetic rather
// than a guess about where a message ended.
//
// The three signature fields are a hole rather than a placeholder in the batch
// form. %G? makes git verify the signature, which runs a verifier per signed
// commit and answers from whatever keys the machine happens to trust, so a walk
// over thousands of commits would pay for a verdict it did not ask for and would
// return a different answer on a different machine. Leaving the slots empty
// keeps the record shape, and therefore the parser and the field count,
// identical either way.
const (
	commitFieldsIdentity  = "%H%x00%P%x00%an%x00%ae%x00%aI%x00%ad%x00%cn%x00%ce%x00%cI%x00%cd"
	commitFieldsSigned    = "%x00%G?%x00%GK%x00%GS"
	commitFieldsUnsigned  = "%x00%x00%x00"
	commitFieldsMessage   = "%x00%s%x00%B%x00%(trailers:only=true,unfold=true)"
	commitFormat          = commitFieldsIdentity + commitFieldsSigned + commitFieldsMessage
	commitFormatUnsigned  = commitFieldsIdentity + commitFieldsUnsigned + commitFieldsMessage
	commitFieldCount      = 16
	commitObjectNameField = 0
	commitSubjectField    = 13
)

// CommitInfo reads metadata, signature status, and trailers for one revision.
func (r *Runner) CommitInfo(ctx context.Context, revision string) (Commit, error) {
	if err := validateArgument("revision", revision); err != nil {
		return Commit{}, fmt.Errorf("git commit metadata: %w", err)
	}
	// The message, identities, and trailers are upstream content that is
	// replayed verbatim, so this read bypasses the redactor.
	out, err := r.runRaw(ctx, nil, "log", "-z", "-1", "--no-patch", "--date=raw", "--format="+commitFormat, "--end-of-options", revision, "--")
	if err != nil {
		return Commit{}, fmt.Errorf("git commit metadata for %q: %w", r.redactor.String(revision), err)
	}
	commits, err := parseCommitRecords(out)
	if err != nil {
		return Commit{}, fmt.Errorf("git commit metadata for %q: %w", r.redactor.String(revision), err)
	}
	if len(commits) != 1 {
		return Commit{}, fmt.Errorf("git commit metadata for %q: got %d records, want 1", r.redactor.String(revision), len(commits))
	}
	return commits[0], nil
}

// CommitLogOptions selects the commits one batched metadata read covers.
type CommitLogOptions struct {
	// Include lists the revisions to walk from, such as branch tips.
	Include []string
	// Exclude lists the revisions whose ancestors are left out, which is how a
	// walk is bounded below by an already mapped commit.
	Exclude []string
	// FirstParent follows only the first parent of every merge, which yields the
	// mainline of a branch.
	FirstParent bool
	// MaxCount bounds the number of commits returned. Zero means no bound.
	MaxCount int
	// Signatures fills SignatureStatus, SignerKey, and Signer. They are empty
	// without it, because verifying every commit in a walk is neither free nor
	// reproducible across machines. Ask for one commit's signature with
	// CommitInfo instead unless the whole walk genuinely needs it.
	Signatures bool
}

// CommitLog reads metadata, messages, and trailers for a range of commits in one
// subprocess.
//
// This is the batched form of CommitInfo, and batching is the point: mapping a
// source history onto a published one reads tens of thousands of commits, and a
// process per commit would dominate every other cost in the engine.
//
// The order is git's own reverse topological order, parents before children,
// which matches CommitGraph so a caller can zip the two together. It is not a
// date order, because commit dates in an imported history are rebase and
// attacker controlled.
//
// MaxCount is applied before the reversal, exactly as git applies it: a bounded
// walk returns the newest commits presented oldest first, not the oldest ones.
func (r *Runner) CommitLog(ctx context.Context, opts CommitLogOptions) ([]Commit, error) {
	if len(opts.Include) == 0 {
		return nil, errors.New("git log: at least one revision is required")
	}
	for _, revision := range opts.Include {
		if err := validateRevision(revision); err != nil {
			return nil, fmt.Errorf("git log: %w", err)
		}
	}
	for _, revision := range opts.Exclude {
		if err := validateRevision(revision); err != nil {
			return nil, fmt.Errorf("git log: %w", err)
		}
	}
	if opts.MaxCount < 0 {
		return nil, fmt.Errorf("git log: max count %d must not be negative", opts.MaxCount)
	}

	format := commitFormatUnsigned
	if opts.Signatures {
		format = commitFormat
	}
	args := []string{"log", "-z", "--no-patch", "--topo-order", "--reverse", "--date=raw", "--format=" + format}
	if opts.FirstParent {
		args = append(args, "--first-parent")
	}
	if opts.MaxCount > 0 {
		args = append(args, "--max-count="+strconv.Itoa(opts.MaxCount))
	}
	args = append(args, "--end-of-options")
	args = append(args, opts.Include...)
	for _, revision := range opts.Exclude {
		args = append(args, "^"+revision)
	}
	// The trailing separator keeps a revision that happens to match a file name
	// from being read as a path.
	args = append(args, "--")

	// Same as CommitInfo: this reads content, not diagnostics.
	out, err := r.runRaw(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	commits, err := parseCommitRecords(out)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	return commits, nil
}

// parseCommitRecords splits null delimited commit records into commits.
//
// With -z git terminates every entry with a null byte and ends the whole stream
// with a trailing newline, so the fields of every commit are a fixed size window
// into one split. Both the trimmed newline and the empty token the final
// terminator leaves behind are load bearing: without either, the last record
// would carry a stray field and the count would stop dividing evenly.
//
// Null is the only byte a commit message cannot hold. git refuses to write one
// ("a NUL byte in commit log message not allowed"), so a stream that does not
// divide evenly into records, or whose record does not begin with an object
// name, means the objects themselves are corrupt. That is reported as
// unparseable rather than silently misattributed to the wrong commit.
func parseCommitRecords(out string) ([]Commit, error) {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil, nil
	}
	fields := strings.Split(out, "\x00")
	if last := len(fields) - 1; fields[last] == "" {
		fields = fields[:last]
	}
	if len(fields)%commitFieldCount != 0 {
		return nil, fmt.Errorf("got %d fields, want a multiple of %d", len(fields), commitFieldCount)
	}
	commits := make([]Commit, 0, len(fields)/commitFieldCount)
	for start := 0; start < len(fields); start += commitFieldCount {
		record := fields[start : start+commitFieldCount]
		if !isObjectName(record[commitObjectNameField]) {
			return nil, fmt.Errorf("record %d does not begin with an object name", start/commitFieldCount)
		}
		commits = append(commits, Commit{
			SHA:              record[0],
			Parents:          strings.Fields(record[1]),
			AuthorName:       record[2],
			AuthorEmail:      record[3],
			AuthorDate:       record[4],
			AuthorDateRaw:    record[5],
			CommitterName:    record[6],
			CommitterEmail:   record[7],
			CommitterDate:    record[8],
			CommitterDateRaw: record[9],
			SignatureStatus:  record[10],
			SignerKey:        record[11],
			Signer:           record[12],
			Subject:          record[13],
			RawMessage:       record[14],
			Trailers:         parseTrailers(record[15]),
		})
	}
	return commits, nil
}

// isObjectName reports whether value is a full lowercase hexadecimal object
// name in either hash algorithm.
func isObjectName(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// parseTrailers splits unfolded trailer lines into key and value pairs.
func parseTrailers(block string) []Trailer {
	var trailers []Trailer
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		trailers = append(trailers, Trailer{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
	}
	return trailers
}

// ParseTrailers reports the trailers git itself finds in a commit message.
//
// This is git's own answer rather than an approximation of it: interpret-trailers
// applies the trailer block rules, folds continuation lines, and ignores a patch
// part. It reads no repository, so it works anywhere, and it is the reference
// the pure implementation in gitgraph is checked against.
func (r *Runner) ParseTrailers(ctx context.Context, message string) ([]Trailer, error) {
	out, err := r.runRaw(ctx, []byte(message), "interpret-trailers", "--parse")
	if err != nil {
		return nil, fmt.Errorf("git interpret-trailers: %w", err)
	}
	return parseTrailers(strings.TrimSuffix(out, "\n")), nil
}

// Signature is a Git author or committer identity with an optional date. The
// date uses any format git accepts, such as RFC 3339.
type Signature struct {
	Name  string
	Email string
	Date  string
}

// env renders the signature as environment entries for the given role.
func (s Signature) env(role string) []string {
	var entries []string
	if s.Name != "" {
		entries = append(entries, "GIT_"+role+"_NAME="+s.Name)
	}
	if s.Email != "" {
		entries = append(entries, "GIT_"+role+"_EMAIL="+s.Email)
	}
	if s.Date != "" {
		entries = append(entries, "GIT_"+role+"_DATE="+s.Date)
	}
	return entries
}

// AddPaths stages exact repository relative paths. Pathspec magic and wildcards
// are rejected, and literal pathspecs are forced for the duration of the
// command, so a configured path can only ever stage the file it names.
func (r *Runner) AddPaths(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return fmt.Errorf("git add: at least one path is required")
	}
	for _, path := range paths {
		if err := validateArgument("path", path); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
		if err := validateLiteralPath(path); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
	}
	args := append([]string{"add", "--"}, paths...)
	if _, err := r.runWith(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, args...); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	return nil
}

// validateLiteralPath rejects pathspec magic and glob characters.
func validateLiteralPath(path string) error {
	if strings.HasPrefix(path, ":") {
		return fmt.Errorf("path %q must not use pathspec magic", path)
	}
	if strings.ContainsAny(path, "*?[") {
		return fmt.Errorf("path %q must name one file, not a pattern", path)
	}
	return nil
}

// CommitOptions describes one commit created through the Git command line.
type CommitOptions struct {
	Message    string
	Author     Signature
	Committer  Signature
	AllowEmpty bool
	// Sign requests a signed commit. Generated replay commits are never signed,
	// so this stays false everywhere except fixtures and bootstrap commits that
	// must carry a real signature.
	Sign bool
}

// Commit records the staged tree. Identity travels through the environment, the
// message is preserved verbatim, and signing is always off, because the engine
// only creates generated or bootstrap commits.
func (r *Runner) Commit(ctx context.Context, opts CommitOptions) error {
	if opts.Message == "" {
		return fmt.Errorf("git commit: a message is required")
	}
	args := []string{"commit", "--quiet", "--no-verify", "--cleanup=verbatim"}
	if opts.Sign {
		args = append(args, "--gpg-sign")
	} else {
		args = append(args, "--no-gpg-sign")
	}
	if opts.AllowEmpty {
		args = append(args, "--allow-empty")
	}
	args = append(args, "-m", opts.Message)
	env := append(opts.Author.env("AUTHOR"), opts.Committer.env("COMMITTER")...)
	if _, err := r.runWith(ctx, env, args...); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	return nil
}

// Push updates remote refs. There is no force variant: every refspec is checked
// and any forced or deleting refspec is rejected before the subprocess starts.
func (r *Runner) Push(ctx context.Context, remote string, refspecs ...string) error {
	if err := ValidatePushRemote(remote); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	if len(refspecs) == 0 {
		return fmt.Errorf("git push: at least one refspec is required")
	}
	for _, spec := range refspecs {
		if err := ValidatePushRefspec(spec); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}
	if err := r.assertNoRemoteRewrites(ctx); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	// The empty credential.helper resets the helper list, so a repository local
	// helper can neither be consulted nor prompt. Hooks are already disabled for
	// every command and --no-verify keeps the pre-push hook off as well.
	args := []string{"-c", "credential.helper=", "push", "--atomic", "--porcelain", "--no-verify", "--", remote}
	out, err := r.run(ctx, append(args, refspecs...)...)
	if err != nil {
		// Porcelain output carries the per ref verdict on standard output, so
		// the rejection reason would otherwise be lost.
		if verdict := rejectedRef(out); verdict != "" {
			return fmt.Errorf("git push to %q: %s: %w", redactRemote(remote), verdict, err)
		}
		return fmt.Errorf("git push to %q: %w", redactRemote(remote), err)
	}
	return nil
}

// PushUpdate is one ref update in an atomic push, bound to the value the caller
// expects the destination to hold at the moment the push runs.
type PushUpdate struct {
	// Ref is the fully qualified destination ref.
	Ref string
	// New is the full object name the ref must end up holding.
	New string
	// ExpectedOld is the full object name the destination must currently hold.
	ExpectedOld string
	// ExpectAbsent states that the ref must not exist yet. It is separate from
	// an empty ExpectedOld so that "it must not exist" cannot be spelled by
	// forgetting to say anything.
	ExpectAbsent bool
}

// PushAtomic updates remote refs in one transaction, guarding every ref with a
// compare and swap against the value the caller expects it to hold.
//
// Push lets git reject anything that is not a fast forward, and a fast forward
// check is not the same guarantee as a compare and swap. Between reading a
// remote and pushing to it, another writer can advance a branch to a commit
// this push then fast forwards straight over. Git is satisfied, and what lands
// is not the update that was reviewed, because the value it was reviewed
// against is gone. Naming the expected value moves the comparison into the ref
// transaction on the far side, where nothing can race it.
//
// The lease is a compare and swap rather than a licence to force. With a
// matching lease git will accept a rewind, so this is where the fast forward
// rule is kept instead: every update naming an expected value is proved to
// descend from it before the subprocess starts, and refused otherwise. The
// empty lease is git's own spelling of "this ref must not exist yet", which is
// what keeps a create from quietly becoming an update to whatever appeared in
// the meantime.
//
// Deleting remains impossible. Every update names an object it moves to, and
// the null object name git's protocol spells a deletion with is refused.
func (r *Runner) PushAtomic(ctx context.Context, remote string, updates []PushUpdate) error {
	if err := ValidatePushRemote(remote); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	if len(updates) == 0 {
		return fmt.Errorf("git push: at least one update is required")
	}
	seen := make(map[string]bool, len(updates))
	leases := make([]string, 0, len(updates))
	refspecs := make([]string, 0, len(updates))
	for i, update := range updates {
		if err := validatePushUpdate(update); err != nil {
			return fmt.Errorf("git push: update %d: %w", i, err)
		}
		if seen[update.Ref] {
			return fmt.Errorf("git push: update %d: ref %q appears more than once", i, update.Ref)
		}
		seen[update.Ref] = true
		if err := r.assertFastForward(ctx, update); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
		// The lease is keyed by the destination ref, and an empty expected
		// value is the form that requires the ref to be absent.
		leases = append(leases, "--force-with-lease="+update.Ref+":"+update.ExpectedOld)
		refspecs = append(refspecs, update.New+":"+update.Ref)
	}
	// The refspecs are held to the same rules a caller written one is, so the
	// one construction site inside this package is not the one that gets to
	// skip them.
	for _, spec := range refspecs {
		if err := ValidatePushRefspec(spec); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}
	if err := r.assertNoRemoteRewrites(ctx); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	args := []string{"-c", "credential.helper=", "push", "--atomic", "--porcelain", "--no-verify"}
	args = append(args, leases...)
	args = append(args, "--", remote)
	out, err := r.run(ctx, append(args, refspecs...)...)
	if err != nil {
		if verdict := rejectedRef(out); verdict != "" {
			return fmt.Errorf("git push to %q: %s: %w", redactRemote(remote), verdict, err)
		}
		return fmt.Errorf("git push to %q: %w", redactRemote(remote), err)
	}
	return nil
}

// assertFastForward proves that an update naming an expected value moves
// forward from it.
//
// The lease replaces git's own fast forward check rather than adding to it, so
// the rule has to be kept here. An update whose expected value is not in this
// repository is refused for the same reason: nothing can be proved about a
// history that is not present, and the lease alone would let the push rewind
// the ref.
func (r *Runner) assertFastForward(ctx context.Context, update PushUpdate) error {
	if update.ExpectedOld == "" {
		return nil
	}
	forward, err := r.IsAncestor(ctx, update.ExpectedOld, update.New)
	if err != nil {
		return fmt.Errorf("%q: %w", update.Ref, err)
	}
	if !forward {
		return fmt.Errorf("%q: %s does not descend from %s: %w", update.Ref, update.New, update.ExpectedOld, ErrForceRefspec)
	}
	return nil
}

// validatePushUpdate checks one update of an atomic push.
func validatePushUpdate(update PushUpdate) error {
	// A leading plus is git's force marker. It cannot survive into a refspec
	// this package builds, but a caller that wrote one meant to force, and
	// answering that intent with a silent fast forward would hide the
	// disagreement rather than settle it.
	if strings.HasPrefix(update.Ref, "+") {
		return fmt.Errorf("ref %q: %w", update.Ref, ErrForceRefspec)
	}
	if err := ValidateRefName(update.Ref); err != nil {
		return err
	}
	if err := validatePushObject("new object", update.New); err != nil {
		return fmt.Errorf("ref %q: %w", update.Ref, err)
	}
	switch {
	case update.ExpectAbsent && update.ExpectedOld != "":
		return fmt.Errorf("ref %q cannot both expect no ref and expect object %s", update.Ref, update.ExpectedOld)
	case !update.ExpectAbsent && update.ExpectedOld == "":
		return fmt.Errorf("ref %q must state the value the remote is expected to hold, or that it must not exist", update.Ref)
	case update.ExpectedOld != "":
		if err := validatePushObject("expected object", update.ExpectedOld); err != nil {
			return fmt.Errorf("ref %q: %w", update.Ref, err)
		}
	}
	return nil
}

// validatePushObject rejects anything that is not a full object name, and the
// null object name, which is how git's protocol spells a deletion.
func validatePushObject(kind, name string) error {
	if !isObjectName(name) {
		return fmt.Errorf("%s %q must be a full object name", kind, name)
	}
	if strings.Trim(name, "0") == "" {
		return fmt.Errorf("%s is the null object name: %w", kind, ErrDeleteRefspec)
	}
	return nil
}

// assertNoRemoteRewrites fails closed when configuration this repository can
// see would silently redirect a command that names a remote explicitly.
//
// Every scope is queried, not just the local one. Global and system
// configuration are already neutralised for every subprocess, so what remains
// is the repository's own config and its per work tree config, and the latter
// is invisible to a --local query: a rewrite parked in config.worktree would
// pass a local-only check and still redirect the transfer.
//
// The gate does not depend on the target's scheme. A file mirror is redirected
// by exactly the same mechanism as an https remote, and a test that proves the
// gate on a file URL is the test that proves it for github.com.
func (r *Runner) assertNoRemoteRewrites(ctx context.Context) error {
	for _, pattern := range rewritePatterns {
		out, err := r.run(ctx, "config", "--name-only", "--get-regexp", "--end-of-options", pattern)
		switch {
		case err == nil:
			names := strings.Fields(out)
			slices.Sort(names)
			return fmt.Errorf("repository configuration %s rewrites the remote", strings.Join(slices.Compact(names), ", "))
		case ExitCodeOf(err) == 1:
		default:
			return fmt.Errorf("read remote rewrite configuration: %w", err)
		}
	}
	return nil
}

// rewritePatterns match the configuration keys that redirect a transfer whose
// remote was named explicitly on the command line. insteadOf rewrites the URL
// git connects to, pushInsteadOf does the same for a push, and a remote's
// pushurl replaces the target of one.
var rewritePatterns = []string{
	`^url\..*\.(insteadof|pushinsteadof)$`,
	`^remote\..*\.pushurl$`,
}

// rejectedRef returns the first rejected ref line of git push porcelain output.
func rejectedRef(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "!") {
			return strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(line, "!"), "\t", " "))
		}
	}
	return ""
}

// redactRemote renders a remote without any user information so an error can
// never echo a credential that was passed by mistake. Both URL and scp like
// forms are covered, and an unparseable value is replaced entirely.
func redactRemote(remote string) string {
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return Placeholder
		}
		if parsed.User == nil {
			return remote
		}
		// The placeholder carries no characters that URL encoding would escape.
		parsed.User = url.User(redactedUser)
		return parsed.String()
	}
	if at := strings.LastIndex(remote, "@"); at >= 0 {
		return redactedUser + "@" + remote[at+1:]
	}
	return remote
}

// ValidateRemote checks a fetch or local push target. Credentials may never
// travel in a remote URL, so any embedded user information is rejected.
func ValidateRemote(remote string) error {
	if err := validateRemoteArgument(remote); err != nil {
		return err
	}
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return fmt.Errorf("remote %q is malformed: %s", redactRemote(remote), urlErrorReason(err))
		}
		switch parsed.Scheme {
		case "https", "file":
		default:
			return fmt.Errorf("remote %q must use https or file", redactRemote(remote))
		}
		if parsed.User != nil {
			return fmt.Errorf("remote %q must not embed credentials", redactRemote(remote))
		}
		return nil
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	if strings.ContainsAny(remote, ":@") {
		return fmt.Errorf("remote %q must be a remote name, an absolute path, or an https URL", redactRemote(remote))
	}
	return nil
}

// ValidatePushRemote checks a push target. A push may carry credentials, so the
// target must be explicit: named remotes are rejected because their URL lives in
// configuration, and an https target must be the one host the engine publishes
// to. Absolute paths and file URLs remain available for local verification.
func ValidatePushRemote(remote string) error {
	if err := ValidateRemote(remote); err != nil {
		return err
	}
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return fmt.Errorf("remote %q is malformed: %s", redactRemote(remote), urlErrorReason(err))
		}
		if parsed.Scheme == "https" && parsed.Hostname() != PublishHost {
			return fmt.Errorf("remote %q must publish to %s", redactRemote(remote), PublishHost)
		}
		return nil
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	return fmt.Errorf("remote %q must be an absolute path or an https URL, a named remote hides its target in configuration", redactRemote(remote))
}

// urlErrorReason reports why a URL failed to parse without echoing the URL,
// which url.Error embeds in its own message.
func urlErrorReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return "unparseable"
}

// validateRemoteArgument rejects empty, option like, and null bearing remotes
// without echoing a credential.
func validateRemoteArgument(remote string) error {
	switch {
	case remote == "":
		return errors.New("remote must not be empty")
	case strings.HasPrefix(remote, "-"):
		return fmt.Errorf("remote %q: %w", redactRemote(remote), ErrFlagLikeArgument)
	case strings.ContainsRune(remote, '\x00'):
		return fmt.Errorf("remote %q must not contain a null byte", redactRemote(remote))
	}
	return nil
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// leadingDigits returns the leading decimal digits of s.
func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}
