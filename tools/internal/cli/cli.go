// Package cli dispatches soapbox subcommands.
//
// Dispatch uses the standard library flag package. Every command writes to
// injected writers and returns an error, so the process exit code is decided in
// exactly one place and no code path calls os.Exit or panics on bad input.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/doctor"
)

// Version is the engine version reported by the version command.
const Version = buildinfo.Version

// Process exit codes. They are part of the workflow contract, so they are
// stable and distinct.
const (
	// ExitOK reports success.
	ExitOK = 0
	// ExitFailure reports an unexpected runtime failure.
	ExitFailure = 1
	// ExitUsage reports a malformed command line.
	ExitUsage = 2
	// ExitCheck reports that the command ran and found policy violations.
	ExitCheck = 3
	// ExitCanceled reports that the context ended before the command did.
	ExitCanceled = 4
)

// Env carries the injectable process environment of one command run.
type Env struct {
	// Stdout receives command output. A nil writer discards it.
	Stdout io.Writer
	// Stderr receives diagnostics. A nil writer discards it.
	Stderr io.Writer
	// Dir is the working directory relative paths resolve against.
	Dir string
}

// normalize replaces nil writers so commands never write to a nil interface.
func (e Env) normalize() Env {
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	return e
}

// resolve resolves a possibly relative path against the environment directory.
func (e Env) resolve(path string) string {
	if path == "" {
		return e.Dir
	}
	if filepath.IsAbs(path) || e.Dir == "" {
		return path
	}
	return filepath.Join(e.Dir, path)
}

// usageError reports a malformed command line. It carries the usage block that
// belongs to the failing scope, so a bad command flag prints that command's
// flags and never the top level usage as well.
type usageError struct {
	err   error
	usage func(io.Writer)
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...), usage: writeUsage}
}

// checkError reports that a command ran successfully and found violations. The
// command has already written the detail, so only the exit code remains.
//
// The cause is carried rather than discarded. A cancellation reaches the
// dispatcher wrapped in whatever the interrupted phase reported, and a plan may
// wrap that again as a stage failure, so a check error that severed its chain
// would make an interrupted run exit as a finding about the profile. The
// dispatcher tests for cancellation before it tests for this type, which only
// works while the chain survives.
type checkError struct {
	summary string
	err     error
}

func (e *checkError) Error() string { return e.summary }
func (e *checkError) Unwrap() error { return e.err }

// command is one dispatchable subcommand.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env Env, args []string) error
}

// Command definitions. They are functions so the help output and the running
// command always share one description.
func doctorCommand() command {
	return command{name: "doctor", summary: "check the local toolchain, identity, and signing policy", run: runDoctor}
}

func validateCommand() command {
	return command{name: "validate", summary: "decode and validate the soapbox.yaml profile", run: runValidate}
}

func versionCommand() command {
	return command{name: "version", summary: "print the engine version", run: runVersion}
}

func planCommand() command {
	return command{name: "plan", summary: "compute the extraction plan for one upstream ref", run: runPlan}
}

func generateCommand() command {
	return command{name: "generate", summary: "compose the generated module for one upstream release tag", run: runGenerate}
}

func syncCommand() command {
	return command{name: "sync", summary: "plan, and with an approval publish, one upstream release", run: runSync}
}

func setupCommand() command {
	return command{name: "setup", summary: "transform this template checkout into one derived repository", run: runSetup}
}

func upgradeCommand() command {
	return command{name: "upgrade", summary: "upgrade setup-owned files in a derived repository to a new engine release", run: runUpgrade}
}

func helpCommand() command {
	return command{name: "help", summary: "print usage for soapbox or one command", run: runHelp}
}

// commands are listed in the order the help output uses.
func commands() []command {
	return []command{
		doctorCommand(), validateCommand(), planCommand(), generateCommand(),
		syncCommand(), setupCommand(), upgradeCommand(), versionCommand(), helpCommand(),
	}
}

// Run dispatches one command line and returns the process exit code.
func Run(ctx context.Context, env Env, args []string) int {
	env = env.normalize()
	err := dispatch(ctx, env, args)
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(env.Stderr, "soapbox: %v\n", err)
		return ExitCanceled
	case errors.Is(err, errHelpHandled), errors.Is(err, flag.ErrHelp):
		return ExitOK
	}
	var usage *usageError
	if errors.As(err, &usage) {
		fmt.Fprintf(env.Stderr, "soapbox: %v\n", usage)
		if usage.usage != nil {
			usage.usage(env.Stderr)
		}
		return ExitUsage
	}
	var check *checkError
	if errors.As(err, &check) {
		fmt.Fprintf(env.Stderr, "soapbox: %v\n", check)
		return ExitCheck
	}
	fmt.Fprintf(env.Stderr, "soapbox: %v\n", err)
	return ExitFailure
}

