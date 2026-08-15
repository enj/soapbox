// Package treebuild turns a relocated file set into the Git objects a generated
// module is published from.
//
// Nothing here touches a work tree, an index the repository shares, or a ref.
// A file set arrives as bytes in memory and leaves as a tree object name, so the
// same set produces the same tree in a bare repository, in a checkout someone
// else is using, and on a machine with different configuration. That is the
// property the whole engine rests on: a published module has to be reproducible
// from its inputs, and a tree built through a checkout would instead record
// whatever the checkout happened to contain.
//
// Every blob name is computed here before git is asked for anything. Computing
// it is what makes the write skippable, because an object the repository already
// holds must not be written again, and it is also the check on the write: git's
// answer is compared against the computed name, so a filter or a conversion that
// changed the bytes on their way into the object store is a failure rather than
// a different published file. The tree is read back and compared against the
// entries it was built from for the same reason. git's own answer to a tree
// entry it dislikes is silence, and silence has to be turned into an error
// somewhere.
package treebuild

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: Git SHA-1 object names require the SHA-1 algorithm
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// Build sentinels. Each names a verdict a caller has to act on rather than a
// command that happened to fail.
var (
	// ErrObjectMismatch reports a blob git named differently from the name
	// computed for its content. It means the bytes changed between here and the
	// object store, which is the one failure this package exists to make loud.
	ErrObjectMismatch = errors.New("git named a blob differently from its computed name")
	// ErrTreeMismatch reports a written tree that does not read back as the
	// entries it was built from.
	ErrTreeMismatch = errors.New("written tree does not hold the entries it was built from")
	// ErrUnsupportedMode reports a relocated file mode a tree cannot record.
	ErrUnsupportedMode = errors.New("file mode is not one a generated tree records")
	// ErrRawDate reports a date that is not git's raw form. A published object
	// records the date as git stores it, and any other format is interpreted on
	// the way in, which makes the object depend on who wrote it.
	ErrRawDate = errors.New("date must be git's raw form, <seconds> <±hhmm>")
)

// ManifestFile is one file of a written tree.
type ManifestFile struct {
	// Path is the module relative destination path.
	Path string
	// Mode is the mode the tree records.
	Mode gitcli.FileMode
	// Object is the blob object name.
	Object string
	// Size is the content length in bytes.
	Size int64
}

// Manifest is the deterministic record of one written tree.
//
// It holds object names, paths inside the generated module, modes, and counts,
// and deliberately nothing else. A local directory, a temporary path, or an
// environment value would make two identical builds produce different records,
// and this record is meant to be comparable across machines and printable in a
// dry run that a person approves before anything is published.
type Manifest struct {
	// Format is the hash algorithm the objects were written under.
	Format gitcli.ObjectFormat
	// Tree is the tree object name.
	Tree string
	// Files are the tree entries in path order.
	Files []ManifestFile
	// Written counts the distinct blobs this build stored.
	Written int
	// Reused counts the distinct blobs the repository already held.
	Reused int
	// Bytes is the total content length of the distinct blobs.
	Bytes int64
}

// Report renders the manifest as deterministic lines for a dry run.
//
// Two builds of the same file set produce identical lines, and the lines carry
// nothing about the machine that produced them.
//
// Written and Reused are deliberately absent. They describe what this
// repository already happened to hold rather than what was built, so a second
// build of an unchanged module would otherwise report differently from the
// first and a comparison between two dry runs would show a difference that is
// not one. Their sum, the number of distinct blobs, is a fact about the content
// and is reported.
func (m Manifest) Report() []string {
	lines := make([]string, 0, len(m.Files)+3)
	lines = append(lines,
		"format "+string(m.Format),
		"tree "+m.Tree,
		fmt.Sprintf("blobs %d bytes %d", m.Written+m.Reused, m.Bytes),
	)
	for _, file := range m.Files {
		lines = append(lines, strings.Join([]string{
			string(file.Mode), file.Object, strconv.FormatInt(file.Size, 10), file.Path,
		}, " "))
	}
	return lines
}

