// Package gocli is the only Go toolchain subprocess boundary in the engine.
//
// It exists for the same reason gitcli does. Every command is built as an
// explicit argument vector and executed with exec.CommandContext, so no module
// path, version query, or package pattern is ever handed to a shell. The
// difference from gitcli is what has to be neutralised: the go command reads a
// configuration file that go env -w writes, a workspace file, and a dozen
// environment variables that silently change which modules are trusted, where
// they come from, and whether the answer is reproducible. All of it is turned
// off here rather than at each call site, because a default that has to be
// remembered is a default that will eventually be forgotten.
package gocli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// DefaultBinary is the Go executable looked up on PATH when none is given.
const DefaultBinary = "go"

// defaultInherit lists the process environment variables a Go subprocess may
// see. Everything else is dropped so runs do not depend on ambient state. HOME
// is present because the go command derives its module and build caches from it
// when they are not named explicitly, and a run without either fails outright.
var defaultInherit = []string{"PATH", "HOME", "SYSTEMROOT"}

// DefaultProxy is the module proxy used when Options.Proxy is empty.
//
// It is deliberately not the go command's own default, which is
// "https://proxy.golang.org,direct". That trailing "direct" is a fallback to
// fetching straight from version control, which turns a module resolution into
// git running against an arbitrary host with whatever credentials it can find.
// The engine resolves public modules through the proxy and the checksum
// database, so the fallback is removed rather than relied upon, and a caller
// that genuinely needs another proxy states it.
const DefaultProxy = "https://proxy.golang.org"

// ProxyOff disables module downloads entirely, which is what an offline
// resolution asks for.
const ProxyOff = "off"

// fixedEnv is applied to every Go subprocess, after everything else, so neither
// a caller entry nor an inherited value can override it.
//
// Each entry closes a way for ambient state to change the answer or to reach
// the network on its own:
//
//   - GOENV=off ignores the configuration file go env -w writes, which is the
//     go command's equivalent of a global gitconfig and would otherwise supply
//     every variable below from a file no engine run ever inspects.
//   - GOWORK=off keeps a workspace file found above the module directory from
//     silently replacing the module being resolved.
//   - GOFLAGS is emptied because it can inject any flag into any command,
//     including -mod=mod, which turns a read into a rewrite of go.mod. The
//     one exception is Options.ModCacheRW, which appends GOFLAGS=-modcacherw
//     after the fixed empty entry to make the module cache writable. That is
//     a typed boolean rather than an open string, so an arbitrary flag still
//     cannot reach the subprocess through this route.
//   - GOPRIVATE, GONOPROXY, GONOSUMDB, and GOINSECURE are emptied rather than
//     set, because each one is a list of exemptions: empty means no module is
//     exempt from the proxy and the checksum database, which is the safe
//     direction. Emptying GOSUMDB would be the unsafe one, so it is left at its
//     default, and GOPROXY is a typed option instead.
//   - GOTOOLCHAIN=local refuses to download a different toolchain to satisfy a
//     go directive. The engine pins an exact patch release because generated
//     formatting must be byte identical, so a silent switch is precisely the
//     event that must fail loudly instead. It pins the switch, not the version,
//     which is what ValidateToolchain is for.
//   - GOVCS=*:off refuses to run a version control tool to fetch a module. With
//     DefaultProxy there is no direct fallback to trigger one, so this is the
//     second lock on the same door.
//   - GOAUTH=off stops the go command from finding credentials for module
//     fetches. Its default is "netrc", which reads ~/.netrc, so without this an
//     operator's stored passwords are in scope for every resolution. NETRC is
//     pointed at the null device for the tools that read it directly.
//   - The GIT_* entries neutralise the operator's Git configuration for the
//     case where a VCS fetch happens anyway. They mirror gitcli's own fixed
//     environment: no system or global config, no terminal prompt, no askpass
//     helper. A go command that shells out to git must not inherit what gitcli
//     itself refuses to inherit.
//   - The C locale keeps parsed output stable.
var fixedEnv = []string{
	"GOENV=off",
	"GOWORK=off",
	"GOFLAGS=",
	"GOPRIVATE=",
	"GONOPROXY=",
	"GONOSUMDB=",
	"GOINSECURE=",
	"GOTOOLCHAIN=local",
	"GOVCS=*:off",
	"GOAUTH=off",
	"NETRC=" + os.DevNull,
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_SYSTEM=" + os.DevNull,
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"LC_ALL=C",
	"LANG=C",
}

