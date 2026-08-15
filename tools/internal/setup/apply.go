package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// stagePattern names the sibling directory a run assembles its payload in. The
// leading dot keeps it out of ordinary listings if a run is killed between the
// assembly and the cleanup.
const stagePattern = ".soapbox-setup-"

// Permissions the transformation creates. The file mode is the one Git records
// for a regular blob, so a fresh checkout of the derived repository and the
// repository setup produced look the same.
const (
	payloadFileMode = 0o644
	payloadDirMode  = 0o750
)

// tempSuffix names the transient file an atomic write renames from.
const tempSuffix = ".soapbox-setup.tmp"

// apply writes the approved manifest into the repository.
//
// The payload is assembled in full in a sibling directory and read back before
// the repository is touched, so a composition that cannot be written fails while
// the repository is still exactly as the operator left it.
//
// The assembled tree is then applied file by file rather than moved into place
// as a whole. A whole-tree replacement cannot be used here and the reason is not
// a limitation of the file system: the destination is the repository itself, and
// renaming a tree over it would take .git and every unrecognised file the
// operator owns with it. Preserving those is the point of the allowlist, so the
// atomicity that is kept is per file, which is the granularity at which a
// partial run is recoverable.
//
// Writes precede deletes for that recovery. Everything deleted is tracked at
// HEAD and the work tree was clean, so a run interrupted at any point leaves a
// repository that "git checkout -- . && git clean -fd" restores exactly.
func (r *run) apply(ctx context.Context) error {
	staged, err := r.stage(ctx)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(r.opts.Root)
	if err != nil {
		return fmt.Errorf("setup: open repository: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, action := range r.report.Actions {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		if action.Kind == ActionDelete {
			continue
		}
		contents, ok := staged[action.Path]
		if !ok {
			return fmt.Errorf("setup: %s was planned but not staged", action.Path)
		}
		if err := writeAtomic(root, action.Path, contents); err != nil {
			return fmt.Errorf("setup: write %s: %w", action.Path, err)
		}
	}

	var removedDirs []string
	for _, action := range r.report.Actions {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		if action.Kind != ActionDelete {
			continue
		}
		current, err := root.ReadFile(action.Path)
		if err != nil {
			return &PolicyError{Err: fmt.Errorf("setup: %w: %s changed or disappeared after planning: %w", ErrApproval, action.Path, err)}
		}
		if got := digest(current); got != action.Digest {
			return &PolicyError{Err: fmt.Errorf("setup: %w: %s now digests to %s, approved %s", ErrApproval, action.Path, got, action.Digest)}
		}
		if err := root.Remove(action.Path); err != nil {
			return fmt.Errorf("setup: remove %s: %w", action.Path, err)
		}
		removedDirs = append(removedDirs, path.Dir(action.Path))
	}
	return pruneEmptyDirs(ctx, root, removedDirs)
}

// stage assembles the whole payload inside a reserved scratch directory in the
// repository and reads it back. Keeping the scratch inside Root preserves the
// package contract that setup writes nowhere else; it is removed before any
// payload path is changed.
//
// The read back is the point. Composing bytes in memory proves they were
// computed; writing and reading them proves the payload can exist as files with
// these names on this file system, which is where a case collision the manifest
// missed or a name the platform refuses would otherwise surface halfway through
// mutating the repository.
func (r *run) stage(ctx context.Context) (map[string][]byte, error) {
	dir, err := os.MkdirTemp(r.opts.Root, stagePattern)
	if err != nil {
		return nil, fmt.Errorf("setup: create staging directory: %w", err)
	}
	staged, stageErr := r.writeStage(ctx, dir)
	if err := os.RemoveAll(dir); err != nil {
		return nil, errors.Join(stageErr, fmt.Errorf("setup: discard staging directory: %w", err))
	}
	if stageErr != nil {
		return nil, stageErr
	}
	return staged, nil
}