// WriteFileSet writes every file of a relocated set as a blob and builds the
// tree that holds them.
//
// Files with identical content share one blob, which is what git does anyway:
// the mode lives in the tree entry rather than in the object, so a file and an
// executable with the same bytes are one object named twice. Blobs the
// repository already holds are probed for in a single batch and left alone,
// which is what keeps a replay over tens of thousands of commits from rewriting
// the same unchanged file once per commit.
//
// The probe answers from the local object store only. A partial clone would
// otherwise reach the network to decide whether it already has an object, and
// "do I have this" is a question about this repository rather than about the
// upstream one.
func WriteFileSet(ctx context.Context, git *gitcli.Runner, set relocate.FileSet) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, fmt.Errorf("treebuild: %w", err)
	}
	if len(set.Files) == 0 {
		return Manifest{}, errors.New("treebuild: the file set holds no files")
	}
	format, err := git.ObjectFormat(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("treebuild: %w", err)
	}

	entries := make([]gitcli.TreeEntry, 0, len(set.Files))
	files := make([]ManifestFile, 0, len(set.Files))
	contents := make(map[string][]byte, len(set.Files))
	// The distinct names are kept in the order the sorted set first reaches
	// them, so the probe below asks the same question in the same order every
	// time this set is built.
	distinct := make([]string, 0, len(set.Files))
	for _, file := range set.Files {
		mode, err := treeMode(file.Mode)
		if err != nil {
			return Manifest{}, fmt.Errorf("treebuild %q: %w", file.Path, err)
		}
		name, err := blobName(format, file.Contents)
		if err != nil {
			return Manifest{}, fmt.Errorf("treebuild %q: %w", file.Path, err)
		}
		if _, seen := contents[name]; !seen {
			contents[name] = file.Contents
			distinct = append(distinct, name)
		}
		entries = append(entries, gitcli.TreeEntry{Mode: mode, Object: name, Path: file.Path})
		files = append(files, ManifestFile{Path: file.Path, Mode: mode, Object: name, Size: int64(len(file.Contents))})
	}

	// The manifest is sorted rather than left in the order the set arrived in,
	// so it describes the tree rather than the call. Two builds of the same
	// module produce the same record whatever order the closure emitted files
	// in, which is what makes two dry runs comparable.
	slices.SortFunc(files, func(a, b ManifestFile) int { return strings.Compare(a.Path, b.Path) })
	manifest := Manifest{Format: format, Files: files}
	infos, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: distinct})
	if err != nil {
		return Manifest{}, fmt.Errorf("treebuild: %w", err)
	}
	if len(infos) != len(distinct) {
		return Manifest{}, fmt.Errorf("treebuild: probed %d objects, got %d answers", len(distinct), len(infos))
	}
	for i, info := range infos {
		name := distinct[i]
		content := contents[name]
		manifest.Bytes += int64(len(content))
		if !info.Missing {
			// A present object is trusted only after it is confirmed to be the
			// object the name promises. An object name that resolved to another
			// type, or to a different length, would mean the computed name and
			// the stored object disagree, and reusing it would publish content
			// nobody assembled.
			if info.Type != "blob" {
				return Manifest{}, fmt.Errorf("treebuild: object %s is a %s, not a blob", name, info.Type)
			}
			if info.Size != int64(len(content)) {
				return Manifest{}, fmt.Errorf("treebuild: object %s holds %d bytes, want %d", name, info.Size, len(content))
			}
			manifest.Reused++
			continue
		}
		written, err := git.WriteBlob(ctx, content)
		if err != nil {
			return Manifest{}, fmt.Errorf("treebuild: %w", err)
		}
		if written != name {
			return Manifest{}, fmt.Errorf("treebuild: %w: computed %s, git wrote %s", ErrObjectMismatch, name, written)
		}
		manifest.Written++
	}

	tree, err := git.WriteTree(ctx, entries)
	if err != nil {
		return Manifest{}, fmt.Errorf("treebuild: %w", err)
	}
	listed, err := git.ListTree(ctx, tree)
	if err != nil {
		return Manifest{}, fmt.Errorf("treebuild: %w", err)
	}
	if err := sameEntries(entries, listed); err != nil {
		return Manifest{}, fmt.Errorf("treebuild: tree %s: %w", tree, err)
	}
	manifest.Tree = tree
	return manifest, nil
}

