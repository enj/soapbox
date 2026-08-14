package modgen

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
)

// ErrPinFloated reports a requirement the go command resolved above the version
// the engine pinned.
var ErrPinFloated = errors.New("a pinned requirement was raised by minimal version selection")

// ErrModuleDrift reports a generated module the go command changed in a way that
// is not a requirement version.
var ErrModuleDrift = errors.New("the generated module file drifted")

// Report is what one verification pass observed.
//
// It is returned rather than acted on, because this package produces the
// provisional module: dependency policy, the facade, and publication all run
// later and all read this. Nothing here writes to a destination repository.
type Report struct {
	// GoMod is the module file as it stands after tidying.
	GoMod []byte
	// GoSum is the checksum file after tidying, empty when the module needs no
	// checksums because it requires nothing outside the standard library.
	GoSum []byte
	// Kept lists the requirements that survived tidying, sorted by path, each
	// carrying the directness the go command settled on rather than the one the
	// source module had.
	Kept []gomodmap.Requirement
	// Added lists transitive requirements introduced by an explicitly allowed
	// compatibility re-tidy, sorted by path.
	Added []gomodmap.Requirement
	// Dropped lists the module paths tidying removed because nothing in the
	// extracted sources imports them, sorted. A large Dropped set is the normal
	// outcome of extracting a few packages out of Kubernetes rather than a
	// problem.
	Dropped []string
	// Reclassified lists the requirements tidying kept at the pinned version but
	// marked differently than the source module did, sorted by path.
	//
	// It is reported rather than refused because the extracted module is a
	// subset. A module the source imports from a package that was not extracted
	// becomes indirect here, and one the source only reached through a dependency
	// becomes direct when an extracted package imports it. Both are the correct
	// answer for the module being generated, and neither changes which code is
	// built, so what matters is that the change is stated instead of being
	// absorbed by copying the source's marking into the report.
	Reclassified []Reclassification
}

// Reclassification is one requirement whose directness the go command changed.
type Reclassification struct {
	// Path is the module path.
	Path string
	// Indirect is what tidying decided, and therefore what the generated module
	// carries. What the source module said is its negation, so recording both
	// would only give the two a way to disagree.
	Indirect bool
}

// VerifyOptions describes one verification pass.
type VerifyOptions struct {
	// Dir is the scratch module directory. It must already hold the extracted Go
	// sources and must hold neither a go.mod nor a go.sum, because this pass is
	// what decides what those files say and what removes them again if it fails.
	Dir string
	// GoMod is the generated module file to install.
	GoMod []byte
	// AllowAdditions permits tidy to add transitive requirements after a
	// compatibility transform deliberately removed a staging module whose go.mod
	// had supplied them. Added versions are recorded rather than hidden.
	AllowAdditions bool
}