// writeStage writes the payload into the staging directory and reads it back,
// comparing what returned against the manifest the operator approved.
func (r *run) writeStage(ctx context.Context, dir string) (map[string][]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("setup: open staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	planned := make(map[string]Action, len(r.report.Actions))
	for _, action := range r.report.Actions {
		if action.Kind != ActionDelete {
			planned[action.Path] = action
		}
	}

	staged := make(map[string][]byte, len(r.payload))
	for _, file := range r.payload {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		if err := writeAtomic(root, file.path, file.contents); err != nil {
			return nil, fmt.Errorf("setup: stage %s: %w", file.path, err)
		}
		contents, err := root.ReadFile(file.path)
		if err != nil {
			return nil, fmt.Errorf("setup: read staged %s: %w", file.path, err)
		}
		action, ok := planned[file.path]
		if !ok {
			return nil, fmt.Errorf("setup: staged %s is not in the approved manifest", file.path)
		}
		if got := digest(contents); got != action.Digest {
			return nil, fmt.Errorf("setup: staged %s is %s, the manifest approved %s", file.path, got, action.Digest)
		}
		if !bytes.Equal(contents, file.contents) {
			return nil, fmt.Errorf("setup: staged %s did not read back as it was written", file.path)
		}
		staged[file.path] = contents
	}
	if len(staged) != len(planned) {
		return nil, fmt.Errorf("setup: the manifest writes %d paths, the payload staged %d", len(planned), len(staged))
	}
	return staged, nil
}

// writeAtomic writes one file through a rename inside its own directory.
//
// The temporary file is a sibling of the target rather than of the tree, because
// a rename is only atomic within one file system and only a sibling is
// guaranteed to be on the same one. Every operation goes through the [os.Root],
// so a path that tried to climb out of the tree, or a symbolic link planted
// where a directory was expected, fails at the operating system rather than at a
// check this package performed earlier against a tree that has since changed.
func writeAtomic(root *os.Root, name string, contents []byte) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), payloadDirMode); err != nil {
			return err
		}
	}
	temp := name + tempSuffix
	if err := root.WriteFile(filepath.FromSlash(temp), contents, payloadFileMode); err != nil {
		return err
	}
	// The permission WriteFile is given is masked by the process umask, so the
	// mode is set explicitly: a file that lost a read bit here would be committed
	// with a mode no other checkout has.
	if err := root.Chmod(filepath.FromSlash(temp), payloadFileMode); err != nil {
		return errors.Join(err, root.Remove(filepath.FromSlash(temp)))
	}
	if err := root.Rename(filepath.FromSlash(temp), filepath.FromSlash(name)); err != nil {
		return errors.Join(err, root.Remove(filepath.FromSlash(temp)))
	}
	return nil
}

// pruneEmptyDirs removes the directories the deletions emptied.
//
// Only a directory that is both inside a template owned prefix and observably
// empty is removed, and the candidates are walked deepest first so that emptying
// a leaf lets its parent go too. A directory that still holds anything is left
// alone, which is how a preserved file keeps the directory it lives in.
func pruneEmptyDirs(ctx context.Context, root *os.Root, dirs []string) error {
	candidates := expandParents(dirs)
	slices.SortFunc(candidates, func(a, b string) int {
		if by := strings.Count(b, "/") - strings.Count(a, "/"); by != 0 {
			return by
		}
		return strings.Compare(b, a)
	})
	for _, dir := range candidates {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		if !templateOwned(dir + "/") {
			continue
		}
		empty, err := isEmptyDir(root, dir)
		if err != nil {
			return fmt.Errorf("setup: inspect %s: %w", dir, err)
		}
		if !empty {
			continue
		}
		if err := root.Remove(filepath.FromSlash(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("setup: remove directory %s: %w", dir, err)
		}
	}
	return nil
}

// expandParents lists every directory and ancestor directory of a deletion,
// without duplicates and without the repository root itself.
func expandParents(dirs []string) []string {
	seen := make(map[string]bool, len(dirs))
	var all []string
	for _, dir := range dirs {
		for current := dir; current != "." && current != "/" && current != ""; current = path.Dir(current) {
			if seen[current] {
				break
			}
			seen[current] = true
			all = append(all, current)
		}
	}
	return all
}

// isEmptyDir reports whether a directory holds nothing at all.
func isEmptyDir(root *os.Root, dir string) (bool, error) {
	handle, err := root.Open(filepath.FromSlash(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = handle.Close() }()

	entries, err := handle.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(entries) == 0, nil
}