// proxyVariable is owned by Options.Proxy rather than by fixedEnv, so it is
// named separately where both need to refuse a caller entry for it.
const proxyVariable = "GOPROXY"

// fixedNames is the set of variables the fixed entries own. It is derived from
// fixedEnv rather than written out again, so the two cannot drift apart.
var fixedNames = envNames(fixedEnv)

// isolationVariables are the variables an Isolation entry may name.
//
// Isolation decides where the go command keeps state; it does not decide what
// the go command trusts. Restricting it to this set is what makes the fixed
// entries a policy rather than a default: without it a caller could re-enable
// GOFLAGS, a workspace, or a VCS fetch through the same field that redirects a
// cache, and would do it by accident rather than on purpose.
var isolationVariables = map[string]bool{
	"HOME":            true,
	"TMPDIR":          true,
	"TEMP":            true,
	"TMP":             true,
	"GOCACHE":         true,
	"GOMODCACHE":      true,
	"GOPATH":          true,
	"GOTMPDIR":        true,
	"XDG_CACHE_HOME":  true,
	"XDG_CONFIG_HOME": true,
}

// envNames reports the variable names an entry list sets.
func envNames(entries []string) map[string]bool {
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names[name] = true
		}
	}
	return names
}

// commandWaitDelay bounds how long a killed subprocess and its descendants may
// keep the output pipes open. The go command starts compilers and its own
// helpers, so without it a grandchild holding a pipe would block cancellation
// forever.
const commandWaitDelay = 5 * time.Second

// Options configures a Runner.
type Options struct {
	// Binary is the Go executable. Empty means look up DefaultBinary on PATH.
	Binary string
	// Dir is the working directory for every command. It must be absolute, so a
	// module location never depends on the process working directory. Empty
	// leaves the runner without one, which the module commands refuse.
	Dir string
	// Inherit names the process environment variables to pass through. Empty
	// means the default set. The values are read once, here, so a later change
	// to the process environment cannot alter what an already built runner does.
	// A name the fixed entries own is inherited and then overridden by them.
	Inherit []string
	// Isolation holds KEY=VALUE entries that decide where the go command keeps
	// state, such as GOCACHE, GOMODCACHE, GOPATH, HOME, or TMPDIR. Only those
	// variables may be named: see isolationVariables for why a cache knob and a
	// trust knob must not travel through the same field. They carry no
	// credential, so they are not seeded into the redactor, because a path is not
	// a secret and redacting it would corrupt output that names it.
	Isolation []string
	// Env holds additional KEY=VALUE entries, which is how a credential reaches
	// the go command. Every non-empty value is seeded into the redactor, so an
	// entry cannot leak by being forgotten in Secrets. It may not name a variable
	// the fixed entries own or GOPROXY, both of which have their own typed route.
	Env []string
	// Proxy is the module proxy. Empty means DefaultProxy, and ProxyOff disables
	// downloads. Its value is seeded into the redactor because a proxy URL is a
	// normal place for a token to live.
	Proxy string
	// Secrets holds additional exact values that must never appear in captured
	// output.
	Secrets []string
	// ModCacheRW makes the module cache writable by setting GOFLAGS=-modcacherw.
	// Without it the go command marks downloaded modules read-only, which is
	// correct for production but prevents test cleanup on container file systems
	// where chmod is restricted. This field is the only way to inject GOFLAGS,
	// and it carries a single well-understood flag rather than an open string.
	ModCacheRW bool
	// OutputLimit bounds the bytes one command may return on each stream. Zero
	// means DefaultOutputLimit and a negative value is rejected.
	OutputLimit int64
}

