package gocli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/buildinfo"
)

// ErrTidyRequired reports a module whose go.mod or go.sum does not already match
// what tidying would produce. It is a verdict rather than a failure, so a caller
// that gates publication on an unchanged dependency set can distinguish it from
// a go command that broke.
var ErrTidyRequired = errors.New("go.mod or go.sum is not tidy")

// ErrToolchainMismatch reports a go command that is not the pinned release.
var ErrToolchainMismatch = errors.New("go toolchain is not the pinned release")

// ValidateToolchain fails unless the resolved go command is exactly the release
// the engine pins.
//
// GOTOOLCHAIN=local only stops the go command from downloading a different
// toolchain; it says nothing about which one is already installed. Generated
// formatting and module metadata have to be byte identical across machines, and
// gofmt output does change between releases, so the version actually running is
// a precondition rather than a detail. It is a separate call rather than a check
// inside every command because it costs a subprocess: run it once at preflight,
// the way gitcli's RequireMinimumVersion is run.
func (r *Runner) ValidateToolchain(ctx context.Context) error {
	out, err := r.run(ctx, "version")
	if err != nil {
		return fmt.Errorf("go version: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "version" {
		return fmt.Errorf("go version: unexpected output %q", strings.TrimSpace(out))
	}
	if fields[2] != buildinfo.Toolchain {
		return fmt.Errorf("go version: %s is not the pinned %s: %w", fields[2], buildinfo.Toolchain, ErrToolchainMismatch)
	}
	return nil
}

// Env reports the requested Go environment values.
//
// The JSON form is used rather than the line form because a value may itself
// contain a newline, which would silently shift every later value in a line
// delimited read.
func (r *Runner) Env(ctx context.Context, names ...string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, errors.New("go env: at least one variable name is required")
	}
	for _, name := range names {
		if err := validateEnvName(name); err != nil {
			return nil, fmt.Errorf("go env: %w", err)
		}
	}
	args, err := appendArguments([]string{"env", "-json"}, "variable name", names)
	if err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(out), &values); err != nil {
		return nil, fmt.Errorf("go env: %w", err)
	}
	// An unknown name is reported as an empty value rather than as an error, so
	// the caller's own list is what proves the response is complete.
	for _, name := range names {
		if _, ok := values[name]; !ok {
			return nil, fmt.Errorf("go env: %s is missing from the response, which reported %s",
				name, strings.Join(sortedNames(values), ", "))
		}
	}
	return values, nil
}

// validateEnvName rejects a variable name that is not one the go command could
// report, which keeps an option or a shell fragment from reaching the vector.
func validateEnvName(name string) error {
	if name == "" {
		return errors.New("variable name must not be empty")
	}
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return fmt.Errorf("variable name %q must be upper case letters, digits, and underscores", name)
		}
	}
	return nil
}

// loaderVariables are the Go location variables a package load must be told
// explicitly, in the order LoaderEnv reports them.
var loaderVariables = []string{"GOROOT", "GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR"}

// loaderOptional are the variables an empty value is an answer for rather than
// a gap.
//
// GOTMPDIR is the only one. The go command reports it empty when nothing set
// it, and empty means "use the system temporary directory" rather than "use an
// empty path", so writing the empty entry out would state a different
// instruction rather than the same one made explicit.
var loaderOptional = []string{"GOTMPDIR"}

