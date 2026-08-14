package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// File holds the state document, and MappingFile optionally holds the exact
// staging index its Mapping field identifies.
//
// The record lives in a tree of its own rather than beside the generated
// module, so nothing about it can reach a module consumer: it is not in the
// published tree, it is not in the module zip the proxy serves, and no import
// path resolves into it.
const (
	File        = "state.json"
	MappingFile = "mapping.json"
)

// StoreOptions describes one stored record.
//
// The identity and the dates are supplied rather than read from configuration
// or from the clock, because they are inputs to the commit's object name. A
// commit that took its date from the clock would have a different name on every
// run, and the property this package exists to provide is that a run which did
// the same work writes the same objects.
var ErrMappingEvidenceMissing = errors.New("state record has no reachable mapping evidence")

type StoreOptions struct {
	// Document is the record to store.
	Document Document
	// Mapping is the canonical staging index named by Document.Mapping. Empty
	// preserves compatibility with records written before mapping blobs became
	// reachable from the state tree.
	Mapping []byte
	// Parents are the previous state commits, in order. A first record has none.
	Parents []string
	// Author is the identity the record is attributed to, with a raw date.
	Author gitcli.Signature
	// Committer is the identity that recorded it, with a raw date.
	Committer gitcli.Signature
}

// Record names the objects one stored document produced.
type Record struct {
	// Format is the hash algorithm the objects were written under.
	Format gitcli.ObjectFormat
	// Blob is the object holding the encoded document.
	Blob string
	// MappingBlob is the optional object holding MappingFile.
	MappingBlob string
	// Tree is the object holding the state and optional mapping blobs.
	Tree string
	// Commit is the object holding the tree. No ref points at it.
	Commit string
	// Digest is the document's own digest, repeated here so a caller comparing
	// two runs does not have to decode either one.
	Digest string
	// Bytes is the encoded length of the document.
	Bytes int64
}

// Report renders the record as deterministic lines for a dry run.
//
// Two runs that recorded the same state produce identical lines, and the lines
// carry nothing about the machine that produced them, so a person approving an
// outward plan is comparing the work rather than the runner.
func (r Record) Report() []string {
	lines := []string{
		"format " + string(r.Format),
		"digest " + r.Digest,
		"blob " + r.Blob + " bytes " + strconv.FormatInt(r.Bytes, 10),
	}
	if r.MappingBlob != "" {
		lines = append(lines, "mapping "+r.MappingBlob)
	}
	return append(lines, "tree "+r.Tree, "commit "+r.Commit)
}

// Store writes a document as a blob, a tree, and a commit, and reports their
// names.
//
// No ref is created, moved, or deleted. That is the whole point of the seam: a
// run persists where it got to by writing objects, and whether any branch
// should point at the newest one is a publication decision made after every
// gate has passed. Until that decision is made the commit is unreachable, which
// is exactly what a record of work in progress should be.
//
// The stored bytes are read back out of the commit and checked against the
// bytes that went in, and the document is decoded from them and checked against
// its own digest. A write that git accepted and altered, through a filter or a
// conversion, is a resume from a record nobody produced, and the only place to
// notice is here.
func Store(ctx context.Context, git *gitcli.Runner, opts StoreOptions) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	encoded, err := opts.Document.Encode()
	if err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	if err := requireFormat(ctx, git, opts.Document.ObjectFormat); err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	if err := checkParents(ctx, git, opts.Parents, opts.Document.ObjectFormat); err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}

	files := []relocate.File{{
		Path: File, Mode: relocate.ModeRegular, Contents: encoded,
	}}
	if len(opts.Mapping) > 0 {
		if err := validateMappingEvidence(opts.Document.Mapping, opts.Mapping); err != nil {
			return Record{}, fmt.Errorf("store state: %w", err)
		}
		files = append(files, relocate.File{
			Path: MappingFile, Mode: relocate.ModeRegular, Contents: opts.Mapping,
		})
	}
	manifest, err := treebuild.WriteFileSet(ctx, git, relocate.FileSet{Files: files})
	if err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	if len(manifest.Files) != len(files) {
		return Record{}, fmt.Errorf("store state: the record tree holds %d files, want %d", len(manifest.Files), len(files))
	}
	var stateBlob, mappingBlob string
	var stateBytes int64
	for _, file := range manifest.Files {
		switch file.Path {
		case File:
			stateBlob, stateBytes = file.Object, file.Size
		case MappingFile:
			mappingBlob = file.Object
		}
	}
	if stateBlob == "" {
		return Record{}, fmt.Errorf("store state: the record tree has no %s", File)
	}
	if len(opts.Mapping) > 0 && mappingBlob != opts.Document.Mapping.Object {
		return Record{}, fmt.Errorf("store state: mapping bytes wrote blob %s, document records %s", mappingBlob, opts.Document.Mapping.Object)
	}

	commit, err := treebuild.WriteSyntheticCommit(ctx, git, treebuild.SyntheticCommitOptions{
		Tree:      manifest.Tree,
		Parents:   opts.Parents,
		Author:    opts.Author,
		Committer: opts.Committer,
		Message:   message(opts.Document),
	})
	if err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}

	stored, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: commit, Path: File})
	if err != nil {
		return Record{}, fmt.Errorf("store state: read back: %w", err)
	}
	if !bytes.Equal(stored, encoded) {
		return Record{}, fmt.Errorf("store state: commit %s holds %d bytes at %s, %d were written",
			commit, len(stored), File, len(encoded))
	}
	readBack, err := Decode(stored)
	if err != nil {
		return Record{}, fmt.Errorf("store state: read back: %w", err)
	}
	if readBack.Digest != opts.Document.Digest {
		return Record{}, fmt.Errorf("%w: commit %s holds %s, %s was written",
			ErrDigest, commit, readBack.Digest, opts.Document.Digest)
	}
	if len(opts.Mapping) > 0 {
		storedMapping, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: commit, Path: MappingFile})
		if err != nil {
			return Record{}, fmt.Errorf("store state: read back mapping: %w", err)
		}
		if !bytes.Equal(storedMapping, opts.Mapping) {
			return Record{}, fmt.Errorf("store state: commit %s holds %d mapping bytes, %d were written", commit, len(storedMapping), len(opts.Mapping))
		}
		if err := validateMappingEvidence(readBack.Mapping, storedMapping); err != nil {
			return Record{}, fmt.Errorf("store state: read back: %w", err)
		}
	}

	return Record{
		Format:      manifest.Format,
		Blob:        stateBlob,
		MappingBlob: mappingBlob,
		Tree:        manifest.Tree,
		Commit:      commit,
		Digest:      readBack.Digest,
		Bytes:       stateBytes,
	}, nil
}