// Runner executes Go commands with a controlled environment.
type Runner struct {
	binary string
	dir    string
	// inherited, isolation, and proxy are kept alongside the assembled
	// environment because LoaderEnv builds a second environment from the same
	// isolation and proxy while deliberately leaving the Env entries out of it.
	// Recovering them from env instead would mean parsing back a list that no
	// longer records which entry came from where, and the one entry that must
	// not be recovered by accident is the one carrying a credential.
	inherited   []string
	isolation   []string
	proxy       string
	modCacheRW  bool
	env         []string
	outputLimit int64
	redactor    *gitcli.Redactor
}

// New resolves the Go executable and builds the subprocess environment.
func New(ctx context.Context, opts Options) (*Runner, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("go runner: %w", err)
	}
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if !strings.ContainsRune(binary, filepath.Separator) && !strings.ContainsRune(binary, '/') {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			return nil, fmt.Errorf("go executable lookup: %w", err)
		}
		binary = resolved
	}
	if opts.Dir != "" {
		if err := validateWorkingDirectory(opts.Dir); err != nil {
			return nil, err
		}
	}
	for i, entry := range opts.Isolation {
		if err := validateIsolationEntry(i, entry); err != nil {
			return nil, fmt.Errorf("go runner: %w", err)
		}
	}
	for i, entry := range opts.Env {
		if err := validateExtraEntry(i, entry); err != nil {
			return nil, fmt.Errorf("go runner: %w", err)
		}
	}
	proxy := opts.Proxy
	if proxy == "" {
		proxy = DefaultProxy
	}
	if err := validateProxy(proxy); err != nil {
		return nil, fmt.Errorf("go runner: %w", err)
	}
	limit := opts.OutputLimit
	switch {
	case limit < 0:
		return nil, fmt.Errorf("go runner: output limit %d must not be negative", limit)
	case limit == 0:
		limit = DefaultOutputLimit
	}
	inherited := inheritedEnv(opts.Inherit)
	return &Runner{
		binary:      binary,
		dir:         opts.Dir,
		inherited:   inherited,
		isolation:   slices.Clone(opts.Isolation),
		proxy:       proxy,
		modCacheRW:  opts.ModCacheRW,
		env:         assembleEnv(inherited, opts.Isolation, opts.Env, proxy, opts.ModCacheRW),
		outputLimit: limit,
		// The proxy is seeded alongside the environment values because a proxy
		// URL is a normal place for a token to live.
		redactor: gitcli.NewRedactor(append(append(envValues(opts.Env), proxyCredential(proxy)), opts.Secrets...)...),
	}, nil
}

// Binary reports the resolved Go executable path.
func (r *Runner) Binary() string { return r.binary }

// Dir reports the working directory used for every command.
func (r *Runner) Dir() string { return r.dir }

// Redactor reports the redactor seeded with this runner's secrets.
func (r *Runner) Redactor() *gitcli.Redactor { return r.redactor }

// WithDir returns a copy of the runner that operates in dir.
//
// The directory is checked exactly as New checks one, because a runner pointed
// at a path that was never created would otherwise run its first command in the
// process working directory, where the go command would find whatever module
// happens to contain it. The path must be absolute for the same reason.
func (r *Runner) WithDir(dir string) (*Runner, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("go working directory %q must be absolute", dir)
	}
	if err := validateDirectory(dir); err != nil {
		return nil, err
	}
	clone := *r
	clone.dir = dir
	return &clone, nil
}

// validateWorkingDirectory checks that a working directory is absolute, exists,
// and is a directory.
//
// Absolute is required for the same reason WithDir requires it: a relative path
// resolves against the process working directory, and the go command would then
// search upwards from somewhere the caller never named.
func validateWorkingDirectory(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("go working directory %q must be absolute", dir)
	}
	return validateDirectory(dir)
}

// validateDirectory checks that a working directory exists and is a directory.
func validateDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("go working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("go working directory: %q is not a directory", dir)
	}
	return nil
}