// LoaderEnv returns the complete environment a go/packages load must run under.
//
// A package load runs the go command, so it meets exactly the problems this
// package exists to close, but it meets them in a process this package does not
// start: the loader builds its own subprocess and is handed an environment
// rather than a Runner. Anything missing from that environment is therefore not
// defaulted here but inherited from whoever started the engine, and what it
// decides is which code gets type checked. A public API generated against one
// module cache and a policy decided against another is not one result, so the
// environment is stated in full rather than grown from os.Environ.
//
// It is built from this runner's isolation and proxy and from the locations the
// go command itself resolves, so a load reads the same GOROOT, build cache, and
// module cache as every other command this runner runs. Two things are absent on
// purpose. The Env entries are left out because they are this package's
// credential channel and a load needs no credential of its own: the proxy URL
// already carries whatever a module fetch requires. Ambient Go policy is left
// out for the reason it is refused everywhere else here, and the fixed entries
// are appended last so an inherited GOFLAGS, a workspace file, or a toolchain
// switch cannot outrank them.
func (r *Runner) LoaderEnv(ctx context.Context) ([]string, error) {
	values, err := r.Env(ctx, loaderVariables...)
	if err != nil {
		return nil, fmt.Errorf("go loader environment: %w", err)
	}
	env := make([]string, 0, len(r.inherited)+len(r.isolation)+len(loaderVariables)+len(fixedEnv)+1)
	env = append(env, r.inherited...)
	env = append(env, r.isolation...)
	for _, name := range loaderVariables {
		switch value := values[name]; {
		case value != "":
			env = append(env, name+"="+value)
		case !slices.Contains(loaderOptional, name):
			// An empty required location is not a location. The go command
			// would resolve its own default from HOME, which is the ambient
			// state this environment exists to replace.
			return nil, fmt.Errorf("go loader environment: %s resolved to an empty path, so a load would fall back to an ambient location", name)
		}
	}
	env = append(env, fixedEnv...)
	if r.modCacheRW {
		env = append(env, "GOFLAGS=-modcacherw")
	}
	return append(env, proxyVariable+"="+r.proxy), nil
}

// ModuleError is the reason the go command could not resolve one module.
type ModuleError struct {
	Err string
}

// ModuleOrigin is the go command's record of where a module version came from
// in version control.
//
// It is the strongest available proof that a resolved version names the source
// commit it claims to: Hash is the revision the version was built from, which
// for a pseudo-version is the only thing tying the synthesised version string
// back to a real commit. Module.Time carries the same claim in a weaker form,
// because a commit date can be rewritten and a hash cannot.
//
// Only the fields the engine reads are declared, for the same reason as Module.
// The go command also reports TagPrefix, TagSum, and RepoSum, which describe how
// a resolution would be revalidated rather than where the code came from.
type ModuleOrigin struct {
	// VCS is the version control system, such as "git".
	VCS string
	// URL is the repository the version was resolved from.
	URL string
	// Subdir is the path within the repository that holds the module, empty
	// when the module sits at the repository root.
	Subdir string
	// Hash is the revision the version resolved to, such as a Git commit object
	// name.
	Hash string
	// Ref is the reference the version was found through, such as
	// refs/tags/v1.2.3.
	Ref string
}

// Module is one module as go list -m -json reports it. Only the fields the
// engine reads are declared; the rest of the go command's schema is ignored
// rather than mirrored, so a new field upstream is not a parse failure here.
type Module struct {
	Path      string
	Version   string
	Query     string
	Main      bool
	Indirect  bool
	Dir       string
	GoMod     string
	GoVersion string
	// Time is the commit time behind a pseudo-version, which is what proves a
	// resolved version really names the source commit it was derived from.
	Time *time.Time
	// Origin is where the version came from in version control, or nil when the
	// go command reported none.
	//
	// Nil is a normal answer rather than a failure: the main module has no
	// origin, and neither does a version whose source did not report one. The
	// pointer is what keeps that case distinguishable from an origin reported
	// with every field empty, so a caller can tell "not answered" from "answered
	// with nothing" instead of guessing from a zero value.
	Origin  *ModuleOrigin
	Replace *Module
	Error   *ModuleError
}

// ListModules resolves module queries in one subprocess.
//
// Batching is the point: resolving a staging module per Kubernetes release means
// dozens of queries, and the go command amortises proxy round trips across a
// single invocation.
//
// A query the go command cannot resolve is reported in that module's Error field
// rather than failing the batch, because one unresolvable module must not hide
// the answer for every other one. Callers have to check it.
//
// The result is not one record per query. A pattern such as "all" or a path
// ending in "..." expands to every module it matches, and a pattern that matches
// nothing yields none, so records are returned as the go command grouped them
// rather than zipped back onto the queries that produced them.
func (r *Runner) ListModules(ctx context.Context, queries ...string) ([]Module, error) {
	if len(queries) == 0 {
		return nil, errors.New("go list -m: at least one module query is required")
	}
	if err := r.requireModuleDir(); err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	args, err := appendArguments([]string{"list", "-m", "-json", "-e"}, "module query", queries)
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	modules, err := decodeJSONStream[Module](out)
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	return modules, nil
}

// ModuleWithVersions is one module as go list -m -versions -json reports it.
type ModuleWithVersions struct {
	Path     string
	Version  string
	Versions []string
	Error    *ModuleError
}