// Load reads the document a revision holds.
//
// The revision may be a commit or a tree. It is resolved through the object
// store rather than through a ref, so a caller that already knows which commit
// it wants to resume from does not have to have published it anywhere.
func validateMappingEvidence(mapping Mapping, data []byte) error {
	if mapping.Entries <= 0 {
		return fmt.Errorf("mapping evidence has %d entries", mapping.Entries)
	}
	index, err := gomodmap.Decode(data)
	if err != nil {
		return fmt.Errorf("mapping evidence: %w", err)
	}
	canonical, err := gomodmap.Encode(index)
	if err != nil {
		return fmt.Errorf("mapping evidence: %w", err)
	}
	if !bytes.Equal(canonical, data) {
		return errors.New("mapping evidence is not canonically encoded")
	}
	if index.Len() != mapping.Entries {
		return fmt.Errorf("mapping evidence resolves %d entries, document records %d", index.Len(), mapping.Entries)
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if digest != mapping.Digest {
		return fmt.Errorf("mapping evidence digests to %s, document records %s", digest, mapping.Digest)
	}
	return nil
}

// Inspect describes an existing stored record without rewriting it.
func Inspect(ctx context.Context, git *gitcli.Runner, revision string) (Record, error) {
	doc, err := Load(ctx, git, revision)
	if err != nil {
		return Record{}, err
	}
	commit, err := git.ResolveCommit(ctx, revision)
	if err != nil {
		return Record{}, fmt.Errorf("inspect state %s: resolve commit: %w", revision, err)
	}
	tree, err := git.ResolveTree(ctx, commit)
	if err != nil {
		return Record{}, fmt.Errorf("inspect state %s: resolve tree: %w", revision, err)
	}
	entries, err := git.ListTree(ctx, tree)
	if err != nil {
		return Record{}, fmt.Errorf("inspect state %s: list tree: %w", revision, err)
	}
	var blob, mappingBlob string
	for _, entry := range entries {
		switch entry.Path {
		case File:
			blob = entry.Object
		case MappingFile:
			mappingBlob = entry.Object
		}
	}
	if blob == "" {
		return Record{}, fmt.Errorf("inspect state %s: tree has no %s", revision, File)
	}
	encoded, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: commit, Path: File})
	if err != nil {
		return Record{}, fmt.Errorf("inspect state %s: %w", revision, err)
	}
	return Record{
		Format: doc.ObjectFormat, Blob: blob, MappingBlob: mappingBlob,
		Tree: tree, Commit: commit, Digest: doc.Digest, Bytes: int64(len(encoded)),
	}, nil
}