// entryName reports the variable an entry names, rejecting a malformed one.
//
// A malformed entry is reported by position rather than quoted. The whole entry
// is the value when there is no separator, Env is where credentials arrive, and
// this runs before the redactor that would have masked it exists, so quoting it
// would be the one way a token reaches a log through this package.
func entryName(kind string, index int, entry string) (string, error) {
	name, _, ok := strings.Cut(entry, "=")
	switch {
	case !ok:
		return "", fmt.Errorf("%s %d must be KEY=VALUE", kind, index)
	case name == "":
		return "", fmt.Errorf("%s %d must name a variable", kind, index)
	case strings.ContainsRune(entry, '\x00'):
		return "", fmt.Errorf("%s %q must not contain a null byte", kind, name)
	}
	return name, nil
}

// validateIsolationEntry confines Isolation to the state and cache variables.
func validateIsolationEntry(index int, entry string) error {
	name, err := entryName("isolation entry", index, entry)
	if err != nil {
		return err
	}
	if !isolationVariables[name] {
		return fmt.Errorf("isolation entry %q must name a state or cache variable, one of %s",
			name, strings.Join(sortedKeys(isolationVariables), ", "))
	}
	return nil
}

// validateExtraEntry keeps an Env entry away from the variables that decide what
// the go command trusts.
func validateExtraEntry(index int, entry string) error {
	name, err := entryName("environment entry", index, entry)
	if err != nil {
		return err
	}
	if fixedNames[name] {
		return fmt.Errorf("environment entry %q is fixed by this package and must not be overridden", name)
	}
	if name == proxyVariable {
		return fmt.Errorf("environment entry %q must be set through the Proxy option", name)
	}
	return nil
}

// validateProxy rejects a proxy value the go command would not read as one.
//
// The rejected value is never echoed, for the same reason entryName does not
// echo a malformed entry: a proxy URL is a normal place for a token to live,
// and this runs before the redactor that would have masked it exists. The
// message names the accepted forms instead, which is what a caller needs.
func validateProxy(proxy string) error {
	if strings.ContainsRune(proxy, '\x00') {
		return errors.New("proxy must not contain a null byte")
	}
	if proxy == ProxyOff || proxy == "direct" {
		return nil
	}
	for entry := range strings.SplitSeq(proxy, ",") {
		switch {
		case entry == ProxyOff, entry == "direct":
		case strings.HasPrefix(entry, "https://"):
		case strings.HasPrefix(entry, "file://"):
		default:
			// A plain http proxy would carry the module bytes, and any token in
			// the URL, over a cleartext connection.
			return errors.New("proxy must be off, direct, or a comma separated list of https and file URLs")
		}
	}
	return nil
}

// proxyCredential reports the user information embedded in a proxy URL, which is
// the part that must never reach captured output. The URL itself is not a
// secret and redacting all of it would corrupt every diagnostic that names it.
func proxyCredential(proxy string) string {
	for entry := range strings.SplitSeq(proxy, ",") {
		parsed, err := url.Parse(entry)
		if err != nil || parsed.User == nil {
			continue
		}
		if password, ok := parsed.User.Password(); ok && password != "" {
			return password
		}
	}
	return ""
}

// sortedKeys renders a set in a stable order for an error message.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
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

// assembleEnv joins the inherited snapshot, the isolation entries, the caller
// supplied entries, and finally the fixed entries and the proxy.
//
// The fixed entries come last on purpose. Later entries win, so putting them at
// the end is what makes them fixed: no inherited value and no caller entry can
// reach the subprocess ahead of them. Validation refuses those entries too, so
// the ordering is a second lock rather than the only one.
func assembleEnv(inherited, isolation, extra []string, proxy string, modCacheRW bool) []string {
	env := make([]string, 0, len(inherited)+len(isolation)+len(extra)+len(fixedEnv)+2)
	env = append(env, inherited...)
	env = append(env, isolation...)
	env = append(env, extra...)
	env = append(env, fixedEnv...)
	if modCacheRW {
		// Override the fixed GOFLAGS= with the single safe flag that makes
		// the module cache writable. It is appended last so it wins.
		env = append(env, "GOFLAGS=-modcacherw")
	}
	return append(env, proxyVariable+"="+proxy)
}

// ExecError reports a Go command that exited non-zero or could not run. Every
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
	message := fmt.Sprintf("go %s", strings.Join(e.Args, " "))
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

