// Package state records what a resumable run already did, as a Git object that
// no ref points at.
//
// A backfill of tens of thousands of upstream commits does not finish in one
// process. It stops, on a timeout, on a token expiry, or on a
// chunk boundary it was asked to stop at, and the next process has to know
// exactly where the last one got to. That knowledge cannot live in a file on a
// runner that is destroyed between jobs, so it lives in the destination
// repository as an object, and the record is built to be compared rather than
// merely read back.
//
// Three properties make it comparable. The document holds no time, no location,
// and no credential: no timestamp field exists, a URL or an absolute path is
// refused rather than stored, and there is nothing a secret could be written
// into that would survive validation. Every list is in one canonical order, so
// two runs that did the same work render the same bytes whatever order they
// discovered it in. And the document carries a digest over itself, so a record
// that was edited after it was written is a refusal rather than a resume from a
// position nobody computed.
//
// Nothing here updates a ref. Store writes a blob, a tree, and a commit, and
// returns their names; deciding that some branch should point at that commit is
// a publication, and publications are gated somewhere else. That separation is
// what lets a run persist its progress without any of it becoming visible to a
// module consumer.
package state

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Schema is the document version this engine writes and the only one it reads.
//
// It is checked before anything else is decoded. A document from a future
// engine is refused by version rather than by whichever field happened to be
// added, because "unknown field mappingIndex" tells an operator nothing about
// what to do and "schema 2, this engine writes 1" tells them to upgrade.
const Schema = 1

// digestPrefix and digestDigits are the exact form every digest in this package
// takes. The engine has one producer of these values and it emits precisely
// this: "sha256:" followed by the lower case hex encoding of a sha256 sum.
// Accepting anything looser would let a value that is not a digest at all be
// recorded as the identity of a document.
const (
	digestPrefix = "sha256:"
	digestDigits = 64
)

// Ref namespaces. Which namespace a ref lives in is what decides who can see
// it, so the split is stated once here and every rule below is expressed
// against it.
const (
	// ProgressNamespace holds a gated backfill's chunks.
	//
	// It is deliberately outside the two consumer namespaces. Nothing under it
	// is a ref a module user resolves: the module proxy does not serve it, go
	// get does not see it, and it is not among the branches and tags someone
	// browsing the repository reads. That is what lets a long backfill record
	// where it got to between chunks while the refs a consumer reads stay where
	// the last fully gated run left them.
	ProgressNamespace = "refs/soapbox/progress/"
	// BranchNamespace holds the consumer branches.
	BranchNamespace = "refs/heads/"
	// TagNamespace holds the consumer tags.
	TagNamespace = "refs/tags/"
)

// Refusals. Each names a verdict a caller has to act on rather than a field
// that happened to be empty.
var (
	// ErrSchema reports a document written under a version this engine does not
	// read.
	ErrSchema = errors.New("state schema is not the one this engine writes")
	// ErrDigest reports a document whose recorded digest does not match its
	// content, which means it was edited after it was written.
	ErrDigest = errors.New("state digest does not match the document")
	// ErrNotCanonical reports bytes that decode to a valid document but are not
	// the bytes this engine renders that document as. Two spellings of one
	// record would be two blobs with two object names, and a resume comparing
	// names would report a change that is not one.
	ErrNotCanonical = errors.New("state document is not in its canonical rendering")
	// ErrTrailing reports bytes following the document. A record with a second
	// value appended is a record two readers could disagree about.
	ErrTrailing = errors.New("state document is followed by trailing data")
	// ErrObjectFormat reports an object name whose width does not match the
	// document's hash algorithm. A document mixing widths describes two
	// repositories, and only one of them is the one being resumed.
	ErrObjectFormat = errors.New("object name width does not match the document's object format")
	// ErrLocation reports a value carrying a URL, a credential, or an absolute
	// path. The record is published into a repository and read from failure
	// artifacts, so anything that could name a machine or authorise access to
	// one is refused at the boundary rather than redacted afterwards.
	ErrLocation = errors.New("state must carry no URL, credential, or absolute path")
	// ErrNamespace reports a ref recorded outside the namespace its role lives
	// in, such as a backfill's progress held on a consumer branch.
	ErrNamespace = errors.New("state ref is outside the namespace its role lives in")
	// ErrDuplicate reports two entries claiming the same identity.
	ErrDuplicate = errors.New("state holds a duplicate entry")
	// ErrUnsorted reports a list that is not in its canonical order. Order is
	// part of the bytes the digest covers, so an out of order list is a document
	// whose digest describes a different rendering of the same facts.
	ErrUnsorted = errors.New("state list is not in its canonical order")
	// ErrCorrespondence reports a source commit mapped onto two destination
	// commits, or a destination commit claimed by two sources.
	ErrCorrespondence = errors.New("source and destination commits do not correspond one to one")
	// ErrImmutable reports a field that identifies the record changing between
	// two documents, such as the anchor the whole published history hangs from.
	ErrImmutable = errors.New("state field is immutable and changed")
	// ErrRewind reports a cursor moving to a commit that does not descend from
	// the recorded one, or a count going backwards.
	ErrRewind = errors.New("state cursor moved backwards")
	// ErrDropped reports an entry present in the previous document and absent
	// from the next one.
	ErrDropped = errors.New("state entry was dropped")
	// ErrTagMoved reports a published tag pointing somewhere new. A module
	// consumer and the module proxy have already resolved that tag, so moving it
	// changes code that was already fetched.
	ErrTagMoved = errors.New("published tag moved")
)