func Load(ctx context.Context, git *gitcli.Runner, revision string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, fmt.Errorf("load state: %w", err)
	}
	encoded, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: revision, Path: File})
	if err != nil {
		return Document{}, fmt.Errorf("load state: %w", err)
	}
	doc, err := Decode(encoded)
	if err != nil {
		return Document{}, fmt.Errorf("load state %s: %w", revision, err)
	}
	// The document names the hash algorithm every object in it is written in,
	// so a record from another repository decodes cleanly and still describes
	// commits this one does not have. Comparing it against the repository is
	// what turns that into a refusal instead of a resume from nothing.
	if err := requireFormat(ctx, git, doc.ObjectFormat); err != nil {
		return Document{}, fmt.Errorf("load state %s: %w", revision, err)
	}
	entries, err := git.ListTree(ctx, revision)
	if err != nil {
		return Document{}, fmt.Errorf("load state %s: list record tree: %w", revision, err)
	}
	var mappingObject string
	for _, entry := range entries {
		switch entry.Path {
		case File:
		case MappingFile:
			mappingObject = entry.Object
		default:
			return Document{}, fmt.Errorf("load state %s: record tree contains unexpected path %s", revision, entry.Path)
		}
	}
	if mappingObject != "" {
		if mappingObject != doc.Mapping.Object {
			return Document{}, fmt.Errorf("load state %s: mapping tree object %s does not match document object %s", revision, mappingObject, doc.Mapping.Object)
		}
		mapping, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: revision, Path: MappingFile})
		if err != nil {
			return Document{}, fmt.Errorf("load state %s: read mapping: %w", revision, err)
		}
		if err := validateMappingEvidence(doc.Mapping, mapping); err != nil {
			return Document{}, fmt.Errorf("load state %s: %w", revision, err)
		}
	}
	return doc, nil
}

// LoadMapping reads and verifies the canonical staging index reachable from a
// state record. Legacy records that named a mapping blob without placing it in
// their tree report ErrMappingEvidenceMissing rather than fetching an unrelated
// object by name.
func LoadMapping(ctx context.Context, git *gitcli.Runner, revision string) ([]byte, *gomodmap.Index, error) {
	doc, err := Load(ctx, git, revision)
	if err != nil {
		return nil, nil, err
	}
	if doc.Mapping.Entries == 0 {
		return nil, gomodmap.NewIndex(), nil
	}
	entries, err := git.ListTree(ctx, revision)
	if err != nil {
		return nil, nil, fmt.Errorf("load state mapping %s: %w", revision, err)
	}
	if !slices.ContainsFunc(entries, func(entry gitcli.TreeEntry) bool {
		return entry.Path == MappingFile && entry.Object == doc.Mapping.Object
	}) {
		return nil, nil, fmt.Errorf("load state mapping %s: %w", revision, ErrMappingEvidenceMissing)
	}
	data, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: revision, Path: MappingFile})
	if err != nil {
		return nil, nil, fmt.Errorf("load state mapping %s: %w", revision, err)
	}
	if err := validateMappingEvidence(doc.Mapping, data); err != nil {
		return nil, nil, fmt.Errorf("load state mapping %s: %w", revision, err)
	}
	index, err := gomodmap.Decode(data)
	if err != nil {
		return nil, nil, fmt.Errorf("load state mapping %s: %w", revision, err)
	}
	return data, index, nil
}

// requireFormat reports a document whose object names belong to a repository
// with a different hash algorithm.
func requireFormat(ctx context.Context, git *gitcli.Runner, want gitcli.ObjectFormat) error {
	format, err := git.ObjectFormat(ctx)
	if err != nil {
		return err
	}
	if format != want {
		return fmt.Errorf("%w: the document records %s object names, the repository writes %s",
			ErrObjectFormat, string(want), string(format))
	}
	return nil
}

// checkParents reports a parent that is not a commit this repository holds
// under this document's hash algorithm.
//
// Three things are refused and each would produce a commit that reads back
// wrong rather than a call that fails. An abbreviation, or any name that is not
// the document's width, would let a sha1 name into a sha256 record: git resolves
// it or does not, and either way the state chain would name a parent in a
// notation the rest of the document does not use. A name for an object that is
// not here would produce a chain whose earlier records cannot be read. And an
// object that is here but is a tree or a blob would make the state history
// unwalkable at exactly the point a resume needs to walk it.
//
// The probe answers from the local object store only, which is what "do I hold
// this" means. A partial clone must not reach the network to decide whether a
// record it wrote earlier exists.
func checkParents(ctx context.Context, git *gitcli.Runner, parents []string, format gitcli.ObjectFormat) error {
	if len(parents) == 0 {
		return nil
	}
	width := format.HexLength()
	for i, parent := range parents {
		if err := checkObject(fmt.Sprintf("parent %d", i), parent, width); err != nil {
			return err
		}
	}
	infos, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: parents})
	if err != nil {
		return err
	}
	if len(infos) != len(parents) {
		return fmt.Errorf("probed %d parents, got %d answers", len(parents), len(infos))
	}
	for i, info := range infos {
		switch {
		case info.Missing:
			return fmt.Errorf("parent %d %s is not an object this repository holds", i, parents[i])
		case info.Type != "commit":
			return fmt.Errorf("parent %d %s is a %s, not a commit", i, parents[i], info.Type)
		}
	}
	return nil
}

// message renders the commit message for a stored record.
//
// It is derived from the document rather than taken from the caller, so the
// commit is a function of the record, its parents, and the two signatures, and
// nothing else. A caller supplied message would be one more input that two runs
// doing the same work could differ on.
//
// Neither body line has the shape of a trailer, which keeps the message free of
// provenance a reader could mistake for a claim about an upstream commit. A
// state commit is not a transformed commit and must not be mapped back to one.
func message(doc Document) string {
	return "soapbox state\n\nschema " + strconv.Itoa(doc.Schema) + "\ndigest " + doc.Digest + "\n"
}