// run executes one Go command and returns its redacted standard output.
//
// There is no exported form. A caller that could pass an arbitrary argument
// vector could pass -exec, -toolexec, or -ldflags, each of which turns a query
// into arbitrary execution, so every command this package can run is a method
// that names its own flags.
//
// Standard input is never inherited, so a subprocess can neither read from the
// operator's terminal nor block the engine waiting for one. Both streams are
// bounded: a module the engine does not control decides how much a go command
// prints, so an unbounded capture would let it decide how much memory the
// engine spends.
func (r *Runner) run(ctx context.Context, args ...string) (string, error) {
	stdout := &boundedBuffer{limit: r.outputLimit}
	stderr := &boundedBuffer{limit: r.outputLimit}
	outWriter := r.redactor.Writer(stdout)
	errWriter := r.redactor.Writer(stderr)

	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	cmd.Stdin = nil
	cmd.Stdout = outWriter
	cmd.Stderr = errWriter
	cmd.WaitDelay = commandWaitDelay

	runErr := cmd.Run()
	closeErr := errors.Join(outWriter.Close(), errWriter.Close())
	if closeErr != nil {
		closeErr = fmt.Errorf("go output capture: %w", closeErr)
	}
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), errors.Join(fmt.Errorf("go %s: %w", strings.Join(r.redactor.Strings(args), " "), ctxErr), closeErr)
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return stdout.String(), errors.Join(&ExecError{
			Args:     r.redactor.Strings(args),
			ExitCode: exitCode,
			Stderr:   stderr.String(),
			Err:      runErr,
		}, closeErr)
	}
	// A truncated stream is reported rather than parsed, because a JSON decoder
	// handed the first half of a response would report a syntax error and hide
	// what actually happened.
	if err := errors.Join(stdout.overflow("standard output"), stderr.overflow("standard error")); err != nil {
		return stdout.String(), errors.Join(fmt.Errorf("go %s: %w", strings.Join(r.redactor.Strings(args), " "), err), closeErr)
	}
	return stdout.String(), closeErr
}

// DefaultOutputLimit bounds one stream of one command. It is generous enough for
// a go list -deps -json over a dependency graph the size of Kubernetes and small
// enough that a runaway command fails instead of exhausting memory.
const DefaultOutputLimit = 512 << 20

// boundedBuffer accumulates output up to a limit and remembers that it stopped.
//
// It keeps accepting writes after the limit so the pipe drains and the command
// exits on its own; what it stops doing is retaining them.
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
			return 0, fmt.Errorf("buffer go output: %w", err)
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

// requireModuleDir fails a command that needs a main module when the runner has
// no explicit directory.
//
// The go command walks upwards looking for go.mod exactly as git walks upwards
// looking for a repository, and gitcli pins that walk with a ceiling for the
// same reason: a directory that was never created, or that lost its go.mod,
// would otherwise hand the command whichever module happens to sit above it.
// There is no ceiling variable for the go command, so the check is made here
// before the subprocess starts.
func (r *Runner) requireModuleDir() error {
	if r.dir == "" {
		return errors.New("a module directory is required, build the runner with Dir or WithDir")
	}
	path := filepath.Join(r.dir, "go.mod")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("module directory %q: %w", r.dir, err)
	}
	return nil
}

// validateArgument rejects values the go command would read as an option or
// that would break a line delimited response.
func validateArgument(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q must not start with a dash", kind, value)
	}
	for _, r := range value {
		if r == '\x00' {
			return fmt.Errorf("%s %q must not contain a null byte", kind, value)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s %q must not contain control characters", kind, value)
		}
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

// endOfOptions terminates flag parsing so a validated argument that still
// resembles one is passed through as a value.
const endOfOptions = "--"

// appendArguments validates and appends caller supplied arguments after the
// option terminator.
func appendArguments(args []string, kind string, values []string) ([]string, error) {
	for _, value := range values {
		if err := validateArgument(kind, value); err != nil {
			return nil, err
		}
	}
	args = append(args, endOfOptions)
	return append(args, values...), nil
}

// sortedNames renders map keys in a stable order for an error message.
func sortedNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
