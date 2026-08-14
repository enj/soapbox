package gocli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/gocli"
)

func TestEnvReadsValues(t *testing.T) {
	cache := t.TempDir()
	runner := buildRunner(t, gocli.Options{
		Dir:       t.TempDir(),
		Proxy:     offline,
		Isolation: []string{"GOCACHE=" + cache},
	})

	values, err := runner.Env(t.Context(), "GOCACHE", "GOTOOLCHAIN")
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("got %d values, want 2", len(values))
	}
	if values["GOCACHE"] != cache {
		t.Fatalf("GOCACHE = %q, want %q", values["GOCACHE"], cache)
	}
}

func TestEnvRejectsHostileNames(t *testing.T) {
	runner := newRunner(t, t.TempDir(), offline)

	tests := []struct {
		name  string
		names []string
	}{
		{name: "no names"},
		{name: "empty name", names: []string{""}},
		{name: "option", names: []string{"-json"}},
		{name: "lower case", names: []string{"gocache"}},
		{name: "embedded space", names: []string{"GOCACHE GOPATH"}},
		{name: "null byte", names: []string{"GOCACHE\x00"}},
		{name: "one hostile name in a batch", names: []string{"GOCACHE", "--help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values, err := runner.Env(t.Context(), test.names...)
			if err == nil {
				t.Fatalf("hostile names were accepted and returned %v", values)
			}
			if values != nil {
				t.Fatalf("rejected read returned %v", values)
			}
		})
	}
}

func TestListModules(t *testing.T) {
	runner, dir := newModule(t, testGoMod, "package z\n", offline)

	modules, err := runner.ListModules(t.Context(), "example.com/z")
	if err != nil {
		t.Fatalf("list modules: %v", err)
	}
	if len(modules) != 1 {
		t.Fatalf("got %d modules, want 1", len(modules))
	}
	main := modules[0]
	if !main.Main || main.Path != "example.com/z" {
		t.Fatalf("module = %+v, want the main module", main)
	}
	if main.Dir != dir {
		t.Fatalf("module dir = %q, want %q", main.Dir, dir)
	}
	if main.GoVersion != "1.26.0" {
		t.Fatalf("go version = %q, want 1.26.0", main.GoVersion)
	}
	if main.Error != nil {
		t.Fatalf("main module reported an error: %s", main.Error.Err)
	}
}

// TestListModulesReportsPerQueryErrors proves one unresolvable query does not
// erase the answer for the rest of the batch, which is the property that makes
// batching usable at all.
func TestListModulesReportsPerQueryErrors(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	modules, err := runner.ListModules(t.Context(), "example.com/z", "example.com/absent@v1.0.0")
	if err != nil {
		t.Fatalf("list modules: %v", err)
	}
	if len(modules) != 2 {
		t.Fatalf("got %d modules, want 2", len(modules))
	}
	if modules[0].Error != nil {
		t.Fatalf("main module reported an error: %s", modules[0].Error.Err)
	}
	if modules[1].Error == nil {
		t.Fatal("an unresolvable module reported no error")
	}
	if modules[1].Path != "example.com/absent" {
		t.Fatalf("failed module path = %q", modules[1].Path)
	}
}

func TestListModulesRejectsHostileQueries(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	tests := []struct {
		name    string
		queries []string
	}{
		{name: "no queries"},
		{name: "empty query", queries: []string{""}},
		{name: "option", queries: []string{"-mod=mod"}},
		{name: "null byte", queries: []string{"example.com/z\x00"}},
		{name: "newline", queries: []string{"example.com/z\nexample.com/other"}},
		{name: "one hostile query in a batch", queries: []string{"example.com/z", "-u"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modules, err := runner.ListModules(t.Context(), test.queries...)
			if err == nil {
				t.Fatalf("hostile queries were accepted and returned %d modules", len(modules))
			}
			if modules != nil {
				t.Fatalf("rejected read returned %d modules", len(modules))
			}
		})
	}
}

