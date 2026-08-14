package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Object write sentinels. Each names a verdict the caller has to act on rather
// than a command that happened to fail.
var (
	// ErrTagNotFound reports a tag name that no ref resolves to. It is distinct
	// from ErrObjectNotFound because a missing tag is a fact about the ref
	// namespace, which a publisher checks before it creates one, while a missing
	// object is a fact about the object store.
	ErrTagNotFound = errors.New("tag does not exist")
	// ErrDuplicateTreeEntry reports two entries claiming one path. Git would
	// keep whichever the index happened to apply last, so the tree would be
	// written from an input the caller never meant to describe.
	ErrDuplicateTreeEntry = errors.New("tree path is claimed by more than one entry")
	// ErrTreePathConflict reports a path used as both a file and a directory.
	//
	// Git does not refuse this. update-index accepts both entries and write-tree
	// resolves the clash by dropping one, reporting success and an object name,
	// so a generated module carrying such a pair would publish a tree quietly
	// missing a file. Nothing downstream would notice: the tree is well formed,
	// it is simply not the one that was asked for.
	ErrTreePathConflict = errors.New("tree path is used as both a file and a directory")
	// ErrUnsupportedFileMode reports a mode a generated tree cannot record.
	ErrUnsupportedFileMode = errors.New("file mode is not one a generated tree records")
	// ErrReservedTreePath reports a path holding a component that names git's
	// own directory.
	//
	// Git neither records it nor refuses it. update-index prints "Ignoring path"
	// to standard error and exits zero, and write-tree then reports a tree that
	// is simply missing the entry, so this is the ErrTreePathConflict failure
	// again by another route.
	ErrReservedTreePath = errors.New("tree path names git's own directory")
	// ErrTreeEntryDropped reports that update-index declined an entry without
	// failing. It is the net under the named rules above: which paths git will
	// record depends on configuration this package does not own, so the only
	// durable evidence that every entry was staged is that git said nothing.
	ErrTreeEntryDropped = errors.New("git declined to stage a tree entry")
)

// ObjectFormat is a repository's hash algorithm. It decides how long an object
// name is, and the two formats are not interchangeable: an object name written
// under one is meaningless under the other.
type ObjectFormat string

// The hash algorithms git supports.
const (
	ObjectFormatSHA1   ObjectFormat = "sha1"
	ObjectFormatSHA256 ObjectFormat = "sha256"
)

// HexLength reports how many characters an object name occupies in this format.
func (f ObjectFormat) HexLength() int {
	switch f {
	case ObjectFormatSHA1:
		return 40
	case ObjectFormatSHA256:
		return 64
	default:
		return 0
	}
}

// ObjectFormat reports the repository's hash algorithm.
//
// Replay needs it as data rather than as an assumption. The engine computes
// object names locally to decide what it already has, and a computation that
// guessed the algorithm would not fail: it would produce names that never match,
// so every object would look absent and be rewritten.
func (r *Runner) ObjectFormat(ctx context.Context) (ObjectFormat, error) {
	out, err := r.run(ctx, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("git object format: %w", err)
	}
	switch format := ObjectFormat(strings.TrimSpace(out)); format {
	case ObjectFormatSHA1, ObjectFormatSHA256:
		return format, nil
	default:
		return "", fmt.Errorf("git object format: unsupported format %q", string(format))
	}
}

// InitRepositoryWithFormat creates a repository whose objects use the named hash
// algorithm.
//
// The algorithm is fixed when a repository is created and cannot be changed
// afterwards, so a destination repository has to choose it deliberately rather
// than inherit whatever the local git defaults to. InitRepository remains the
// call for a repository whose object names are nobody's business but its own.
func (r *Runner) InitRepositoryWithFormat(ctx context.Context, initialBranch string, format ObjectFormat) error {
	if err := ValidateBranchName(initialBranch); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	if format.HexLength() == 0 {
		return fmt.Errorf("git init: unsupported object format %q", string(format))
	}
	args := []string{"init", "--quiet", "--initial-branch=" + initialBranch, "--object-format=" + string(format)}
	if _, err := r.run(ctx, args...); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	return nil
}