// Kind classifies a published destination ref.
type Kind string

const (
	// KindBranch is a consumer branch, which advances by fast forward only.
	KindBranch Kind = "branch"
	// KindTag is a consumer tag, which never moves once observed.
	KindTag Kind = "tag"
)

// refPrefix reports the ref namespace a kind must live in, and whether the kind
// is one this package models.
//
// The namespace is checked rather than assumed because the kind is what decides
// whether a merge may advance the entry. A tag recorded as a branch would be
// allowed to move, and the record itself is the only place that mistake can be
// caught.
func (k Kind) refPrefix() (string, bool) {
	switch k {
	case KindBranch:
		return BranchNamespace, true
	case KindTag:
		return TagNamespace, true
	default:
		return "", false
	}
}

type (
	// Destination identifies the repository a run publishes into.
	//
	// It names the repository and the module rather than a remote, because a
	// remote is a location: the same repository is reachable over https, over
	// ssh, and through a mirror, and a record that pinned one of them would
	// compare unequal to the same run performed through another.
	Destination struct {
		// Repository is the owner and name, such as enj/rbac_authorizer.
		Repository string `json:"repository"`
		// Module is the generated module path, such as monis.app/kk/rbac.
		Module string `json:"module"`
	}

	// Anchor is the source commit every published commit descends from.
	//
	// It is fixed at setup and never changes afterwards. Moving it would rewrite
	// the base of history that consumers have already fetched, so a merge
	// refuses a document whose anchor differs at all.
	Anchor struct {
		// Source is the source commit the transformed history starts from.
		Source string `json:"source"`
		// Ref is the fully qualified source ref the anchor was proved against,
		// such as refs/tags/v1.36.1.
		Ref string `json:"ref"`
	}

	// Epoch is the current output affecting profile and where its history is
	// grafted on.
	//
	// A profile change re-derives every transformed commit, so the destination
	// history it produces is a new line rather than a continuation. Recording
	// which profile produced the current line, and where that line was attached,
	// is what lets a merge tell an ordinary advance from a re-graft and apply
	// the right rule to each.
	Epoch struct {
		// Profile is the digest of the output affecting profile, in the form
		// "sha256:" followed by 64 lower case hexadecimal characters.
		Profile string `json:"profile"`
		// Source is the source commit this epoch begins transforming at.
		Source string `json:"source"`
		// Destination is the destination commit this epoch's history is grafted
		// onto. It is empty for the first epoch, which has nothing to attach to.
		Destination string `json:"destination"`
	}

	// Cursor is how far one tracked source ref has been transformed.
	Cursor struct {
		// Ref is the fully qualified source ref, such as refs/heads/master.
		Ref string `json:"ref"`
		// Source is the last source commit transformed on this ref.
		Source string `json:"source"`
		// Destination is the commit that source commit became.
		Destination string `json:"destination"`
	}

	// Mapping is the staging version index, referenced rather than inlined.
	//
	// The index maps source commits to the dependency versions resolved for
	// them, and it grows to one entry per transformed commit. Carrying it inside
	// the document would make every resume read and rewrite the whole cache, so
	// the document holds the blob it lives in and a digest of its content. The
	// count is what makes the reference checkable: the index is append only, so
	// an unchanged count with a changed digest is a rewritten cache rather than
	// an extended one.
	Mapping struct {
		// Digest is the content digest of the index, in the form "sha256:"
		// followed by 64 lower case hexadecimal characters.
		Digest string `json:"digest"`
		// Object is the blob object the index is stored in.
		Object string `json:"object"`
		// Entries is the number of source commits the index resolves.
		Entries int `json:"entries"`
	}

	// Track is one gated backfill in progress.
	//
	// A long backfill publishes progress refs between chunks and leaves consumer
	// refs where they were, so the work in flight is visible to a resume without
	// being visible to a module consumer. The track is the record of that work.
	Track struct {
		// Name is the track identifier, which is the last component of Ref.
		Name string `json:"name"`
		// Ref is the progress ref holding the chunk. It is always
		// ProgressNamespace followed by Name.
		Ref string `json:"ref"`
		// Source is the source commit the last completed chunk stopped at. It is
		// empty exactly when Done is zero.
		Source string `json:"source"`
		// Destination is the commit the progress ref holds. It is empty exactly
		// when Done is zero.
		Destination string `json:"destination"`
		// Done is the number of source commits this track has transformed.
		Done int `json:"done"`
		// Total is the number of source commits the track must cover.
		Total int `json:"total"`
	}

	// Published is one destination ref as it was last observed.
	//
	// It records what the remote held rather than what a run intends, because
	// its purpose is to catch a ref that changed underneath the engine. A tag
	// that moved and a branch that stopped being a fast forward are both
	// failures that only this comparison can see.
	Published struct {
		// Ref is the fully qualified destination ref.
		Ref string `json:"ref"`
		// Kind classifies the ref and decides whether it may advance.
		Kind Kind `json:"kind"`
		// Object is the object the ref was observed pointing at.
		Object string `json:"object"`
		// Source is the source commit the ref was published from.
		Source string `json:"source"`
	}

	// Engine records what produced the document.
	//
	// A record written by a different engine version, or formatted by a
	// different toolchain, may not be byte reproducible by the reader, and the
	// reader has to be able to say so rather than compare digests and conclude
	// that the work was wrong.
	Engine struct {
		// Version is the engine build version.
		Version string `json:"version"`
		// Toolchain is the exact Go toolchain the run pinned, such as go1.26.5.
		Toolchain string `json:"toolchain"`
	}

	// Document is one complete, self describing state record.
	//
	// Field order is the encoded order and the digest covers it, so this
	// declaration is part of the format rather than a presentation choice.
	Document struct {
		// Schema is the document version, which must equal Schema.
		Schema int `json:"schema"`
		// ObjectFormat is the hash algorithm every object name is written in.
		ObjectFormat gitcli.ObjectFormat `json:"objectFormat"`
		// Destination identifies the repository and module.
		Destination Destination `json:"destination"`
		// Anchor is the immutable base of published history.
		Anchor Anchor `json:"anchor"`
		// Epoch is the current profile and its graft point.
		Epoch Epoch `json:"epoch"`
		// Cursors are the tracked ref positions, sorted by ref.
		Cursors []Cursor `json:"cursors"`
		// Mapping references the staging version index.
		Mapping Mapping `json:"mapping"`
		// Tracks are the backfills in progress, sorted by name.
		Tracks []Track `json:"tracks"`
		// Published are the observed destination refs, sorted by ref.
		Published []Published `json:"published"`
		// Engine records the producer.
		Engine Engine `json:"engine"`
		// Digest covers every other field. It is excluded from its own input,
		// which is what lets a resume name a record by digest and prove the
		// record it read is the one that was written.
		Digest string `json:"digest"`
	}
)