// sameEntries reports whether a tree read back holds exactly the entries it was
// written from.
//
// This is the check that catches an entry git decided not to record. Both sides
// are sorted rather than compared in place, so the result does not depend on
// git's tree ordering matching a byte ordering of the paths.
func sameEntries(want, got []gitcli.TreeEntry) error {
	order := func(a, b gitcli.TreeEntry) int { return strings.Compare(a.Path, b.Path) }
	wantSorted := slices.SortedFunc(slices.Values(want), order)
	gotSorted := slices.SortedFunc(slices.Values(got), order)
	if len(wantSorted) != len(gotSorted) {
		return fmt.Errorf("%w: holds %d entries, want %d", ErrTreeMismatch, len(gotSorted), len(wantSorted))
	}
	for i, want := range wantSorted {
		if got := gotSorted[i]; got != want {
			return fmt.Errorf("%w: entry %d is %s %s %q, want %s %s %q",
				ErrTreeMismatch, i, string(got.Mode), got.Object, got.Path,
				string(want.Mode), want.Object, want.Path)
		}
	}
	return nil
}

// treeMode maps a relocated file mode onto the mode a tree records.
//
// The switch is exhaustive on purpose rather than deferring to the mode's own
// rendering: an unrecognised mode has to fail here, where the file can be named,
// instead of reaching git as a string that means nothing.
func treeMode(mode relocate.Mode) (gitcli.FileMode, error) {
	switch mode {
	case relocate.ModeRegular:
		return gitcli.ModeRegular, nil
	case relocate.ModeExecutable:
		return gitcli.ModeExecutable, nil
	case relocate.ModeSymlink:
		return gitcli.ModeSymlink, nil
	default:
		return "", fmt.Errorf("mode %s: %w", mode, ErrUnsupportedMode)
	}
}

// blobName computes the object name git gives a blob holding content.
//
// The construction is git's own: the header "blob <length>", a null byte, then
// the content, hashed under the repository's algorithm. Computing it locally is
// what lets a build decide whether an object already exists without a round trip
// per file, and it is what the write is checked against afterwards.
func blobName(format gitcli.ObjectFormat, content []byte) (string, error) {
	payload := make([]byte, 0, len(content)+32)
	payload = fmt.Appendf(payload, "blob %d\x00", len(content))
	payload = append(payload, content...)
	switch format {
	case gitcli.ObjectFormatSHA1:
		// SHA-1 is not a security choice. It is the name git gives an object in
		// a sha1 repository, and computing anything else would simply never
		// match what the object store holds.
		sum := sha1.Sum(payload) //nolint:gosec // git's own object name in a sha1 repository
		return hex.EncodeToString(sum[:]), nil
	case gitcli.ObjectFormatSHA256:
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:]), nil
	default:
		return "", fmt.Errorf("unsupported object format %q", string(format))
	}
}

// validateRawDate checks git's raw date form, "<seconds> <±hhmm>".
//
// Every date this package writes is checked, because the friendlier formats git
// accepts are interpreted before they are stored: a commit built from an RFC
// 3339 date records whatever that meant to the machine that wrote it, so
// regenerating it elsewhere produces a different object name. Raw is the only
// form that is already what git stores.
//
// The rule itself is gitcli's rather than a second copy of it. A copy is how the
// engine ends up with two answers for one upstream commit, and it had one: this
// package used to refuse the negative second counts gitcli accepts, so a
// pre-1970 author date could be written into a tag and refused for a commit.
// Only the role prefix and the sentinel are added here.
func validateRawDate(role, date string) error {
	if err := gitcli.ValidateRawDate(date); err != nil {
		return fmt.Errorf("%s %w: %w", role, err, ErrRawDate)
	}
	return nil
}