// ListModuleVersions queries the proxy for the available versions of a module,
// including retracted versions.
//
// The result includes the Versions field that go list -m -versions populates.
// Retracted versions are included (-retracted) because a retraction is still a
// release and a maintenance event: excluding them would undercount the cadence
// and approve a module whose real release frequency exceeds the ceiling.
func (r *Runner) ListModuleVersions(ctx context.Context, queries ...string) ([]ModuleWithVersions, error) {
	if len(queries) == 0 {
		return nil, errors.New("go list -m -versions: at least one module query is required")
	}
	if err := r.requireModuleDir(); err != nil {
		return nil, fmt.Errorf("go list -m -versions: %w", err)
	}
	args, err := appendArguments([]string{"list", "-m", "-versions", "-retracted", "-json", "-e"}, "module query", queries)
	if err != nil {
		return nil, fmt.Errorf("go list -m -versions: %w", err)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("go list -m -versions: %w", err)
	}
	modules, err := decodeJSONStream[ModuleWithVersions](out)
	if err != nil {
		return nil, fmt.Errorf("go list -m -versions: %w", err)
	}
	return modules, nil
}

type PackageError struct {
	ImportStack []string
	Pos         string
	Err         string
}

// Package is one package as go list -json reports it, reduced to the fields the
// engine reads.
type Package struct {
	Dir        string
	ImportPath string
	Name       string
	Standard   bool
	DepOnly    bool
	Module     *Module
	GoFiles    []string
	CgoFiles   []string
	Imports    []string
	Deps       []string
	Error      *PackageError
	DepsErrors []*PackageError
}

// PackageListOptions selects the packages one load covers.
type PackageListOptions struct {
	// Patterns are package patterns such as ./... or an import path. At least
	// one is required, because the go command's own default depends on the
	// working directory rather than on anything the caller stated.
	Patterns []string
	// Deps also reports every package reachable from the named ones, which is
	// what a closure over the transformed module needs.
	Deps bool
	// Test also reports the packages the test files import.
	Test bool
}

// ListPackages loads package metadata in one subprocess.
//
// Loading errors are requested rather than fatal, for the same reason as
// ListModules: a single unbuildable package in a large closure must be visible
// as that package's error instead of erasing the whole answer.
func (r *Runner) ListPackages(ctx context.Context, opts PackageListOptions) ([]Package, error) {
	if len(opts.Patterns) == 0 {
		return nil, errors.New("go list: at least one package pattern is required")
	}
	if err := r.requireModuleDir(); err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	args := []string{"list", "-json", "-e"}
	if opts.Deps {
		args = append(args, "-deps")
	}
	if opts.Test {
		args = append(args, "-test")
	}
	args, err := appendArguments(args, "package pattern", opts.Patterns)
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	packages, err := decodeJSONStream[Package](out)
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	return packages, nil
}

// ModuleVersion is one node of the module requirement graph. The synthetic go
// and toolchain nodes the go command emits appear here as themselves rather
// than being filtered out, because they are real requirements.
type ModuleVersion struct {
	Path string
	// Version is empty for the main module, which has none.
	Version string
}

// String renders the node in the go command's own path@version form.
func (m ModuleVersion) String() string {
	if m.Version == "" {
		return m.Path
	}
	return m.Path + "@" + m.Version
}

// ModuleEdge is one requirement: From requires To.
type ModuleEdge struct {
	From ModuleVersion
	To   ModuleVersion
}

// ModuleGraph reports the module requirement graph in the go command's order,
// which is deterministic for a given module.
func (r *Runner) ModuleGraph(ctx context.Context) ([]ModuleEdge, error) {
	if err := r.requireModuleDir(); err != nil {
		return nil, fmt.Errorf("go mod graph: %w", err)
	}
	out, err := r.run(ctx, "mod", "graph")
	if err != nil {
		return nil, fmt.Errorf("go mod graph: %w", err)
	}
	var edges []ModuleEdge
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		from, to, ok := strings.Cut(line, " ")
		if !ok || strings.ContainsRune(to, ' ') {
			return nil, fmt.Errorf("go mod graph: unexpected edge %q", line)
		}
		edges = append(edges, ModuleEdge{From: parseModuleVersion(from), To: parseModuleVersion(to)})
	}
	return edges, nil
}