// New returns the canonical form of a document: lists sorted, digest computed,
// and every invariant checked.
//
// The lists are sorted here rather than required in order, because the caller
// discovers cursors and tracks in whatever order the repository answered in and
// a digest that depended on that order would make two identical runs disagree.
// Validate then requires the order, so a document that reaches a reader
// unsorted was edited after New produced it.
//
// A document that already carries a digest is refused rather than re-digested.
// Recomputing would turn "these bytes were modified" into "these bytes are
// fine", which is the one thing the digest exists to prevent.
func New(doc Document) (Document, error) {
	if doc.Digest != "" {
		return Document{}, fmt.Errorf("%w: a new document must not arrive with a digest, it carries %q",
			ErrDigest, doc.Digest)
	}
	canonical := doc.Clone()
	slices.SortFunc(canonical.Cursors, func(a, b Cursor) int { return strings.Compare(a.Ref, b.Ref) })
	slices.SortFunc(canonical.Tracks, func(a, b Track) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(canonical.Published, func(a, b Published) int { return strings.Compare(a.Ref, b.Ref) })

	// Everything except the digest is checked first, so a document that is
	// wrong is reported as wrong rather than as carrying an empty digest.
	if err := canonical.validateContent(); err != nil {
		return Document{}, err
	}
	digest, err := canonical.computeDigest()
	if err != nil {
		return Document{}, err
	}
	canonical.Digest = digest
	return canonical, nil
}

// Clone returns a copy sharing no slice with the receiver, with every list
// present.
//
// Every constructor and every accessor goes through it. A document handed out
// with its backing arrays shared would let a caller change a record after it
// was validated and digested, and the digest would still verify because it
// covers the rendering rather than the memory.
func (d Document) Clone() Document {
	clone := d
	clone.Cursors = cloneList(d.Cursors)
	clone.Tracks = cloneList(d.Tracks)
	clone.Published = cloneList(d.Published)
	return clone
}

// cloneList copies a list and renders an absent one as an empty one.
//
// A missing list and an empty list say the same thing, so they have to become
// the same value here. Left alone they would render as null and as [], which is
// two blobs with two object names for one record, and a resume comparing names
// would see a change where there is none.
func cloneList[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return slices.Clone(items)
}

// Validate reports a document that is not one this engine would have written.
func (d Document) Validate() error {
	if err := d.validateContent(); err != nil {
		return err
	}
	if d.Digest == "" {
		return fmt.Errorf("%w: the document carries no digest", ErrDigest)
	}
	computed, err := d.computeDigest()
	if err != nil {
		return err
	}
	if computed != d.Digest {
		return fmt.Errorf("%w: it carries %s and digests to %s", ErrDigest, d.Digest, computed)
	}
	return nil
}

// validateContent checks everything the digest covers.
//
// It is separate from Validate because New has to check the content of a
// document whose digest does not exist yet, and running one set of rules from
// both directions is what keeps a document New accepts from being a document
// Validate rejects.
func (d Document) validateContent() error {
	if d.Schema != Schema {
		return fmt.Errorf("%w: the document declares schema %d, this engine writes %d",
			ErrSchema, d.Schema, Schema)
	}
	width := d.ObjectFormat.HexLength()
	if width == 0 {
		return fmt.Errorf("%w: %q is not a hash algorithm git names",
			ErrObjectFormat, string(d.ObjectFormat))
	}
	if err := d.validateIdentity(); err != nil {
		return err
	}
	if err := d.validateAnchor(width); err != nil {
		return err
	}
	if err := d.validateEpoch(width); err != nil {
		return err
	}
	if err := d.validateCursors(width); err != nil {
		return err
	}
	if err := d.validateMapping(width); err != nil {
		return err
	}
	if err := d.validateTracks(width); err != nil {
		return err
	}
	if err := d.validatePublished(width); err != nil {
		return err
	}
	if err := d.validateEngine(); err != nil {
		return err
	}
	// The correspondence spans three lists, so it runs once they have each been
	// checked on their own. Reporting "these two entries disagree" is only
	// meaningful after both are known to hold object names at all.
	return d.validateCorrespondence()
}

// validateIdentity checks the destination the record belongs to.
func (d Document) validateIdentity() error {
	if err := checkOpaque("destination repository", d.Destination.Repository); err != nil {
		return err
	}
	owner, name, ok := strings.Cut(d.Destination.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("destination repository %q must be an owner and a name, such as %q",
			d.Destination.Repository, "enj/rbac_authorizer")
	}
	if err := checkOpaque("destination module", d.Destination.Module); err != nil {
		return err
	}
	if !strings.Contains(d.Destination.Module, "/") || strings.HasSuffix(d.Destination.Module, "/") {
		return fmt.Errorf("destination module %q must be a module path, such as %q",
			d.Destination.Module, "monis.app/kk/rbac_authorizer")
	}
	return nil
}

// validateAnchor checks the immutable base of published history.
func (d Document) validateAnchor(width int) error {
	if err := checkObject("anchor source", d.Anchor.Source, width); err != nil {
		return err
	}
	return checkRef("anchor ref", d.Anchor.Ref)
}

// validateEpoch checks the current profile and its graft point.
func (d Document) validateEpoch(width int) error {
	if err := checkDigest("epoch profile", d.Epoch.Profile); err != nil {
		return err
	}
	if err := checkObject("epoch source", d.Epoch.Source, width); err != nil {
		return err
	}
	// The first epoch has no destination parent, because there is no published
	// history for it to attach to yet. Every later one does.
	if d.Epoch.Destination == "" {
		return nil
	}
	return checkObject("epoch destination", d.Epoch.Destination, width)
}

// validateCursors checks the tracked ref positions.
func (d Document) validateCursors(width int) error {
	if err := checkOrder("cursor", d.Cursors, func(c Cursor) string { return c.Ref }); err != nil {
		return err
	}
	for _, cursor := range d.Cursors {
		if err := checkRef("cursor ref", cursor.Ref); err != nil {
			return err
		}
		if err := checkObject("cursor source", cursor.Source, width); err != nil {
			return err
		}
		if err := checkObject("cursor destination", cursor.Destination, width); err != nil {
			return err
		}
	}
	return nil
}

// image is one claim the document makes about what a source commit became.
type image struct {
	source string
	became string
	// label names where the claim came from, so a contradiction reports the two
	// entries that disagree rather than only the commits.
	label string
}

// images collects every source to destination claim in the document, in a fixed
// order so a contradiction is reported the same way every time.
//
// The epoch is deliberately absent. Its destination is the commit this epoch's
// history is grafted onto, not the image of its source, so including it would
// assert a correspondence the engine never claimed. A track that has done
// nothing is absent for the same reason: it has produced no commit yet.
func (d Document) images() []image {
	claims := make([]image, 0, len(d.Cursors)+len(d.Tracks)+len(d.Published))
	for _, cursor := range d.Cursors {
		claims = append(claims, image{cursor.Source, cursor.Destination, "cursor " + cursor.Ref})
	}
	for _, track := range d.Tracks {
		if track.Done == 0 {
			continue
		}
		claims = append(claims, image{track.Source, track.Destination, "track " + track.Name})
	}
	for _, entry := range d.Published {
		// A tag's Object is an annotated tag object, not the commit the source
		// became. Including it would assert that one source commit maps to both
		// a branch commit and a tag object, which is a correspondence conflict
		// by construction. Tags are excluded from the source-to-destination
		// correspondence check; their immutability is enforced separately by
		// Merge (which refuses any change to a tag's Object or Source) and by
		// the publication plan (which refuses a tag that moved on the remote).
		if entry.Kind == KindTag {
			continue
		}
		claims = append(claims, image{entry.Source, entry.Object, "published " + entry.Ref})
	}
	return claims
}

// validateCorrespondence checks that the whole document describes one mapping
// from source commits to the commits they became.
//
// The check spans the cursors, the tracks, and the published refs together
// rather than each list separately, because they are three views of one
// transform. A cursor saying a source became one commit while a published tag
// says it became another is a contradiction whichever list is read first, and
// resuming from either answer would build history the other half of the record
// denies.
//
// Agreement is what is required, not uniqueness. A release branch, the tag cut
// from it, and the cursor that produced both legitimately carry the same pair.
func (d Document) validateCorrespondence() error {
	forward := make(map[string]image)
	backward := make(map[string]image)
	for _, claim := range d.images() {
		if seen, ok := forward[claim.source]; ok && seen.became != claim.became {
			return fmt.Errorf("%w: %s says source %s became %s, %s says it became %s",
				ErrCorrespondence, seen.label, claim.source, seen.became, claim.label, claim.became)
		}
		if seen, ok := backward[claim.became]; ok && seen.source != claim.source {
			return fmt.Errorf("%w: %s says %s came from %s, %s says it came from %s",
				ErrCorrespondence, seen.label, claim.became, seen.source, claim.label, claim.source)
		}
		forward[claim.source] = claim
		backward[claim.became] = claim
	}

	// Separate tag-only correspondence: two tags must not share one source or
	// one object. Tags are excluded from the branch correspondence above because
	// a tag object is not the commit the source became, but they still must be
	// one-to-one among themselves.
	tagForward := make(map[string]image)
	tagBackward := make(map[string]image)
	for _, entry := range d.Published {
		if entry.Kind != KindTag {
			continue
		}
		claim := image{entry.Source, entry.Object, "published tag " + entry.Ref}
		if seen, ok := tagForward[claim.source]; ok && seen.became != claim.became {
			return fmt.Errorf("%w: %s says source %s became %s, %s says it became %s",
				ErrCorrespondence, seen.label, claim.source, seen.became, claim.label, claim.became)
		}
		if seen, ok := tagBackward[claim.became]; ok && seen.source != claim.source {
			return fmt.Errorf("%w: %s says %s came from %s, %s says it came from %s",
				ErrCorrespondence, seen.label, claim.became, seen.source, claim.label, claim.source)
		}
		tagForward[claim.source] = claim
		tagBackward[claim.became] = claim
	}

	return nil
}

// validateMapping checks the reference to the staging version index.
//
// An index with no entries has no content and therefore no blob, and an index
// with entries has both. Stating it as a biconditional refuses the two shapes
// that would otherwise be accepted and mean nothing: a digest of an index that
// resolves nothing, and a count of entries nobody can read.
func (d Document) validateMapping(width int) error {
	if d.Mapping.Entries < 0 {
		return fmt.Errorf("mapping entries %d must not be negative", d.Mapping.Entries)
	}
	if d.Mapping.Entries == 0 {
		if d.Mapping.Digest != "" || d.Mapping.Object != "" {
			return fmt.Errorf("mapping resolves no source commit but carries digest %q and object %q",
				d.Mapping.Digest, d.Mapping.Object)
		}
		return nil
	}
	if err := checkDigest("mapping digest", d.Mapping.Digest); err != nil {
		return err
	}
	return checkObject("mapping object", d.Mapping.Object, width)
}

// validateTracks checks the backfills in progress.
func (d Document) validateTracks(width int) error {
	if err := checkOrder("track", d.Tracks, func(t Track) string { return t.Name }); err != nil {
		return err
	}
	for _, track := range d.Tracks {
		if err := checkOpaque("track name", track.Name); err != nil {
			return err
		}
		if strings.Contains(track.Name, "/") {
			return fmt.Errorf("track name %q must be one ref component and must not contain %q",
				track.Name, "/")
		}
		if err := checkRef("track ref", track.Ref); err != nil {
			return err
		}
		// The ref is required to be exactly the one the namespace and the name
		// determine, rather than merely to end in the name. Two things follow
		// from that and both matter. A backfill's chunks cannot be recorded on
		// a consumer branch or tag, where a module user would resolve work that
		// has not been gated yet. And because the names are unique and each is
		// one component, no two tracks can name one ref.
		if want := ProgressNamespace + track.Name; track.Ref != want {
			return fmt.Errorf("%w: track %q is held by %q, want %q",
				ErrNamespace, track.Name, track.Ref, want)
		}
		switch {
		case track.Total <= 0:
			return fmt.Errorf("track %q must cover at least one commit, it covers %d", track.Name, track.Total)
		case track.Done < 0:
			return fmt.Errorf("track %q has done %d commits, which must not be negative", track.Name, track.Done)
		case track.Done > track.Total:
			return fmt.Errorf("track %q has done %d of %d commits", track.Name, track.Done, track.Total)
		}
		// A track that has transformed nothing has produced nothing to point at,
		// and a track that has transformed something must say what it produced.
		if track.Done == 0 {
			if track.Source != "" || track.Destination != "" {
				return fmt.Errorf("track %q has done no commits but records source %q and destination %q",
					track.Name, track.Source, track.Destination)
			}
			continue
		}
		if err := checkObject("track source", track.Source, width); err != nil {
			return err
		}
		if err := checkObject("track destination", track.Destination, width); err != nil {
			return err
		}
	}
	return nil
}

// validatePublished checks the observed destination refs.
func (d Document) validatePublished(width int) error {
	if err := checkOrder("published", d.Published, func(p Published) string { return p.Ref }); err != nil {
		return err
	}
	for _, entry := range d.Published {
		prefix, ok := entry.Kind.refPrefix()
		if !ok {
			return fmt.Errorf("published ref %q has kind %q, want %q or %q",
				entry.Ref, string(entry.Kind), string(KindBranch), string(KindTag))
		}
		if err := checkRef("published ref", entry.Ref); err != nil {
			return err
		}
		if !strings.HasPrefix(entry.Ref, prefix) {
			return fmt.Errorf("%w: published %s %q must live under %q",
				ErrNamespace, string(entry.Kind), entry.Ref, prefix)
		}
		if err := checkObject("published object", entry.Object, width); err != nil {
			return err
		}
		if err := checkObject("published source", entry.Source, width); err != nil {
			return err
		}
	}
	return nil
}

// validateEngine checks what produced the document.
func (d Document) validateEngine() error {
	if err := checkOpaque("engine version", d.Engine.Version); err != nil {
		return err
	}
	if err := checkOpaque("engine toolchain", d.Engine.Toolchain); err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(d.Engine.Toolchain, "go")
	if !ok || rest == "" || rest[0] < '0' || rest[0] > '9' {
		return fmt.Errorf("engine toolchain %q must be a Go toolchain name, such as %q",
			d.Engine.Toolchain, "go1.26.5")
	}
	return nil
}

// checkOrder reports a list that is out of order or holds two entries under one
// key.
func checkOrder[T any](kind string, items []T, key func(T) string) error {
	for i := 1; i < len(items); i++ {
		previous, current := key(items[i-1]), key(items[i])
		switch {
		case previous == current:
			return fmt.Errorf("%w: two %s entries carry %q", ErrDuplicate, kind, current)
		case previous > current:
			return fmt.Errorf("%w: %s entry %q follows %q", ErrUnsorted, kind, current, previous)
		}
	}
	return nil
}

// checkObject reports an object name that is not the width this document's hash
// algorithm produces.
//
// The width is checked against the document rather than against git's two
// legal widths, because a document is about one repository. A sha1 name in a
// sha256 record is not a shorter name for the same object, it is a name from
// somewhere else, and resuming from it would transform the wrong history.
func checkObject(field, value string, width int) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) != width {
		return fmt.Errorf("%w: %s %q is %d characters, want %d",
			ErrObjectFormat, field, value, len(value), width)
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("%s %q must be lower case hexadecimal", field, value)
		}
	}
	return nil
}