// Verify installs the generated go.mod in a scratch module, tidies it, and
// reparses the result.
//
// Tidying is the point of the pass. Writing a go.mod with exact pins proves only
// that the engine can write exact pins; it says nothing about whether the go
// command agrees that those versions can build the extracted code together.
// Minimal version selection resolves a module to the highest version any
// requirement asks for, so a dependency that itself requires a newer release of
// something the engine pinned will raise that pin, and the raised version is
// then what a consumer would actually build against. That is refused here rather
// than published, because the operator approved the pin and not its successor.
//
// Everything else the go command could change is compared too. A replace
// directive appearing in a generated module would mean the published module
// resolves to code from somewhere the module path does not name, and a changed
// go or godebug directive would mean the extracted code is compiled under
// different semantics than upstream compiled it under.
//
// The scratch directory is left exactly as it was found unless the pass
// succeeds. Its precondition is that no go.mod is there yet, so a failed pass
// that left one behind would report a reused scratch module on the retry and
// would leave a module file that no report explains.
func Verify(ctx context.Context, runner *gocli.Runner, opts VerifyOptions) (report *Report, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}
	if !filepath.IsAbs(opts.Dir) {
		return nil, fmt.Errorf("verify generated module: directory %q must be absolute", opts.Dir)
	}
	// The two ways a scratch directory can be unusable are reported separately.
	// Folding them together would hand a nil error to %w for the path that exists
	// and is not a directory, which renders as a formatting error and wraps
	// nothing, so the one case an operator can act on would arrive unreadable.
	info, err := os.Stat(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("verify generated module: directory %q is not usable: %w", opts.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("verify generated module: %q is not a directory", opts.Dir)
	}
	modPath := filepath.Join(opts.Dir, goModName)
	// Every name this pass writes has to be absent first, not just go.mod. A
	// go.sum the caller left behind would be read back into the report as though
	// tidying had produced it, so a checksum this pass never computed would reach
	// publication, and the cleanup below would delete a file the pass did not
	// create. Refusing the directory outright is the only answer that neither
	// publishes nor destroys someone else's state.
	for _, name := range generatedNames {
		path := filepath.Join(opts.Dir, name)
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("verify generated module: %s already exists, the scratch module must be generated rather than reused", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("verify generated module: %w", err)
		}
	}

	// The runner is pointed at the scratch module rather than trusted to already
	// be there. A runner built for some other directory would tidy whichever
	// module contains it, which for an engine run started inside a checkout is
	// the engine's own module, and the report would then describe a module this
	// pass never generated.
	scoped, err := runner.WithDir(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}

	intended, err := modfile.Parse("go.mod", opts.GoMod, nil)
	if err != nil {
		return nil, fmt.Errorf("verify generated module: generated go.mod: %w", err)
	}
	if err := checkToolchain(intended); err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}
	if err := os.WriteFile(modPath, opts.GoMod, 0o600); err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}
	// From here on the directory holds files this pass created, so every failure
	// path takes them back out. Removal errors are joined rather than swallowed:
	// a scratch module that could not be cleaned up is the state the next pass
	// will refuse, and hiding that behind the original failure would make the
	// refusal unexplainable.
	defer func() {
		if err != nil {
			err = errors.Join(err, removeGenerated(opts.Dir))
		}
	}()

	if err := scoped.Tidy(ctx, gocli.TidyOptions{}); err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}

	// The path is the scratch module this function just wrote, under a directory
	// the caller supplied and this function validated, so it is the file the pass
	// is about rather than one an input named.
	tidied, err := os.ReadFile(modPath) //nolint:gosec // modPath is the scratch go.mod written above
	if err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}
	actual, err := modfile.Parse("go.mod", tidied, nil)
	if err != nil {
		return nil, fmt.Errorf("verify generated module: tidied go.mod: %w", err)
	}
	report, err = compare(intended, actual, opts.AllowAdditions)
	if err != nil {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}
	if opts.AllowAdditions {
		if err := scoped.Tidy(ctx, gocli.TidyOptions{Diff: true}); err != nil {
			return nil, fmt.Errorf("verify generated module: compatibility tidy is not stable: %w", err)
		}
	}

	sum, err := os.ReadFile(filepath.Join(opts.Dir, goSumName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("verify generated module: %w", err)
	}
	report.GoMod = tidied
	report.GoSum = sum
	return report, nil
}

// checkToolchain refuses a module file that names a toolchain other than the
// engine's pin.
//
// This pass is handed bytes rather than the options that produced them, so it is
// where a module file that did not come from Generate is caught. It is tidied by
// the engine's own go command and the tidied bytes are what a later step
// publishes, so a file naming some other release would be formatted and resolved
// by one toolchain while claiming another.
//
// An absent directive is correct rather than missing. The go command drops a
// toolchain the go directive already implies, so a module whose go directive is
// itself the pinned release names it without carrying a directive at all.
//
// Which release is actually running is a different question and deliberately not
// asked here: it costs a subprocess and is the same answer for every module in a
// run, so gocli.ValidateToolchain answers it once at preflight.
func checkToolchain(file *modfile.File) error {
	goVersion := goVersionOf(file)
	if goVersion == "" {
		return errors.New("generated go.mod: a go directive is required")
	}
	switch name := toolchainOf(file); {
	case name == "":
		if !toolchainIsImplied(buildinfo.Toolchain, goVersion) {
			return fmt.Errorf("generated go.mod: no toolchain directive, and the go directive %s does not imply the engine's pinned %s", goVersion, buildinfo.Toolchain)
		}
	case name != buildinfo.Toolchain:
		return fmt.Errorf("generated go.mod: toolchain %s is not the engine's pinned %s", name, buildinfo.Toolchain)
	}
	return nil
}

// The names one verification pass creates in the scratch module.
const (
	goModName = "go.mod"
	goSumName = "go.sum"
)

// generatedNames lists them once. The precondition that refuses a reused
// directory and the cleanup that empties a failed one both read this, so a name
// that is written but not refused, or removed but never written, cannot appear.
var generatedNames = []string{goModName, goSumName}