// parseModuleVersion splits a path@version node. A module path cannot contain
// an at sign, so the first one is the separator.
func parseModuleVersion(node string) ModuleVersion {
	path, version, _ := strings.Cut(node, "@")
	return ModuleVersion{Path: path, Version: version}
}

// TidyOptions configures a tidy run.
type TidyOptions struct {
	// Diff reports whether tidying would change go.mod or go.sum without writing
	// either, which is how a run proves minimal version selection did not move a
	// pin the engine computed. Without it the files are rewritten in place.
	Diff bool
}

// TidyDiffError reports the change tidying would have made. It carries the diff
// because a run that stops on an unexpected dependency change is useless to an
// operator who cannot see what changed.
type TidyDiffError struct {
	Diff string
}

// Error names the condition without inlining the diff, which callers read from
// the field.
func (e *TidyDiffError) Error() string { return ErrTidyRequired.Error() }

// Unwrap exposes the sentinel so errors.Is keeps working.
func (e *TidyDiffError) Unwrap() error { return ErrTidyRequired }

// Tidy reconciles go.mod and go.sum with the module's imports.
//
// With Diff it writes nothing and returns a *TidyDiffError, which unwraps to
// ErrTidyRequired, when the files are not already what tidying would produce.
func (r *Runner) Tidy(ctx context.Context, opts TidyOptions) error {
	if err := r.requireModuleDir(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	args := []string{"mod", "tidy"}
	if opts.Diff {
		args = append(args, "-diff")
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		// The diff form reports a needed change with exit status 1 and prints the
		// diff, while a module it could not load also exits 1 but prints nothing
		// there. Without that second condition a broken module would be reported
		// as a dependency change, which is the one verdict a caller acts on.
		if opts.Diff && ExitCodeOf(err) == 1 && strings.TrimSpace(out) != "" {
			return fmt.Errorf("go mod tidy: %w", &TidyDiffError{Diff: out})
		}
		return fmt.Errorf("go mod tidy: %w", err)
	}
	return nil
}

// DownloadedModule is one module as go mod download -json reports it.
type DownloadedModule struct {
	Path     string
	Version  string
	Error    string
	Info     string
	GoMod    string
	Zip      string
	Dir      string
	Sum      string
	GoModSum string
}

// Download fetches modules into the module cache and reports where each one
// landed. With no queries it downloads the main module's requirements.
//
// A module that could not be downloaded is reported in its own Error field, and
// the go command still exits non-zero when that happens. That exit status is
// tolerated only when the response actually explains it: an exit with no failed
// record is a command that broke for some other reason and is returned as one.
//
// A cancelled or expired context is never absorbed that way. A run killed part
// way through emits records for the modules it had already finished, and some of
// those can carry their own errors, so the per module explanation would otherwise
// make a truncated run look like a completed one.
func (r *Runner) Download(ctx context.Context, queries ...string) ([]DownloadedModule, error) {
	if err := r.requireModuleDir(); err != nil {
		return nil, fmt.Errorf("go mod download: %w", err)
	}
	args, err := appendArguments([]string{"mod", "download", "-json"}, "module query", queries)
	if err != nil {
		return nil, fmt.Errorf("go mod download: %w", err)
	}
	out, runErr := r.run(ctx, args...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("go mod download: %w", ctxErr)
	}
	downloads, err := decodeJSONStream[DownloadedModule](out)
	if err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("go mod download: %w", runErr)
		}
		return nil, fmt.Errorf("go mod download: %w", err)
	}
	if runErr != nil && !slices.ContainsFunc(downloads, func(d DownloadedModule) bool { return d.Error != "" }) {
		return nil, fmt.Errorf("go mod download: %w", runErr)
	}
	return downloads, nil
}

// decodeJSONStream reads the concatenated JSON objects the go command emits.
//
// The output is a stream of values rather than an array, so it is decoded one
// value at a time. Trailing bytes that are not a value are an error rather than
// something to skip, because a truncated response must not read as a short one.
func decodeJSONStream[T any](out string) ([]T, error) {
	decoder := json.NewDecoder(strings.NewReader(out))
	var values []T
	for {
		var value T
		switch err := decoder.Decode(&value); {
		case errors.Is(err, io.EOF):
			return values, nil
		case err != nil:
			return nil, fmt.Errorf("decode response: %w", err)
		}
		values = append(values, value)
	}
}