// checkDigest reports a value that is not the canonical digest form.
func checkDigest(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	digits, ok := strings.CutPrefix(value, digestPrefix)
	if !ok {
		return fmt.Errorf("%s %q must begin with %q", field, value, digestPrefix)
	}
	if len(digits) != digestDigits {
		return fmt.Errorf("%s %q carries %d digest characters, want %d",
			field, value, len(digits), digestDigits)
	}
	for _, r := range digits {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("%s %q must be lower case hexadecimal", field, value)
		}
	}
	return nil
}

// checkRef reports a ref that is not fully qualified or that git would not
// accept.
//
// Fully qualified is required because the record is read by a process that has
// no working tree and no notion of a current branch. A short name would be
// resolved against whatever the reader happened to have, which is exactly the
// dependence on the machine this record exists to avoid.
func checkRef(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("%s %q must be fully qualified, such as %q", field, name, "refs/heads/main")
	}
	if err := gitcli.ValidateRefName(name); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// checkOpaque reports a value that would carry a location or a credential into
// the record.
//
// The record is committed into the destination repository and attached to
// failure artifacts, so the guard is at the boundary rather than at the point
// of rendering: a value that never enters the document cannot leak out of one.
// Refusing whitespace comes with that, because a value holding a line break
// could otherwise forge a second field in any text rendering of the record.
func checkOpaque(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%s %q must not contain whitespace or a control character", field, value)
		}
	}
	switch {
	case strings.Contains(value, "://"):
		return fmt.Errorf("%w: %s %q carries a URL", ErrLocation, field, value)
	case strings.Contains(value, "@"):
		return fmt.Errorf("%w: %s %q carries a credential or an scp style remote", ErrLocation, field, value)
	case strings.HasPrefix(value, "/"), strings.Contains(value, `\`), hasDriveLetter(value):
		return fmt.Errorf("%w: %s %q carries an absolute path", ErrLocation, field, value)
	}
	return nil
}

// hasDriveLetter reports a Windows absolute path, which a leading slash test
// alone would not catch.
func hasDriveLetter(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	c := value[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