func TestListPackages(t *testing.T) {
	runner, dir := newModule(t, testGoMod, "package z\n\nimport _ \"strings\"\n", offline)

	own, err := runner.ListPackages(t.Context(), gocli.PackageListOptions{Patterns: []string{"./..."}})
	if err != nil {
		t.Fatalf("list packages: %v", err)
	}
	if len(own) != 1 {
		t.Fatalf("got %d packages, want 1", len(own))
	}
	if own[0].ImportPath != "example.com/z" || own[0].Dir != dir {
		t.Fatalf("package = %+v, want the module's own package", own[0])
	}
	if own[0].Module == nil || !own[0].Module.Main {
		t.Fatalf("package module = %+v, want the main module", own[0].Module)
	}
	if len(own[0].GoFiles) != 1 || own[0].GoFiles[0] != "z.go" {
		t.Fatalf("go files = %v, want z.go", own[0].GoFiles)
	}

	withDeps, err := runner.ListPackages(t.Context(), gocli.PackageListOptions{
		Patterns: []string{"./..."},
		Deps:     true,
	})
	if err != nil {
		t.Fatalf("list packages with deps: %v", err)
	}
	if len(withDeps) <= len(own) {
		t.Fatalf("deps walk returned %d packages, want more than %d", len(withDeps), len(own))
	}
	var found bool
	for _, pkg := range withDeps {
		if pkg.ImportPath != "strings" {
			continue
		}
		found = true
		if !pkg.Standard || !pkg.DepOnly {
			t.Fatalf("strings package = %+v, want a standard dependency", pkg)
		}
	}
	if !found {
		t.Fatal("the deps walk did not reach the imported standard package")
	}
}