// dispatch selects and runs one command.
func dispatch(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return usagef("no command given")
	}
	name := args[0]
	if strings.HasPrefix(name, "-") {
		switch name {
		case "-h", "--help", "-help":
			writeUsage(env.Stdout)
			return nil
		default:
			return usagef("unknown flag %q, flags follow the command name", name)
		}
	}
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd.run(ctx, env, args[1:])
		}
	}
	return usagef("unknown command %q", name)
}

// newFlagSet builds a flag set that neither exits nor prints on its own. All
// rendering happens in one place so a message cannot appear twice.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("soapbox "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// commandUsage renders one command's summary and flags.
func commandUsage(cmd command, fs *flag.FlagSet) func(io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "soapbox %s: %s\n", cmd.name, cmd.summary)
		if countFlags(fs) == 0 {
			return
		}
		fmt.Fprintln(w, "flags:")
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
}

// countFlags reports how many flags a command defines.
func countFlags(fs *flag.FlagSet) int {
	count := 0
	fs.VisitAll(func(*flag.Flag) { count++ })
	return count
}

// parseFlags parses command flags and rejects unexpected operands. A help
// request prints the command usage to standard output exactly once and ends the
// command successfully.
func parseFlags(env Env, cmd command, fs *flag.FlagSet, args []string) error {
	usage := commandUsage(cmd, fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(env.Stdout)
			return errHelpHandled
		}
		return &usageError{err: err, usage: usage}
	}
	if fs.NArg() > 0 {
		return &usageError{
			err:   fmt.Errorf("%s takes no arguments, got %q", fs.Name(), strings.Join(fs.Args(), " ")),
			usage: usage,
		}
	}
	return nil
}

// errHelpHandled reports that usage was already printed for a help request.
var errHelpHandled = errors.New("help printed")

// runDoctor reports the local toolchain, identity, and signing state.
func runDoctor(ctx context.Context, env Env, args []string) error {
	fs, flags := doctorFlagSet()
	if err := parseFlags(env, doctorCommand(), fs, args); err != nil {
		return err
	}
	report, err := doctor.Run(ctx, doctor.Options{Dir: env.resolve(*flags.dir)})
	if err != nil {
		return err
	}
	if err := report.Write(env.Stdout); err != nil {
		return err
	}
	failures := report.Failures()
	if len(failures) == 0 {
		return nil
	}
	names := make([]string, 0, len(failures))
	for _, check := range failures {
		names = append(names, check.Name)
	}
	return &checkError{summary: fmt.Sprintf("%d required checks failed: %s", len(names), strings.Join(names, ", "))}
}

// doctorFlags holds the parsed doctor flags.
type doctorFlags struct {
	dir *string
}

func doctorFlagSet() (*flag.FlagSet, *doctorFlags) {
	fs := newFlagSet("doctor")
	return fs, &doctorFlags{
		dir: fs.String("dir", ".", "repository directory to inspect"),
	}
}

// profileFormats are the supported validate output formats, in help order.
var profileFormats = []string{"summary", "canonical", "profile"}

// runValidate decodes and validates a profile.
func runValidate(ctx context.Context, env Env, args []string) error {
	fs, flags := validateFlagSet()
	if err := parseFlags(env, validateCommand(), fs, args); err != nil {
		return err
	}
	// The format is checked before any file is read so an unusable command line
	// fails the same way whether or not a profile exists.
	if !slices.Contains(profileFormats, *flags.format) {
		return &usageError{
			err:   fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(profileFormats, ", ")),
			usage: commandUsage(validateCommand(), fs),
		}
	}
	resolved := *flags.path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(env.resolve(*flags.dir), resolved)
	}
	cfg, err := config.Load(ctx, resolved)
	if err != nil {
		return profileError(env, resolved, err)
	}
	switch *flags.format {
	case "canonical":
		return writeBytes(env.Stdout, cfg.Canonical)
	case "profile":
		return writeBytes(env.Stdout, cfg.ProfileBytes)
	default:
		return writeSummary(env.Stdout, resolved, cfg)
	}
}

// profileError separates a profile the operator can fix from a filesystem or
// runtime failure. Content problems, including unknown fields and malformed
// documents, are policy failures.
func profileError(env Env, path string, err error) error {
	var invalid *config.ValidationError
	if errors.As(err, &invalid) {
		fmt.Fprintf(env.Stderr, "%v\n", err)
		return &checkError{summary: fmt.Sprintf("%s has %d problems", path, len(invalid.Problems))}
	}
	var decode *config.DecodeError
	if errors.As(err, &decode) {
		fmt.Fprintf(env.Stderr, "%v\n", err)
		return &checkError{summary: fmt.Sprintf("%s could not be decoded", path)}
	}
	return err
}

