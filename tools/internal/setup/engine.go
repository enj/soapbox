package setup

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/enj/soapbox/tools/internal/buildinfo"
)

// EngineModulePath is the module a derived repository's nested tools module
// depends on.
const EngineModulePath = "github.com/enj/soapbox/tools"

// EngineTagPrefix is the tag prefix Go requires for a module in a subdirectory.
// The engine lives in tools/ of its repository, so release v1.2.3 of the module
// is the repository tag tools/v1.2.3.
const EngineTagPrefix = "tools/"

// toolsDirName is the subdirectory the nested tools module occupies in both the
// template and the derived repository.
const toolsDirName = "tools"

// enginePin is one immutable engine release, in both spellings.
type enginePin struct {
	// version is the canonical module version, such as v1.2.3.
	version string
	// tag is the repository tag that publishes it, such as tools/v1.2.3.
	tag string
}

// parseEnginePin resolves the engine release the shim pins.
//
// Only an exact published release is accepted. A query such as "latest", a
// branch name, or a pseudo-version would each make the derived repository build
// against whatever the proxy served at the moment of the build, and a repository
// whose engine can move underneath it cannot produce the byte-identical output
// the whole design rests on. A pseudo-version is refused despite naming one
// commit, because the point of the pin is that a human can read which release is
// running and find its notes.
func parseEnginePin(value string) (enginePin, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return enginePin{}, errors.New("an engine version is required, spelled either v1.2.3 or " + EngineTagPrefix + "v1.2.3")
	}
	version := strings.TrimPrefix(trimmed, EngineTagPrefix)
	switch {
	case !strings.HasPrefix(version, "v"):
		return enginePin{}, fmt.Errorf("engine version %q must name a released tag, spelled either v1.2.3 or %sv1.2.3", value, EngineTagPrefix)
	case !semver.IsValid(version):
		return enginePin{}, fmt.Errorf("engine version %q is not a semantic version, so it names no immutable release", value)
	case semver.Canonical(version) != version:
		// Canonicalisation is what refuses an incomplete version such as v1.4 and
		// what refuses build metadata, since canonical form drops it. Both would
		// otherwise reach the module system as a version that resolves to
		// something other than what was written.
		return enginePin{}, fmt.Errorf("engine version %q is not canonical, want %s", value, semver.Canonical(version))
	case module.IsPseudoVersion(version):
		return enginePin{}, fmt.Errorf("engine version %q is a pseudo-version, and the shim pins a published release so the running engine can be read off the go.mod", value)
	}
	// Check rejects a major version the module path does not carry a suffix for,
	// which is the one way a well formed version can still name a module that
	// cannot exist at this path.
	if err := module.Check(EngineModulePath, version); err != nil {
		return enginePin{}, fmt.Errorf("engine version %q: %w", value, err)
	}
	return enginePin{version: version, tag: EngineTagPrefix + version}, nil
}

// toolsModulePath is the module path of the derived repository's nested tools
// module.
//
// It is the root module's path with the directory appended, which is what the
// go command resolves for a nested module and what keeps the two from colliding.
// Nothing depends on it remotely: the shim is built from the checkout it lives
// in, so the path only has to be valid and distinct.
func toolsModulePath(rootModule string) string {
	return path.Join(rootModule, toolsDirName)
}

// composeRootGoMod renders the derived repository's root module.
//
// It carries no requirements. The generated library's dependencies are decided
// by an extraction from a specific upstream commit, and inventing them here
// would put a version in the module graph that no generation chose. The first
// generation writes them.
func composeRootGoMod(modulePath string) ([]byte, error) {
	return renderGoMod(modulePath, nil, nil)
}

// composeToolsGoMod renders the nested tools module.
//
// The shim imports only the engine, but Go's pruned module graph also requires
// the main module to state the modules needed to load that dependency. The
// pinned engine's requirements are therefore copied as indirect requirements.
// They remain behind the nested-module boundary: the generated library's root
// module never requires the engine or any of its tooling dependencies.
func composeToolsGoMod(rootModule string, pin enginePin, indirect []module.Version) ([]byte, error) {
	return renderGoMod(
		toolsModulePath(rootModule),
		[]module.Version{{Path: EngineModulePath, Version: pin.version}},
		indirect,
	)
}