// removeGenerated takes back the files one verification pass wrote.
//
// A file that is not there is not a failure: go.sum is absent whenever the
// module needs no checksums, and the pass can fail before tidying has written
// either name.
func removeGenerated(dir string) error {
	var errs []error
	for _, name := range generatedNames {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// compare checks the tidied module against the generated one.
func compare(intended, actual *modfile.File, allowAdditions bool) (*Report, error) {
	if err := compareDirectives(intended, actual); err != nil {
		return nil, err
	}

	pinned := make(map[string]*modfile.Require, len(intended.Require))
	for _, require := range intended.Require {
		pinned[require.Mod.Path] = require
	}

	report := &Report{}
	surviving := make(map[string]bool, len(actual.Require))
	var floated, added []string
	for _, require := range actual.Require {
		modulePath, version := require.Mod.Path, require.Mod.Version
		surviving[modulePath] = true
		want, wasPinned := pinned[modulePath]
		switch {
		case !wasPinned:
			requirement := gomodmap.Requirement{Path: modulePath, Version: version, Indirect: require.Indirect}
			if allowAdditions {
				report.Kept = append(report.Kept, requirement)
				report.Added = append(report.Added, requirement)
				continue
			}
			// Ordinarily the source module lists every module its build resolves.
			// A compatibility transform is the explicit exception handled above.
			added = append(added, fmt.Sprintf("%s %s", modulePath, version))
			continue
		case version != want.Mod.Version:
			floated = append(floated, fmt.Sprintf("%s %s raised to %s", modulePath, want.Mod.Version, version))
			continue
		}
		// The directness the go command settled on is the one recorded, because
		// it is the one the generated module now carries. A difference from the
		// source module's marking is reported alongside it rather than corrected
		// in either direction: see Report.Reclassified for why a subset of
		// Kubernetes is expected to reach a different answer.
		if require.Indirect != want.Indirect {
			report.Reclassified = append(report.Reclassified, Reclassification{
				Path:     modulePath,
				Indirect: require.Indirect,
			})
		}
		report.Kept = append(report.Kept, gomodmap.Requirement{
			Path:     modulePath,
			Version:  version,
			Indirect: require.Indirect,
		})
	}
	for modulePath := range pinned {
		if !surviving[modulePath] {
			report.Dropped = append(report.Dropped, modulePath)
		}
	}

	slices.Sort(floated)
	slices.Sort(added)
	slices.Sort(report.Dropped)
	slices.SortFunc(report.Kept, func(a, b gomodmap.Requirement) int { return cmp.Compare(a.Path, b.Path) })
	slices.SortFunc(report.Added, func(a, b gomodmap.Requirement) int { return cmp.Compare(a.Path, b.Path) })
	slices.SortFunc(report.Reclassified, func(a, b Reclassification) int { return cmp.Compare(a.Path, b.Path) })

	if len(floated) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrPinFloated, strings.Join(floated, "; "))
	}
	if len(added) > 0 {
		return nil, fmt.Errorf("%w: the go command added %s", ErrModuleDrift, strings.Join(added, "; "))
	}
	return report, nil
}

// compareDirectives checks everything about the module that is not a
// requirement.
func compareDirectives(intended, actual *modfile.File) error {
	switch {
	case actual.Module == nil || intended.Module == nil:
		return fmt.Errorf("%w: the module directive disappeared", ErrModuleDrift)
	case actual.Module.Mod.Path != intended.Module.Mod.Path:
		return fmt.Errorf("%w: module path became %s rather than %s", ErrModuleDrift, actual.Module.Mod.Path, intended.Module.Mod.Path)
	}
	if got, want := goVersionOf(actual), goVersionOf(intended); got != want {
		return fmt.Errorf("%w: go directive became %q rather than %q", ErrModuleDrift, got, want)
	}
	if got, want := toolchainOf(actual), toolchainOf(intended); got != want {
		return fmt.Errorf("%w: toolchain directive became %q rather than %q", ErrModuleDrift, got, want)
	}
	if got, want := godebugOf(actual), godebugOf(intended); !slices.Equal(got, want) {
		return fmt.Errorf("%w: godebug directives became %v rather than %v", ErrModuleDrift, got, want)
	}
	if len(actual.Replace) > 0 {
		targets := make([]string, len(actual.Replace))
		for i, replace := range actual.Replace {
			targets[i] = fmt.Sprintf("%s => %s", replace.Old.Path, replace.New.Path)
		}
		slices.Sort(targets)
		return fmt.Errorf("%w: the generated module must carry no replace directives, found %s", ErrModuleDrift, strings.Join(targets, "; "))
	}
	if len(actual.Exclude) > 0 {
		return fmt.Errorf("%w: the generated module must carry no exclude directives, found %d", ErrModuleDrift, len(actual.Exclude))
	}
	return nil
}

// goVersionOf reports a file's go directive, or the empty string when it has
// none.
func goVersionOf(file *modfile.File) string {
	if file.Go == nil {
		return ""
	}
	return file.Go.Version
}

// toolchainOf reports a file's toolchain directive, or the empty string.
func toolchainOf(file *modfile.File) string {
	if file.Toolchain == nil {
		return ""
	}
	return file.Toolchain.Name
}

// godebugOf reports a file's godebug directives in sorted key order.
func godebugOf(file *modfile.File) []string {
	if len(file.Godebug) == 0 {
		return nil
	}
	directives := make([]string, len(file.Godebug))
	for i, directive := range file.Godebug {
		directives[i] = directive.Key + "=" + directive.Value
	}
	slices.Sort(directives)
	return directives
}