// FileMode is the mode a tree records for one blob.
type FileMode string

// The modes a generated tree may contain. Git's remaining modes are absent
// deliberately: a subtree is implied by the paths rather than named by the
// caller, and a gitlink would publish a submodule pointer to a commit no
// generated repository contains.
const (
	// ModeRegular is a non executable file.
	ModeRegular FileMode = "100644"
	// ModeExecutable is an executable file.
	ModeExecutable FileMode = "100755"
	// ModeSymlink is a symbolic link whose blob content is the link target.
	ModeSymlink FileMode = "120000"
)

// valid reports whether the mode is one this package writes.
func (m FileMode) valid() bool {
	return m == ModeRegular || m == ModeExecutable || m == ModeSymlink
}

// TreeEntry is one blob in a tree, at one path.
type TreeEntry struct {
	// Mode is the recorded file mode.
	Mode FileMode
	// Object is the full blob object name. Short names are refused because a
	// tree records the resolved object, and resolving an abbreviation here would
	// make the written tree depend on which other objects the repository happens
	// to hold.
	Object string
	// Path is the repository relative path, separated by forward slashes.
	// Directories are implied by it rather than listed separately.
	Path string
}

// validate checks one entry against what a tree can record.
func (e TreeEntry) validate() error {
	if !e.Mode.valid() {
		return fmt.Errorf("path %q mode %q: %w", e.Path, string(e.Mode), ErrUnsupportedFileMode)
	}
	if !isObjectName(e.Object) {
		return fmt.Errorf("path %q object %q must be a full object name", e.Path, e.Object)
	}
	// The same rules ReadBlob applies, so every path this writes can be read
	// back through it. A tree that accepted a path the reader refuses would only
	// fail later, at a verification step, with nothing left to point at.
	if err := validateBlobPath(e.Path); err != nil {
		return err
	}
	for component := range strings.SplitSeq(e.Path, "/") {
		if isReservedComponent(component) {
			return fmt.Errorf("path %q component %q: %w", e.Path, component, ErrReservedTreePath)
		}
	}
	return nil
}

// isReservedComponent reports a path component git will not record because it
// names git's own directory.
//
// Git refuses the literal name and the spellings a Windows filesystem maps onto
// it: trailing dots and spaces are dropped there, and "git~1" is the 8.3 short
// name generated for ".git". The neighbours that genuinely appear in a module,
// ".gitignore", ".gitattributes" and ".github", are untouched by this, and so
// is "git~10", which git itself records.
func isReservedComponent(component string) bool {
	trimmed := strings.TrimRight(component, ". ")
	return strings.EqualFold(trimmed, ".git") || strings.EqualFold(trimmed, "git~1")
}

// WriteBlob writes content to the object store and reports the blob's name.
//
// The content travels on standard input, so it is never an argument and may hold
// any byte, including nulls and invalid UTF-8. Filters are disabled: a clean
// filter is a repository local rule about what a working tree file becomes on
// its way into the object store, and applying one here would make the published
// bytes depend on configuration rather than on the content the engine produced.
func (r *Runner) WriteBlob(ctx context.Context, content []byte) (string, error) {
	// A nil slice would leave stdin closed rather than empty, and an empty blob
	// is a legitimate object, so the input is always a non nil slice.
	if content == nil {
		content = []byte{}
	}
	out, err := r.runInput(ctx, content, nil, "hash-object", "-w", "--stdin", "--no-filters", "-t", "blob")
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w", err)
	}
	name := strings.TrimSpace(out)
	if !isObjectName(name) {
		return "", fmt.Errorf("git hash-object: %q is not an object name", name)
	}
	return name, nil
}