// engineRequirements reads the graph roots the pinned engine publishes.
func engineRequirements(data []byte) ([]module.Version, error) {
	file, err := modfile.Parse("engine go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse pinned engine go.mod: %w", err)
	}
	if file.Module == nil || file.Module.Mod.Path != EngineModulePath {
		got := ""
		if file.Module != nil {
			got = file.Module.Mod.Path
		}
		return nil, fmt.Errorf("pinned engine go.mod declares module %q, want %q", got, EngineModulePath)
	}
	if len(file.Replace) > 0 || len(file.Exclude) > 0 {
		return nil, errors.New("pinned engine go.mod carries replace or exclude directives the derived shim cannot reproduce")
	}
	requirements := make([]module.Version, 0, len(file.Require))
	for _, requirement := range file.Require {
		if requirement.Mod.Path == EngineModulePath {
			return nil, errors.New("pinned engine go.mod requires its own module path")
		}
		requirements = append(requirements, requirement.Mod)
	}
	slices.SortFunc(requirements, func(a, b module.Version) int {
		if order := strings.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return strings.Compare(a.Version, b.Version)
	})
	return requirements, nil
}

// renderGoMod formats one go.mod through the module file parser, so the bytes
// written are the bytes the go command would settle on rather than this
// package's idea of the layout.
func renderGoMod(modulePath string, direct, indirect []module.Version) ([]byte, error) {
	if err := module.CheckPath(modulePath); err != nil {
		return nil, fmt.Errorf("module path %q: %w", modulePath, err)
	}
	file := new(modfile.File)
	if err := file.AddModuleStmt(modulePath); err != nil {
		return nil, fmt.Errorf("module directive: %w", err)
	}
	if err := file.AddGoStmt(buildinfo.GoDirective); err != nil {
		return nil, fmt.Errorf("go directive: %w", err)
	}
	// The toolchain is written because the pinned patch release is what makes
	// generated formatting and module metadata byte identical across machines,
	// and the go directive names a language version rather than a release.
	if err := file.AddToolchainStmt(buildinfo.Toolchain); err != nil {
		return nil, fmt.Errorf("toolchain directive: %w", err)
	}
	required := make([]*modfile.Require, 0, len(direct)+len(indirect))
	for _, version := range direct {
		required = append(required, &modfile.Require{Mod: version})
	}
	for _, version := range indirect {
		required = append(required, &modfile.Require{Mod: version, Indirect: true})
	}
	// The Go command keeps direct and indirect requirements in separate groups.
	// Matching that canonical layout makes a freshly generated shim tidy-clean
	// instead of asking its first build to rewrite tools/go.mod.
	file.SetRequireSeparateIndirect(required)
	file.Cleanup()

	data, err := file.Format()
	if err != nil {
		return nil, fmt.Errorf("format go.mod: %w", err)
	}
	return data, nil
}

// composeToolsMain renders the command shim.
//
// It is deliberately the smallest program that can run the engine: everything it
// does is turn an engine exit code into a process exit code and let a signal
// cancel the run. All behaviour lives behind the pinned engine release, so a
// derived repository upgrades by editing one version in one go.mod.
func composeToolsMain() []byte {
	return []byte(`// Command soapbox runs the pinned Soapbox engine for this repository.
//
// Generated by soapbox setup. The engine release this builds against is pinned
// in the go.mod beside it; nothing here changes when the engine does.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	soapbox "` + EngineModulePath + `"
)

func main() {
	os.Exit(run())
}

// run keeps the signal handler teardown ahead of the process exit.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return soapbox.Main(ctx, os.Args[1:])
}
`)
}

// engineSumNotice explains an absent tools/go.sum.
const engineSumNotice = "tools/go.sum was not written: module checksums cannot be computed from a checkout, " +
	"so run \"go mod download all\" inside tools/ once the pinned release is published, or pass the complete verified go.sum content to -engine-sum"

// composeEngineSum validates and normalises operator supplied checksums.
//
// The checksums cannot be derived here. A go.sum line is a hash of the module
// zip the proxy serves, which a local checkout of the engine's source does not
// determine, so the honest options are to write nothing or to write exactly what
// the operator verified. Composing plausible looking lines is not among them: a
// wrong hash fails every build, and a right-looking wrong hash is worse.
//
// What is checked is that the content is a well formed go.sum which covers the
// pinned release and both checksum forms of every module its go.mod requires. A
// shorter file is syntactically valid but cannot load the shim from a clean
// cache, which is the only environment a generated repository may assume.
func composeEngineSum(raw []byte, pin enginePin, requirements []module.Version) ([]byte, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, nil
	}
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("engine checksums: %q is not a go.sum line of module, version, and hash", line)
		}
		if !strings.HasPrefix(fields[2], "h1:") {
			return nil, fmt.Errorf("engine checksums: %q does not carry an h1 hash", line)
		}
		lines = append(lines, strings.Join(fields, " "))
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)

	covered := append([]module.Version{{Path: EngineModulePath, Version: pin.version}}, requirements...)
	for _, requirement := range covered {
		for _, suffix := range []string{"", "/go.mod"} {
			want := requirement.Path + " " + requirement.Version + suffix + " "
			if !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, want) }) {
				return nil, fmt.Errorf("engine checksums: no entry for %q, so the derived shim cannot load the pinned engine from a clean cache", strings.TrimSpace(want))
			}
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