func TestListPackagesRejectsHostilePatterns(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	tests := []struct {
		name string
		opts gocli.PackageListOptions
	}{
		{name: "no patterns", opts: gocli.PackageListOptions{}},
		{name: "empty pattern", opts: gocli.PackageListOptions{Patterns: []string{""}}},
		{name: "option", opts: gocli.PackageListOptions{Patterns: []string{"-export"}}},
		{name: "null byte", opts: gocli.PackageListOptions{Patterns: []string{"./...\x00"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packages, err := runner.ListPackages(t.Context(), test.opts)
			if err == nil {
				t.Fatalf("hostile patterns were accepted and returned %d packages", len(packages))
			}
			if packages != nil {
				t.Fatalf("rejected read returned %d packages", len(packages))
			}
		})
	}
}

func TestModuleGraph(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	edges, err := runner.ModuleGraph(t.Context())
	if err != nil {
		t.Fatalf("module graph: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("module graph is empty")
	}
	root := edges[0]
	if root.From.Path != "example.com/z" {
		t.Fatalf("first edge starts at %q, want the main module", root.From.Path)
	}
	// The main module has no version, which is exactly why a node is a pair
	// rather than a string a caller has to split again.
	if root.From.Version != "" {
		t.Fatalf("main module version = %q, want none", root.From.Version)
	}
	if root.From.String() != "example.com/z" {
		t.Fatalf("main module renders as %q", root.From.String())
	}
	if root.To.Version == "" {
		t.Fatalf("required node %q has no version", root.To.Path)
	}
	if got := root.To.String(); got != root.To.Path+"@"+root.To.Version {
		t.Fatalf("required node renders as %q", got)
	}
}

func TestTidy(t *testing.T) {
	runner, dir := newModule(t, testGoMod, "package z\n\nimport _ \"strings\"\n", offline)
	goSum := filepath.Join(dir, "go.sum")

	if err := runner.Tidy(t.Context(), gocli.TidyOptions{Diff: true}); err != nil {
		t.Fatalf("a tidy module was reported as untidy: %v", err)
	}

	// A checksum for a module nothing requires is exactly the drift the diff
	// mode exists to catch, and it needs no network to detect.
	stale := "example.com/absent v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"
	writeFile(t, goSum, stale)

	err := runner.Tidy(t.Context(), gocli.TidyOptions{Diff: true})
	if !errors.Is(err, gocli.ErrTidyRequired) {
		t.Fatalf("error = %v, want %v", err, gocli.ErrTidyRequired)
	}
	var diffErr *gocli.TidyDiffError
	if !errors.As(err, &diffErr) {
		t.Fatalf("error %v does not carry the diff", err)
	}
	if !strings.Contains(diffErr.Diff, "go.sum") {
		t.Fatalf("diff %q does not mention go.sum", diffErr.Diff)
	}
	// The diff form is a question, not a change.
	if got := readFile(t, goSum); got != stale {
		t.Fatalf("the diff run rewrote go.sum to %q", got)
	}

	if err := runner.Tidy(t.Context(), gocli.TidyOptions{}); err != nil {
		t.Fatalf("tidy: %v", err)
	}
	if got := readFile(t, goSum); got != "" {
		t.Fatalf("tidy left %q in go.sum", got)
	}
}

// TestTidyDistinguishesDriftFromFailure proves a module the go command cannot
// load is not reported as a dependency change. Both exit with status 1, and only
// one of them is a verdict a caller should act on.
func TestTidyDistinguishesDriftFromFailure(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n\nimport _ \"example.com/absent/pkg\"\n", offline)

	err := runner.Tidy(t.Context(), gocli.TidyOptions{Diff: true})
	if err == nil {
		t.Fatal("expected a failure for an unresolvable import")
	}
	if errors.Is(err, gocli.ErrTidyRequired) {
		t.Fatalf("an unloadable module was reported as untidy: %v", err)
	}
	if gocli.ExitCodeOf(err) != 1 {
		t.Fatalf("exit code = %d, want 1", gocli.ExitCodeOf(err))
	}
}

func TestDownload(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	// A module with no requirements downloads nothing, which is a valid answer
	// rather than a failure.
	downloads, err := runner.Download(t.Context())
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if len(downloads) != 0 {
		t.Fatalf("got %d downloads, want none", len(downloads))
	}

	failed, err := runner.Download(t.Context(), "example.com/absent@v1.0.0")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("got %d downloads, want 1", len(failed))
	}
	if failed[0].Error == "" {
		t.Fatal("an unreachable module reported no error")
	}
	if failed[0].Path != "example.com/absent" {
		t.Fatalf("failed download path = %q", failed[0].Path)
	}
}

func TestDownloadRejectsHostileQueries(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	for _, query := range []string{"", "-x", "example.com/z\x00", "example.com/z\n-u"} {
		t.Run(query, func(t *testing.T) {
			downloads, err := runner.Download(t.Context(), query)
			if err == nil {
				t.Fatalf("hostile query was accepted and returned %d downloads", len(downloads))
			}
		})
	}
}

// readFile reports the contents of a fixture file.
func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