// WriteTree writes a complete tree and reports its object name.
//
// The entries are staged into an index of this call's own, named through
// GIT_INDEX_FILE in a temporary directory, so the repository's index is neither
// read nor written. That matters beyond tidiness: replay runs against a
// repository a person may also be using, and building a tree through the shared
// index would make the published output depend on whatever was staged there.
//
// Entries are sorted and framed with nulls on standard input, so two callers
// that describe the same tree in different orders write the same object, and a
// path holding a space, a tab, or a quote cannot be split into two records.
//
// Every named object is confirmed to be a blob this repository already holds
// before any of them is staged, which costs one batched probe per tree. See
// checkTreeObjects for what that check is worth.
func (r *Runner) WriteTree(ctx context.Context, entries []TreeEntry) (tree string, err error) {
	if len(entries) == 0 {
		return "", errors.New("git write-tree: at least one entry is required")
	}
	sorted := slices.SortedFunc(slices.Values(entries), func(a, b TreeEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	if err := checkTreePaths(sorted); err != nil {
		return "", fmt.Errorf("git write-tree: %w", err)
	}
	if err := r.checkTreeObjects(ctx, sorted); err != nil {
		return "", fmt.Errorf("git write-tree: %w", err)
	}
	var input bytes.Buffer
	for _, entry := range sorted {
		// update-index reads mode, object name, a tab, then the path up to the
		// null, so only the first tab is a separator and a path may hold more.
		fmt.Fprintf(&input, "%s %s\t%s\x00", string(entry.Mode), entry.Object, entry.Path)
	}

	dir, err := os.MkdirTemp("", "soapbox-index-")
	if err != nil {
		return "", fmt.Errorf("git write-tree: create index directory: %w", err)
	}
	// The directory holds the index and the lock git writes beside it, and it is
	// removed on every path out of here, including cancellation, so an abandoned
	// run leaves no index behind. A failed removal is reported rather than
	// dropped, and it discards the tree name with it: a run that cannot say
	// whether it left an index behind has not finished cleanly, and a caller
	// reading only the object name would never learn that.
	defer func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("git write-tree: remove index directory: %w", removeErr))
			tree = ""
		}
	}()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}

	_, stderr, err := r.runCapture(ctx, input.Bytes(), env, "update-index", "-z", "--index-info")
	if err != nil {
		return "", fmt.Errorf("git update-index: %w", err)
	}
	// update-index reports a path it declined on standard error and still exits
	// zero, so silence is the only signal that the index holds every entry. The
	// stream is treated as fatal whatever it says, because a message this does
	// not recognise is a rule git applied that this package does not model.
	if message := strings.TrimSpace(stderr); message != "" {
		return "", fmt.Errorf("git update-index: %s: %w", message, ErrTreeEntryDropped)
	}
	out, err := r.runWith(ctx, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("git write-tree: %w", err)
	}
	tree = strings.TrimSpace(out)
	if !isObjectName(tree) {
		return "", fmt.Errorf("git write-tree: %q is not an object name", tree)
	}
	return tree, nil
}

// checkTreePaths validates path sorted entries and refuses the two clashes git
// resolves on its own.
//
// Both are checked here rather than left to write-tree because git reports
// neither. A repeated path and a path that is also a directory each produce a
// well formed tree that is missing an entry, so the only place the caller's
// actual input can still be compared against what it described is before the
// index is fed. Each entry's own validation, which refuses a path naming git's
// directory, runs from here for the same reason.
func checkTreePaths(sorted []TreeEntry) error {
	paths := make(map[string]struct{}, len(sorted))
	for i, entry := range sorted {
		if err := entry.validate(); err != nil {
			return err
		}
		if i > 0 && sorted[i-1].Path == entry.Path {
			return fmt.Errorf("path %q: %w", entry.Path, ErrDuplicateTreeEntry)
		}
		paths[entry.Path] = struct{}{}
	}
	// Every ancestor is tested rather than only the preceding entry, because
	// sorting does not place a file next to the directory that shadows it: with
	// pkg, pkg!x, and pkg/doc.go, the clashing pair is two entries apart.
	for _, entry := range sorted {
		for i, r := range entry.Path {
			if r != '/' {
				continue
			}
			if _, clash := paths[entry.Path[:i]]; clash {
				return fmt.Errorf("path %q is under %q: %w", entry.Path, entry.Path[:i], ErrTreePathConflict)
			}
		}
	}
	return nil
}