// validateFlags holds the parsed validate flags.
type validateFlags struct {
	dir    *string
	path   *string
	format *string
}

func validateFlagSet() (*flag.FlagSet, *validateFlags) {
	fs := newFlagSet("validate")
	return fs, &validateFlags{
		dir:    fs.String("dir", ".", "directory that holds the profile"),
		path:   fs.String("config", config.DefaultFileName, "profile path relative to -dir"),
		format: fs.String("format", profileFormats[0], "output format: "+strings.Join(profileFormats, ", ")),
	}
}

// writeBytes renders one of the deterministic profile encodings.
func writeBytes(w io.Writer, encode func() ([]byte, error)) error {
	data, err := encode()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	return nil
}

// writeSummary renders the fields an operator checks most often.
func writeSummary(w io.Writer, path string, cfg *config.Config) error {
	mapped, err := config.MapReleaseTag(cfg.Release.Policy, cfg.Source.Refs.MinimumRelease)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is a valid soapbox profile\n", path)
	fmt.Fprintf(&b, "  source        %s at %s\n", cfg.Source.ImportPrefix, cfg.Source.Repository)
	fmt.Fprintf(&b, "  destination   %s in %s\n", cfg.Destination.Module, cfg.Destination.Repository)
	fmt.Fprintf(&b, "  packages      %d roots, recursive=%t\n", len(cfg.Packages.Roots), cfg.Packages.Recursive)
	fmt.Fprintf(&b, "  prune         %d files, %d required\n", len(cfg.Prune.Files), len(cfg.Prune.Required))
	fmt.Fprintf(&b, "  deny          %d imports\n", len(cfg.Deny.Imports))
	fmt.Fprintf(&b, "  types         %s with %d pairs\n", cfg.Types.Policy, len(cfg.Types.Pairs))
	fmt.Fprintf(&b, "  dependencies  %s with %d copied packages\n", cfg.Dependencies.Policy, len(cfg.Dependencies.CopyPackages))
	fmt.Fprintf(&b, "  patches       %d entries\n", len(cfg.Patches))
	fmt.Fprintf(&b, "  facade        %d exports, %d aliases, %d assertions\n",
		len(cfg.Facade.Exports), len(cfg.Facade.Aliases), len(cfg.Facade.InterfaceAssertions))
	fmt.Fprintf(&b, "  release       %s maps to %s under %s\n", cfg.Source.Refs.MinimumRelease, mapped, cfg.Release.Policy)
	fmt.Fprintf(&b, "  toolchain     %s\n", cfg.Determinism.Toolchain)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// runVersion prints the engine and Go runtime versions.
func runVersion(_ context.Context, env Env, args []string) error {
	fs := newFlagSet("version")
	if err := parseFlags(env, versionCommand(), fs, args); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(env.Stdout, "soapbox %s\n%s\n", Version, runtime.Version()); err != nil {
		return fmt.Errorf("write version: %w", err)
	}
	return nil
}

// runHelp prints general or command usage.
func runHelp(_ context.Context, env Env, args []string) error {
	if len(args) > 1 {
		return usagef("help takes at most one command name")
	}
	if len(args) == 0 {
		writeUsage(env.Stdout)
		return nil
	}
	for _, cmd := range commands() {
		if cmd.name != args[0] {
			continue
		}
		// Flags come from the same constructors the commands parse with, so help
		// can never drift from the accepted flags.
		var fs *flag.FlagSet
		switch cmd.name {
		case "doctor":
			fs, _ = doctorFlagSet()
		case "validate":
			fs, _ = validateFlagSet()
		case "plan":
			fs, _ = planFlagSet()
		case "generate":
			fs, _ = generateFlagSet()
		case "sync":
			fs, _ = syncFlagSet()
		case "setup":
			fs, _ = setupFlagSet()
		case "upgrade":
			fs, _ = upgradeFlagSet()
		default:
			fs = newFlagSet(cmd.name)
		}
		commandUsage(cmd, fs)(env.Stdout)
		return nil
	}
	return usagef("unknown command %q", args[0])
}

// writeUsage renders the top level usage message.
func writeUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString("soapbox transforms configured Kubernetes packages into an independent Go module.\n\n")
	b.WriteString("usage:\n  soapbox <command> [flags]\n\ncommands:\n")
	for _, cmd := range commands() {
		fmt.Fprintf(&b, "  %-9s %s\n", cmd.name, cmd.summary)
	}
	b.WriteString("\nrun \"soapbox help <command>\" for command flags.\n")
	fmt.Fprint(w, b.String())
}
