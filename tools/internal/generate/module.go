package generate

import (
	"context"
	"errors"
	"fmt"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
)

// runModule generates and proves the provisional go.mod of both scratch
// modules.
//
// The post-prune module is the one being published, and its tidied go.mod and
// go.sum are what a consumer resolves against. The pre-prune module is generated
// for one reason only: the facade baseline has to be produced from a module the
// Go toolchain can actually load, and a directory of relocated sources with no
// module metadata is not one. Both are generated from the same requirements,
// because they came from the same source commit and differ only in which files
// pruning removed.
//
// Generating is cheap and deterministic; proving is the expensive half and the
// half that matters. A go.mod that merely parses establishes nothing, because
// minimal version selection can raise a requirement above the version this
// engine wrote, and a raised requirement is a dependency nobody approved.
func (r *run) runModule(ctx context.Context) error {
	// Combining the source's external requirements with the resolved staging
	// versions is a pure function of two things this run already proved, so any
	// failure here is a disagreement about the staging layout rather than a
	// runtime condition.
	requirements, err := modgen.RequirementsFor(r.root, r.staging)
	if err != nil {
		return policyError(stageModule, err)
	}

	// The pre-prune module is verified first. It is the cheaper failure to
	// diagnose, because it exercises the same requirements against a strictly
	// larger set of imports: a requirement the post-prune module needs but the
	// resolution missed will fail here as well, and here the answer is not
	// entangled with the profile's pruning.
	preReport, err := r.verifyModule(ctx, r.paths.PreModule, requirements, preDirName)
	if err != nil {
		return err
	}
	postReport, err := r.verifyModule(ctx, r.paths.PostModule, requirements, postDirName)
	if err != nil {
		return err
	}

	// The tree publishes what verification produced rather than what generation
	// wrote, so a report with no module file would leave nothing to publish. It
	// cannot happen after a successful pass, which is why it is an engine
	// failure rather than a finding.
	if len(postReport.GoMod) == 0 {
		return runtimeError(stageModule, errors.New("the verification pass produced no go.mod"))
	}
	r.moduleReport = postReport
	r.report.recordModule(postReport, preReport)
	return nil
}

// verifyModule writes one generated go.mod into a scratch module and proves the
// toolchain agrees with it.
//
// The toolchain directive is deliberately left to the engine's own pin rather
// than taken from the profile. The toolchain that formats the generated output
// and the toolchain the generated output names have to be the same one, or the
// bytes stop being reproducible on a machine that honours the directive.
func (r *run) verifyModule(ctx context.Context, dir string, requirements []gomodmap.Requirement, name string) (*modgen.Report, error) {
	// Generation is a pure rendering of already resolved inputs, so a failure is
	// a statement about what the profile and the source commit ask for.
	goMod, err := modgen.Generate(modgen.Options{
		ModulePath: r.cfg.Destination.Module,
		Go:         r.root.Go,
		Godebug:    r.root.Godebug,
		Require:    requirements,
	})
	if err != nil {
		return nil, policyError(stageModule, fmt.Errorf("%s module: %w", name, err))
	}
	runner, err := r.opts.Go.WithDir(dir)
	if err != nil {
		return nil, runtimeError(stageModule, fmt.Errorf("%s module: %w", name, err))
	}
	// Verification runs the go command, so most of what it can return is a
	// toolchain, proxy, or filesystem condition. Only the two sentinels below
	// mean the toolchain disagreed with the module this engine wrote, which is
	// the finding this pass exists to produce.
	report, err := modgen.Verify(ctx, runner, modgen.VerifyOptions{
		Dir: dir, GoMod: goMod,
		AllowAdditions: r.cfg.Compatibility.Apiserver == config.CompatibilityApiserverLocal,
	})
	if err != nil {
		return nil, classify(stageModule, fmt.Errorf("%s module: %w", name, err), moduleSemantic...)
	}
	return report, nil
}