// checkTreeObjects refuses an entry naming something other than a blob this
// repository already holds.
//
// write-tree refuses an object it cannot find, so a missing blob is caught
// whether or not this runs. It does not check the type. An entry naming a tree
// or a commit under a file mode is staged and written without complaint, and
// the result is the same class of quiet wrong answer ErrTreePathConflict exists
// for: the tree is well formed, ls-tree reports the entry as the blob its mode
// claims, and only fsck or a reader that tries to inflate the content ever
// disagrees. By then the tree has been published.
//
// The objects are probed in one batch, deduplicated in path sorted order so the
// same tree asks the same question every time. That is one extra subprocess per
// tree. It buys the guarantee that a tree this package writes is one git will
// still call valid, which a publisher cannot check any later.
func (r *Runner) checkTreeObjects(ctx context.Context, sorted []TreeEntry) error {
	distinct := make([]string, 0, len(sorted))
	seen := make(map[string]struct{}, len(sorted))
	for _, entry := range sorted {
		if _, repeated := seen[entry.Object]; repeated {
			continue
		}
		seen[entry.Object] = struct{}{}
		distinct = append(distinct, entry.Object)
	}
	// The probe answers from the local object store only, which is what "already
	// holds" means. Letting it reach a promisor remote would turn a validation
	// step into a fetch.
	infos, err := r.ObjectInfoBatch(ctx, ObjectInfoOptions{Revisions: distinct})
	if err != nil {
		return err
	}
	if len(infos) != len(distinct) {
		return fmt.Errorf("probed %d objects, got %d answers", len(distinct), len(infos))
	}
	for i, info := range infos {
		switch {
		case info.Missing:
			return fmt.Errorf("object %s: %w", distinct[i], ErrObjectNotFound)
		case info.Type != "blob":
			return fmt.Errorf("object %s is a %s: %w", distinct[i], info.Type, ErrNotABlob)
		}
	}
	return nil
}

// lsTreeArgs builds the vector that lists every blob a tree holds.
//
// The vector ends at the revision. --end-of-options already tells git that the
// revision is not an option however it is spelled, so a trailing bare "--"
// would add nothing but an empty pathspec, and a pathspec is the one argument
// that can make ls-tree answer with fewer entries than the tree holds. A
// listing that quietly shrank would defeat the read back this exists to serve,
// so its absence is pinned by a test rather than left to review.
func lsTreeArgs(revision string) []string {
	return []string{"ls-tree", "-r", "-z", "--full-tree", "--end-of-options", revision}
}