// TestValidateToolchain proves the pinned release is checked rather than
// assumed. GOTOOLCHAIN=local stops the go command from fetching a different
// toolchain; it says nothing about which one is already installed, and gofmt
// output changes between releases.
func TestValidateToolchain(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)
	if err := runner.ValidateToolchain(t.Context()); err != nil {
		t.Fatalf("the running toolchain is not the pinned %s: %v", buildinfo.Toolchain, err)
	}

	// A stand-in reporting a different release has to be refused, or the check
	// would pass on any machine that merely has some go on PATH.
	fake := buildVersionStandIn(t, "go version go1.0.0 linux/arm64\n")
	mismatched, err := gocli.New(t.Context(), gocli.Options{
		Binary:  fake,
		Dir:     t.TempDir(),
		Inherit: []string{"PATH"},
		Proxy:   offline,
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	err = mismatched.ValidateToolchain(t.Context())
	if !errors.Is(err, gocli.ErrToolchainMismatch) {
		t.Fatalf("error = %v, want %v", err, gocli.ErrToolchainMismatch)
	}
	if !strings.Contains(err.Error(), buildinfo.Toolchain) {
		t.Fatalf("error %q does not name the pinned toolchain", err)
	}

	// Output that is not a version line at all is a broken command, not a
	// mismatch, and must not be reported as one.
	garbled := buildVersionStandIn(t, "not a version line\n")
	broken, err := gocli.New(t.Context(), gocli.Options{
		Binary:  garbled,
		Dir:     t.TempDir(),
		Inherit: []string{"PATH"},
		Proxy:   offline,
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	err = broken.ValidateToolchain(t.Context())
	if err == nil {
		t.Fatal("unparseable version output was accepted")
	}
	if errors.Is(err, gocli.ErrToolchainMismatch) {
		t.Fatalf("a broken command was reported as a version mismatch: %v", err)
	}
}

// TestListModulesReportsPatternExpansions covers the shapes where the record
// count is not the query count: a pattern expands to many modules and a
// non-matching one expands to none.
func TestListModulesReportsPatternExpansions(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	tests := []struct {
		name    string
		queries []string
		check   func(*testing.T, []gocli.Module)
	}{
		{
			name:    "all expands to every module in the graph",
			queries: []string{"all"},
			check: func(t *testing.T, modules []gocli.Module) {
				if len(modules) == 0 {
					t.Fatal("all matched no modules")
				}
				if !modules[0].Main {
					t.Fatalf("all did not start at the main module: %+v", modules[0])
				}
			},
		},
		{
			name:    "an ellipsis pattern that matches nothing yields nothing",
			queries: []string{"example.com/absent/..."},
			check: func(t *testing.T, modules []gocli.Module) {
				if len(modules) != 0 {
					t.Fatalf("got %d modules, want none", len(modules))
				}
			},
		},
		{
			name:    "an exact query yields one",
			queries: []string{"example.com/z"},
			check: func(t *testing.T, modules []gocli.Module) {
				if len(modules) != 1 || modules[0].Path != "example.com/z" {
					t.Fatalf("modules = %+v, want the main module", modules)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modules, err := runner.ListModules(t.Context(), test.queries...)
			if err != nil {
				t.Fatalf("list modules: %v", err)
			}
			test.check(t, modules)
		})
	}
}

// TestListModulesReportsOrigin covers the version control identity behind a
// resolved version, which is what proves a pseudo-version names the commit it
// claims to rather than merely carrying a plausible date.
//
// The go command reports an origin only when the version was resolved from a
// repository, so a caller has three shapes to tell apart: an origin with
// content, an origin reported with nothing in it, and no origin at all. The last
// two are why Origin is a pointer, and a value type would collapse them into the
// same zero struct.
func TestListModulesReportsOrigin(t *testing.T) {
	t.Run("the reported shapes stay distinguishable", func(t *testing.T) {
		// The trailing three keys are deliberately fields the engine does not
		// declare, so this also exercises the claim that an unread field of the
		// go command's schema is ignored rather than a parse failure.
		const response = `{
	"Path": "example.com/tagged",
	"Version": "v1.2.3",
	"Time": "2026-01-02T03:04:05Z",
	"Origin": {
		"VCS": "git",
		"URL": "https://example.com/repo",
		"Subdir": "staging/src/tagged",
		"Hash": "0123456789abcdef0123456789abcdef01234567",
		"Ref": "refs/tags/v1.2.3",
		"TagPrefix": "staging/src/tagged/",
		"TagSum": "t1:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
		"RepoSum": "r1:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	}
}
{
	"Path": "example.com/empty",
	"Version": "v0.1.0",
	"Origin": {}
}
{
	"Path": "example.com/none",
	"Version": "v0.2.0"
}
`
		standIn := buildStandInSource(t,
			"package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Print("+strconv.Quote(response)+")\n}\n")
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), testGoMod)
		runner := buildRunner(t, gocli.Options{Binary: standIn, Dir: dir, Proxy: offline})

		modules, err := runner.ListModules(t.Context(),
			"example.com/tagged", "example.com/empty", "example.com/none")
		if err != nil {
			t.Fatalf("list modules: %v", err)
		}
		if len(modules) != 3 {
			t.Fatalf("got %d modules, want 3", len(modules))
		}

		tagged := modules[0]
		want := gocli.ModuleOrigin{
			VCS:    "git",
			URL:    "https://example.com/repo",
			Subdir: "staging/src/tagged",
			Hash:   "0123456789abcdef0123456789abcdef01234567",
			Ref:    "refs/tags/v1.2.3",
		}
		if tagged.Origin == nil {
			t.Fatal("the module resolved from a tag reported no origin")
		}
		// Comparing the whole value rather than field by field is what shows the
		// undeclared keys did not land somewhere they were not wanted.
		if *tagged.Origin != want {
			t.Fatalf("origin = %+v, want %+v", *tagged.Origin, want)
		}
		if tagged.Time == nil {
			t.Fatal("the record carrying an origin lost its commit time")
		}

		switch empty := modules[1].Origin; {
		case empty == nil:
			t.Fatal("an origin reported as empty decoded as no origin at all")
		case *empty != gocli.ModuleOrigin{}:
			t.Fatalf("origin = %+v, want the zero value", *empty)
		}

		if modules[2].Origin != nil {
			t.Fatalf("a module with no origin reported %+v", *modules[2].Origin)
		}
	})

	t.Run("the main module has no origin", func(t *testing.T) {
		runner, _ := newModule(t, testGoMod, "package z\n", offline)

		modules, err := runner.ListModules(t.Context(), "example.com/z")
		if err != nil {
			t.Fatalf("list modules: %v", err)
		}
		if len(modules) != 1 || !modules[0].Main {
			t.Fatalf("modules = %+v, want the main module", modules)
		}
		// The main module is not resolved from anywhere, so nil here is the go
		// command's real answer rather than a gap in the decoding.
		if modules[0].Origin != nil {
			t.Fatalf("the main module reported an origin %+v", *modules[0].Origin)
		}
	})
}

// TestDownloadReportsCancellation proves a cancelled run is never reported as a
// completed one.
//
// go mod download -json emits a record per module and exits non-zero when any of
// them failed, so a run killed part way through can leave behind records that
// carry their own errors. Treating that as the explanation for the exit status
// would turn a truncated run into a successful answer.
func TestDownloadReportsCancellation(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	downloads, err := runner.Download(ctx, "example.com/absent@v1.0.0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if downloads != nil {
		t.Fatalf("a cancelled run returned %d records", len(downloads))
	}
}

// TestOutputLimitIsEnforced proves a command cannot decide how much memory the
// engine spends. A module the engine does not control chooses how much its go
// command prints.
func TestOutputLimitIsEnforced(t *testing.T) {
	runner := buildRunner(t, gocli.Options{
		Dir:         t.TempDir(),
		Proxy:       offline,
		OutputLimit: 8,
	})
	values, err := runner.Env(t.Context(), "GOCACHE", "GOMODCACHE", "GOPATH")
	if err == nil {
		t.Fatalf("an oversized response was accepted: %v", values)
	}
	if !strings.Contains(err.Error(), "past the 8 byte limit") {
		t.Fatalf("error %q does not report the limit", err)
	}

	if _, err := gocli.New(t.Context(), gocli.Options{
		Dir:         t.TempDir(),
		Inherit:     []string{"PATH"},
		OutputLimit: -1,
	}); err == nil {
		t.Fatal("a negative output limit was accepted")
	}
}

// buildVersionStandIn compiles a stand-in for the go command that prints the
// given version line and nothing else.
func buildVersionStandIn(t *testing.T, line string) string {
	t.Helper()
	return buildStandInSource(t, "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Print("+strconv.Quote(line)+")\n}\n")
}

func TestListModuleVersionsDecodesVersionList(t *testing.T) {
	// A stand-in emits deterministic JSON containing multiple versions
	// including a retracted one, plus unknown fields the struct deliberately
	// does not declare. This proves the decoder populates Versions (including
	// retracted) and ignores extra keys rather than failing.
	response := `{
	"Path": "example.com/helpers",
	"Version": "v0.36.1",
	"Versions": ["v0.35.0", "v0.35.1", "v0.36.0", "v0.36.0-retracted", "v0.36.1"],
	"Retracted": ["v0.36.0-retracted is broken"],
	"Update": {"Path": "example.com/helpers", "Version": "v0.37.0"},
	"GoVersion": "1.26.0"
}
`
	standIn := buildStandInSource(t,
		"package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Print("+strconv.Quote(response)+")\n}\n")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), testGoMod)
	runner := buildRunner(t, gocli.Options{Binary: standIn, Dir: dir, Proxy: offline})

	results, err := runner.ListModuleVersions(t.Context(), "example.com/helpers")
	if err != nil {
		t.Fatalf("list module versions: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	m := results[0]
	if m.Path != "example.com/helpers" {
		t.Errorf("path = %q, want example.com/helpers", m.Path)
	}
	if m.Version != "v0.36.1" {
		t.Errorf("version = %q, want v0.36.1", m.Version)
	}
	wantVersions := []string{"v0.35.0", "v0.35.1", "v0.36.0", "v0.36.0-retracted", "v0.36.1"}
	if !slices.Equal(m.Versions, wantVersions) {
		t.Errorf("versions = %v, want %v", m.Versions, wantVersions)
	}
	if m.Error != nil {
		t.Errorf("unexpected error: %s", m.Error.Err)
	}
}

func TestListModuleVersionsReportsErrors(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	// An absent module with GOPROXY=off should report a per-module error.
	results, err := runner.ListModuleVersions(t.Context(), "example.com/absent@v1.0.0")
	if err != nil {
		t.Fatalf("list module versions: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("got 0 results, want at least 1")
	}
	hasError := false
	for _, r := range results {
		if r.Error != nil {
			hasError = true
		}
	}
	if !hasError {
		t.Error("absent module with offline proxy reported no error")
	}
}

func TestListModuleVersionsRejectsEmptyQueries(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	_, err := runner.ListModuleVersions(t.Context())
	if err == nil {
		t.Fatal("empty query list was accepted")
	}
}

func TestListModuleVersionsRejectsHostileQueries(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)

	for _, query := range []string{"-flag", "--option=value"} {
		_, err := runner.ListModuleVersions(t.Context(), query)
		if err == nil {
			t.Errorf("hostile query %q was accepted", query)
		}
	}
}

func TestModCacheRWMakesWritableCache(t *testing.T) {
	runner, err := gocli.New(t.Context(), isolatedOptions(t, gocli.Options{
		Dir:        t.TempDir(),
		ModCacheRW: true,
	}))
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	// The GOFLAGS should contain -modcacherw.
	values, err := runner.Env(t.Context(), "GOFLAGS")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if !strings.Contains(values["GOFLAGS"], "-modcacherw") {
		t.Errorf("GOFLAGS = %q, want it to contain -modcacherw", values["GOFLAGS"])
	}
}

func TestModCacheRWDefaultOff(t *testing.T) {
	runner, err := gocli.New(t.Context(), isolatedOptions(t, gocli.Options{
		Dir: t.TempDir(),
	}))
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	values, err := runner.Env(t.Context(), "GOFLAGS")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if values["GOFLAGS"] != "" {
		t.Errorf("GOFLAGS = %q, want empty when ModCacheRW is not set", values["GOFLAGS"])
	}
}