// ListTree lists every blob a tree holds, recursively, sorted by path.
//
// It is the read side of WriteTree, and the pair is what lets a caller prove a
// tree holds exactly the files it meant to publish rather than trusting that the
// write did what it was asked.
//
// Paths reach the caller exactly as git recorded them, without passing through
// the redactor, for the reason ReadBlob's bytes do: a path is content, and
// rewriting one that merely happened to match a secret would report a tree that
// does not exist.
func (r *Runner) ListTree(ctx context.Context, revision string) ([]TreeEntry, error) {
	if err := validateRevision(revision); err != nil {
		return nil, fmt.Errorf("git ls-tree: %w", err)
	}
	// -z stops git quoting a path that holds a space, a quote, or a byte outside
	// ASCII, which is the difference between reading the path and reading git's
	// rendering of it.
	out, err := r.runRaw(ctx, nil, lsTreeArgs(revision)...)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %q: %w", r.redactor.String(revision), err)
	}
	var entries []TreeEntry
	for _, record := range splitNull(out) {
		entry, err := parseTreeRecord(record)
		if err != nil {
			return nil, fmt.Errorf("git ls-tree %q: %w", r.redactor.String(revision), err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// parseTreeRecord reads one ls-tree record, which is a mode, a type, and an
// object name separated by spaces, then a tab, then the path.
func parseTreeRecord(record string) (TreeEntry, error) {
	head, path, ok := strings.Cut(record, "\t")
	if !ok {
		return TreeEntry{}, fmt.Errorf("record %q has no path separator", record)
	}
	fields := strings.Split(head, " ")
	if len(fields) != 3 {
		return TreeEntry{}, fmt.Errorf("record %q has %d fields before the path, want 3", record, len(fields))
	}
	mode, objectType, object := FileMode(fields[0]), fields[1], fields[2]
	if objectType != "blob" {
		return TreeEntry{}, fmt.Errorf("path %q is a %s, and only blobs are listed", path, objectType)
	}
	if !mode.valid() {
		return TreeEntry{}, fmt.Errorf("path %q mode %q: %w", path, string(mode), ErrUnsupportedFileMode)
	}
	if !isObjectName(object) {
		return TreeEntry{}, fmt.Errorf("path %q object %q is not an object name", path, object)
	}
	return TreeEntry{Mode: mode, Object: object, Path: path}, nil
}

// TagObjectOptions describes one annotated tag object to write.
type TagObjectOptions struct {
	// Object is the full object name the tag points at.
	Object string
	// Type is the pointed at object's type, normally commit.
	Type string
	// Name is the short tag name recorded inside the object, such as v0.36.1.
	Name string
	// Message is the tag message, recorded verbatim.
	Message string
	// Tagger is the recorded identity. Date must be git's raw form,
	// "<seconds> <±hhmm>", because this writes the object's bytes rather than
	// asking git to format them.
	Tagger Signature
}

// The object types a tag may point at.
var tagTargetTypes = []string{"commit", "tree", "blob", "tag"}

// WriteTagObject writes an annotated tag object and reports its name, without
// creating a ref.
//
// Separating the object from the ref is what makes a tag reproducible. The
// object carries the message, the tagger, and the date, so writing it is enough
// to learn the name a release would have, and a run can compare that name
// against a published tag without having created anything locally that would
// have to be cleaned up. CreateTag is the step that gives the object a name in
// the ref namespace, and it stays separate so a dry run never takes it.
func (r *Runner) WriteTagObject(ctx context.Context, opts TagObjectOptions) (string, error) {
	if !isObjectName(opts.Object) {
		return "", fmt.Errorf("git mktag: object %q must be a full object name", opts.Object)
	}
	if !slices.Contains(tagTargetTypes, opts.Type) {
		return "", fmt.Errorf("git mktag: type %q must be one of %s", opts.Type, strings.Join(tagTargetTypes, ", "))
	}
	if err := ValidateBranchName(opts.Name); err != nil {
		return "", fmt.Errorf("git mktag: %w", err)
	}
	if opts.Message == "" {
		return "", errors.New("git mktag: a message is required, a tag without one is lightweight")
	}
	if err := validateIdentity(opts.Tagger); err != nil {
		return "", fmt.Errorf("git mktag: tagger: %w", err)
	}

	var object bytes.Buffer
	fmt.Fprintf(&object, "object %s\ntype %s\ntag %s\n", opts.Object, opts.Type, opts.Name)
	fmt.Fprintf(&object, "tagger %s <%s> %s\n\n", opts.Tagger.Name, opts.Tagger.Email, opts.Tagger.Date)
	object.WriteString(opts.Message)

	// mktag parses and fsck checks what it is handed before storing it, so a
	// header this assembled wrongly is refused here rather than becoming an
	// object that only fails when something later reads it.
	out, err := r.runInput(ctx, object.Bytes(), nil, "mktag")
	if err != nil {
		return "", fmt.Errorf("git mktag %q: %w", opts.Name, err)
	}
	name := strings.TrimSpace(out)
	if !isObjectName(name) {
		return "", fmt.Errorf("git mktag %q: %q is not an object name", opts.Name, name)
	}
	return name, nil
}

// validateIdentityFields checks the parts of a signature that are syntax rather
// than data, whichever way the identity reaches git.
//
// A name or address carrying an angle bracket, a line break, or a null byte
// cannot be recorded faithfully: in an object body it would close the address
// early or start a header line, and through the environment it would produce an
// identity git parses back differently from the one supplied. Both are silent,
// so both are refused here rather than compared afterwards.
func validateIdentityFields(s Signature) error {
	switch {
	case s.Name == "":
		return errors.New("a name is required")
	case s.Email == "":
		return errors.New("an email address is required")
	}
	if strings.ContainsAny(s.Name, "<>\n\x00") {
		return fmt.Errorf("name %q must not contain angle brackets, a line break, or a null byte", s.Name)
	}
	if strings.ContainsAny(s.Email, "<>\n\x00") {
		return fmt.Errorf("email %q must not contain angle brackets, a line break, or a null byte", s.Email)
	}
	return nil
}

// validateIdentity checks a signature that is about to be written into an object
// body, where the field separators are part of the syntax rather than an
// argument git will quote, and where the date is stored exactly as given.
//
// The raw date requirement belongs to this form and not to the environment form:
// a signature handed to commit-tree is parsed by git, which accepts the
// friendlier formats Signature documents, while one written into a tag body is
// stored byte for byte and has to already be what git stores.
func validateIdentity(s Signature) error {
	if err := validateIdentityFields(s); err != nil {
		return err
	}
	return ValidateRawDate(s.Date)
}

// ValidateRawDate checks git's raw date form, "<seconds> <±hhmm>".
//
// It is exported because it is the one rule for what a replayed date may be, and
// a second copy of it elsewhere is how the engine ends up with two answers for
// the same upstream commit. An object body records the date as git stores it, so
// this is the one place a signature cannot accept the friendlier formats git's
// date parser understands.
//
// A negative count of seconds is accepted. Dates before 1970 are rare but they
// are real: histories imported from CVS and Subversion carry them, git stores
// them, and refusing one here would make the engine unable to replay a commit
// that upstream published years ago.
func ValidateRawDate(date string) error {
	seconds, zone, ok := strings.Cut(date, " ")
	if !ok {
		return fmt.Errorf("date %q must be git's raw form, such as %q", date, "1700000000 +0530")
	}
	digits := strings.TrimPrefix(seconds, "-")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return fmt.Errorf("date %q must begin with a count of seconds", date)
	}
	if len(zone) != 5 || (zone[0] != '+' && zone[0] != '-') || strings.Trim(zone[1:], "0123456789") != "" {
		return fmt.Errorf("date %q must end with a zone offset such as %q", date, "+0530")
	}
	return nil
}

// Tag is a resolved tag ref.
type Tag struct {
	// Name is the short tag name.
	Name string
	// Object is what the ref points at directly: the tag object when the tag is
	// annotated, and the commit itself when it is lightweight.
	Object string
	// Target is the commit the tag ultimately names, which is the value a
	// caller comparing two tags means.
	Target string
	// Annotated reports a tag that carries its own object. A release tag always
	// does, because it records a tagger and a date of its own.
	Annotated bool
	// Tagger is the recorded identity, with the date in git's raw form. It is
	// empty for a lightweight tag, which has nowhere to record one.
	Tagger Signature
	// Message is the tag message exactly as stored, empty for a lightweight tag.
	Message string
}

// TagInfo describes one tag.
//
// An annotated tag is read from its object rather than through a ref format,
// because the message has to survive byte for byte: for-each-ref terminates each
// record with a newline of its own, which a message that did not end in one
// would silently gain.
func (r *Runner) TagInfo(ctx context.Context, name string) (Tag, error) {
	if err := ValidateBranchName(name); err != nil {
		return Tag{}, fmt.Errorf("git tag info: %w", err)
	}
	// The refname is asked for rather than assumed. A for-each-ref pattern
	// without a glob matches a ref completely OR from the beginning up to a
	// slash, so "refs/tags/v1" also names "refs/tags/v1/beta". Without this the
	// answer for a tag that does not exist would be a description of whichever
	// descendant does, which a publisher would read as "that release is already
	// out" and decline to create.
	const format = "--format=%(refname)%00%(objectname)%00%(objecttype)%00%(*objectname)"
	want := "refs/tags/" + name
	out, err := r.run(ctx, "for-each-ref", format, "--end-of-options", want)
	if err != nil {
		return Tag{}, fmt.Errorf("git tag info %q: %w", name, err)
	}
	// Only the record for the exact ref is considered; a descendant's record is
	// a different tag's answer, not this one's.
	var record string
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if refname, rest, ok := strings.Cut(line, "\x00"); ok && refname == want {
			record = rest
			break
		}
	}
	if record == "" {
		return Tag{}, fmt.Errorf("git tag info %q: %w", name, ErrTagNotFound)
	}
	fields := strings.Split(record, "\x00")
	if len(fields) != 3 {
		return Tag{}, fmt.Errorf("git tag info %q: got %d fields, want 3", name, len(fields))
	}
	object, objectType, dereferenced := fields[0], fields[1], fields[2]

	tag := Tag{Name: name, Object: object, Target: object}
	if objectType != "tag" {
		return tag, nil
	}
	tag.Annotated = true
	tag.Target = dereferenced

	// The tag body is upstream content that a regenerated tag has to reproduce,
	// so it bypasses the redactor for the reason a commit message does.
	body, err := r.runRaw(ctx, nil, "cat-file", "tag", "--end-of-options", object)
	if err != nil {
		return Tag{}, fmt.Errorf("git tag info %q: %w", name, err)
	}
	tagger, message, err := parseTagObject(body)
	if err != nil {
		return Tag{}, fmt.Errorf("git tag info %q: %w", name, err)
	}
	tag.Tagger = tagger
	tag.Message = message
	return tag, nil
}

// parseTagObject splits a tag object into its tagger and its message.
func parseTagObject(body string) (Signature, string, error) {
	header, message, ok := strings.Cut(body, "\n\n")
	if !ok {
		return Signature{}, "", errors.New("tag object has no message")
	}
	for _, line := range strings.Split(header, "\n") {
		value, found := strings.CutPrefix(line, "tagger ")
		if !found {
			continue
		}
		tagger, err := parseIdentityLine(value)
		if err != nil {
			return Signature{}, "", err
		}
		return tagger, message, nil
	}
	return Signature{}, "", errors.New("tag object has no tagger")
}

// parseIdentityLine reads "Name <email> <seconds> <±hhmm>".
//
// The name is whatever precedes the address rather than a field position,
// because a name may contain spaces and, in real upstream history, angle
// brackets. Reading the address from the last bracket pair makes the parse
// depend on the part of the line git guarantees.
func parseIdentityLine(line string) (Signature, error) {
	closing := strings.LastIndex(line, ">")
	if closing < 0 {
		return Signature{}, fmt.Errorf("identity %q has no email address", line)
	}
	opening := strings.LastIndex(line[:closing], "<")
	if opening < 0 {
		return Signature{}, fmt.Errorf("identity %q has no email address", line)
	}
	return Signature{
		Name:  strings.TrimSuffix(line[:opening], " "),
		Email: line[opening+1 : closing],
		Date:  strings.TrimPrefix(line[closing+1:], " "),
	}, nil
}

// TagObject is the content of one annotated tag object read by OID.
type TagObject struct {
	// InternalName is the "tag" header inside the object. It must match the
	// ref name the tag is published under; a mismatch means the object was
	// created for a different name.
	InternalName string
	// TargetOID is the "object" header: the OID the tag points at.
	TargetOID string
	// TargetType is the "type" header, normally "commit".
	TargetType string
	// Tagger is the recorded identity and date.
	Tagger Signature
	// Message is the tag message exactly as stored.
	Message string
}

// TagObjectByOID reads an annotated tag object by its OID using cat-file.
//
// The OID must name a tag object; a commit, tree, or blob is refused. This is
// the read-by-OID complement to TagInfo, which reads by ref name. It exists so
// a tag fetched by OID (via FetchExact) can be inspected without a local ref
// pointing at it.
func (r *Runner) TagObjectByOID(ctx context.Context, oid string) (TagObject, error) {
	if !isObjectName(oid) {
		return TagObject{}, fmt.Errorf("git tag object: %q is not a full object name", oid)
	}
	// Verify the object is a tag before reading its body.
	typeOut, err := r.run(ctx, "cat-file", "-t", "--end-of-options", oid)
	if err != nil {
		return TagObject{}, fmt.Errorf("git tag object %s: %w", oid, err)
	}
	if got := strings.TrimSpace(typeOut); got != "tag" {
		return TagObject{}, fmt.Errorf("git tag object %s: object is type %q, want tag", oid, got)
	}

	body, err := r.runRaw(ctx, nil, "cat-file", "tag", "--end-of-options", oid)
	if err != nil {
		return TagObject{}, fmt.Errorf("git tag object %s: %w", oid, err)
	}

	return parseFullTagObject(body)
}

// parseFullTagObject parses an annotated tag object body into all its fields.
//
// The format is:
//
//	object <oid>\n
//	type <type>\n
//	tag <name>\n
//	tagger <identity> <date>\n
//	\n
//	<message>
func parseFullTagObject(body string) (TagObject, error) {
	header, message, ok := strings.Cut(body, "\n\n")
	if !ok {
		return TagObject{}, errors.New("tag object has no message separator")
	}
	var result TagObject
	result.Message = message

	var sawObject, sawType, sawTag, sawTagger bool
	for _, line := range strings.Split(header, "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found {
			return TagObject{}, fmt.Errorf("tag object has malformed header %q", line)
		}
		switch key {
		case "object":
			if sawObject {
				return TagObject{}, errors.New("tag object has duplicate object header")
			}
			sawObject = true
			result.TargetOID = value
		case "type":
			if sawType {
				return TagObject{}, errors.New("tag object has duplicate type header")
			}
			sawType = true
			result.TargetType = value
		case "tag":
			if sawTag {
				return TagObject{}, errors.New("tag object has duplicate tag header")
			}
			sawTag = true
			result.InternalName = value
		case "tagger":
			if sawTagger {
				return TagObject{}, errors.New("tag object has duplicate tagger header")
			}
			sawTagger = true
			tagger, err := parseIdentityLine(value)
			if err != nil {
				return TagObject{}, err
			}
			result.Tagger = tagger
		default:
			return TagObject{}, fmt.Errorf("tag object has unknown header %q", key)
		}
	}
	if result.TargetOID == "" {
		return TagObject{}, errors.New("tag object has no object header")
	}
	if result.TargetType == "" {
		return TagObject{}, errors.New("tag object has no type header")
	}
	if result.InternalName == "" {
		return TagObject{}, errors.New("tag object has no tag header")
	}
	if !sawTagger {
		return TagObject{}, errors.New("tag object has no tagger header")
	}
	if result.Tagger.Name == "" {
		return TagObject{}, errors.New("tag object tagger has no name")
	}
	if result.Tagger.Email == "" {
		return TagObject{}, errors.New("tag object tagger has no email address")
	}
	if err := ValidateRawDate(result.Tagger.Date); err != nil {
		return TagObject{}, fmt.Errorf("tag object tagger date: %w", err)
	}
	if !isObjectName(result.TargetOID) {
		return TagObject{}, fmt.Errorf("tag object target OID %q is not a full object name", result.TargetOID)
	}
	if !slices.Contains(tagTargetTypes, result.TargetType) {
		return TagObject{}, fmt.Errorf("tag object target type %q is not a valid git object type", result.TargetType)
	}
	if err := ValidateBranchName(result.InternalName); err != nil {
		return TagObject{}, fmt.Errorf("tag object internal name: %w", err)
	}
	return result, nil
}
