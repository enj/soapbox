package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Ancestry answers whether one commit is reachable from another.
//
// It is supplied by the caller rather than taken as a repository, because the
// question is about a repository this package has deliberately not been given.
// A merge decides whether a recorded position may advance to a proposed one,
// and the only way to know is to ask the graph; taking the answerer as a
// parameter keeps the rules here testable against a graph a test constructs and
// keeps this package from deciding which repository to consult.
//
// *gitcli.Runner satisfies it.
type Ancestry interface {
	// IsAncestor reports whether ancestor is reachable from descendant. A commit
	// is its own ancestor.
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
}

// Merge accepts next as the successor of prev, or reports why it is not.
//
// next is a complete document rather than a patch. That is the more demanding
// shape and it is chosen for what it makes expressible: a caller can propose
// dropping a cursor, closing a track, or removing a published tag, and each of
// those is a distinct refusal here. Under a patch shape the removals would be
// inexpressible rather than refused, and "the engine cannot say that" is a much
// weaker guarantee than "the engine says no". A caller building next from
// prev.Clone() with the fields it advanced is doing exactly what a delta would
// have meant. A next that carries no digest is canonicalised on the way in, so
// the accepted successor comes back sorted, checked, and digested.
//
// The rules are:
//
//   - The destination and the anchor never change. They identify the record,
//     and history already published hangs from the anchor.
//   - Within one epoch, every cursor advances only to a descendant. Across an
//     epoch change the destination side is re-derived, so only the source side
//     is required to move forward.
//   - Nothing recorded is dropped, except a track that has finished.
//   - A published branch advances only by fast forward, and a published tag
//     never moves and is never removed. A consumer and the module proxy have
//     already resolved that tag.
//
// The ancestry checker is consulted only where the answer could differ, so a
// merge that advanced nothing asks the repository nothing.
// Merge uses one ancestry graph for both source and destination positions. It is
// retained for callers whose fixture or repository contains both histories.
func Merge(ctx context.Context, prev, next Document, ancestry Ancestry) (Document, error) {
	return MergeWith(ctx, prev, next, ancestry, ancestry)
}

// MergeWith checks source and destination advances against their respective
// repositories. Soapbox's source cache and generated destination never share an
// object database, so conflating these graphs would make a valid cursor or epoch
// advance fail with a missing-object error.

func MergeWith(ctx context.Context, prev, next Document, sourceAncestry, destinationAncestry Ancestry) (Document, error) {
	// The check is at the top rather than left to the first ancestry call,
	// because a merge that happens to need no ancestry call would otherwise
	// accept a successor after its caller had already given up on the work.
	if err := ctx.Err(); err != nil {
		return Document{}, fmt.Errorf("merge state: %w", err)
	}
	if sourceAncestry == nil || destinationAncestry == nil {
		return Document{}, errors.New("merge state: source and destination ancestry checkers are required")
	}
	if err := prev.Validate(); err != nil {
		return Document{}, fmt.Errorf("merge state: previous document: %w", err)
	}
	candidate, err := canonical(next)
	if err != nil {
		return Document{}, fmt.Errorf("merge state: next document: %w", err)
	}

	// The schema is not compared, because both documents have already been
	// validated against the one constant this engine writes and so cannot
	// differ. The object format can, and does have to be checked.
	if err := immutable("object format", string(prev.ObjectFormat), string(candidate.ObjectFormat)); err != nil {
		return Document{}, err
	}
	if err := immutable("destination repository", prev.Destination.Repository, candidate.Destination.Repository); err != nil {
		return Document{}, err
	}
	if err := immutable("destination module", prev.Destination.Module, candidate.Destination.Module); err != nil {
		return Document{}, err
	}
	if err := immutable("anchor source", prev.Anchor.Source, candidate.Anchor.Source); err != nil {
		return Document{}, err
	}
	if err := immutable("anchor ref", prev.Anchor.Ref, candidate.Anchor.Ref); err != nil {
		return Document{}, err
	}

	m := merger{source: sourceAncestry, destination: destinationAncestry, sameEpoch: prev.Epoch.Profile == candidate.Epoch.Profile}
	if err := m.epoch(ctx, prev.Epoch, candidate.Epoch); err != nil {
		return Document{}, err
	}
	if err := m.cursors(ctx, prev.Cursors, candidate.Cursors); err != nil {
		return Document{}, err
	}
	if err := m.mapping(prev.Mapping, candidate.Mapping); err != nil {
		return Document{}, err
	}
	if err := m.tracks(ctx, prev.Tracks, candidate.Tracks); err != nil {
		return Document{}, err
	}
	if err := m.published(ctx, prev.Published, candidate.Published); err != nil {
		return Document{}, err
	}
	return candidate, nil
}

// canonical returns the document a merge compares against.
//
// A candidate with no digest is one a caller just built, so it is sorted,
// checked, and digested here. A candidate that already carries one has been
// through that already and is only verified, because re-digesting it would
// silently repair a record that was modified after it was written.
func canonical(doc Document) (Document, error) {
	if doc.Digest == "" {
		return New(doc)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc.Clone(), nil
}

// merger carries the two things every rule below needs: who answers ancestry
// questions, and whether the destination history was re-derived.
type merger struct {
	source      Ancestry
	destination Ancestry
	// sameEpoch reports that both documents were produced by the same output
	// affecting profile. When it is false the destination commits in next were
	// re-derived from scratch and grafted onto a new parent, so they are a new
	// line of history rather than a continuation of the recorded one, and
	// requiring descent from the old line would refuse every profile change.
	sameEpoch bool
}

// epoch checks the profile and its graft point.
//
// An unchanged profile with a changed graft point is refused outright. The
// profile is the epoch's identity, so moving where its history attaches without
// changing that identity would rewrite commits already published under it while
// still claiming they were produced the same way.
func (m merger) epoch(ctx context.Context, prev, next Epoch) error {
	if m.sameEpoch {
		if err := immutable("epoch source", prev.Source, next.Source); err != nil {
			return err
		}
		return immutable("epoch destination", prev.Destination, next.Destination)
	}
	if err := m.sourceDescends(ctx, "epoch source", prev.Source, next.Source); err != nil {
		return err
	}
	// A new epoch grafts onto the destination history the previous one left
	// behind, so its parent has to descend from the previous parent even though
	// the commits between them were re-derived.
	return m.destinationRawDescends(ctx, "epoch destination", prev.Destination, next.Destination)
}

// cursors checks the tracked ref positions.
func (m merger) cursors(ctx context.Context, prev, next []Cursor) error {
	index := make(map[string]Cursor, len(next))
	for _, cursor := range next {
		index[cursor.Ref] = cursor
	}
	for _, before := range prev {
		after, ok := index[before.Ref]
		if !ok {
			return fmt.Errorf("%w: cursor %q is recorded at %s and absent from the next document",
				ErrDropped, before.Ref, before.Source)
		}
		what := "cursor " + before.Ref
		if err := m.sourceDescends(ctx, what+" source", before.Source, after.Source); err != nil {
			return err
		}
		if err := m.destinationDescends(ctx, what+" destination", before.Destination, after.Destination); err != nil {
			return err
		}
	}
	return nil
}

// mapping checks the staging version index reference.
//
// The index is append only, so its entry count never falls, and the same number
// of entries is the same set of source commits and therefore the same content: a
// count that stayed put while the digest moved is a cache that was rewritten
// rather than extended, and the versions a published commit was built against
// would no longer be the ones the record resolves.
//
// The second rule is that the digest and the blob determine each other. The
// digest is of the index's content and the blob holds exactly that content, so
// one digest cannot be stored in two blobs and one blob cannot digest to two
// values. Either would mean the reference no longer identifies what it points
// at, and unlike the count rule this holds however much the index grew.
func (m merger) mapping(prev, next Mapping) error {
	if next.Entries < prev.Entries {
		return fmt.Errorf("%w: the mapping index resolves %d source commits, it resolved %d",
			ErrRewind, next.Entries, prev.Entries)
	}
	// An index resolving nothing has no digest and no blob, so there is no
	// pairing to compare the next one against.
	if prev.Entries == 0 {
		return nil
	}
	if next.Digest == prev.Digest {
		if next.Object != prev.Object {
			return fmt.Errorf("%w: the mapping index digests to %s and is stored in %s, that digest was stored in %s",
				ErrCorrespondence, next.Digest, next.Object, prev.Object)
		}
		if next.Entries != prev.Entries {
			return fmt.Errorf("%w: the mapping index digests to %s and resolves %d source commits, that digest resolved %d",
				ErrCorrespondence, next.Digest, next.Entries, prev.Entries)
		}
		return nil
	}
	if next.Object == prev.Object {
		return fmt.Errorf("%w: the mapping index is stored in %s and digests to %s, that blob digested to %s",
			ErrCorrespondence, next.Object, next.Digest, prev.Digest)
	}
	if next.Entries == prev.Entries {
		return fmt.Errorf("%w: the mapping index still resolves %d source commits but digests to %s, it digested to %s",
			ErrImmutable, next.Entries, next.Digest, prev.Digest)
	}
	return nil
}

// tracks checks the backfills in progress.
//
// A finished track may disappear, because that is what finishing one looks
// like: the chunks landed and the progress ref that held them is deleted. An
// unfinished one may not, because its progress ref still holds commits nothing
// else refers to, and forgetting it is how they become unreachable.
func (m merger) tracks(ctx context.Context, prev, next []Track) error {
	index := make(map[string]Track, len(next))
	for _, track := range next {
		index[track.Name] = track
	}
	for _, before := range prev {
		after, ok := index[before.Name]
		if !ok {
			if before.Done == before.Total {
				continue
			}
			return fmt.Errorf("%w: track %q had done %d of %d commits and is absent from the next document",
				ErrDropped, before.Name, before.Done, before.Total)
		}
		if err := immutable("track "+before.Name+" ref", before.Ref, after.Ref); err != nil {
			return err
		}
		switch {
		case after.Done < before.Done:
			return fmt.Errorf("%w: track %q has done %d commits, it had done %d",
				ErrRewind, before.Name, after.Done, before.Done)
		case after.Total < before.Total:
			return fmt.Errorf("%w: track %q must cover %d commits, it had to cover %d",
				ErrRewind, before.Name, after.Total, before.Total)
		}
		what := "track " + before.Name
		if err := m.sourceDescends(ctx, what+" source", before.Source, after.Source); err != nil {
			return err
		}
		if err := m.destinationDescends(ctx, what+" destination", before.Destination, after.Destination); err != nil {
			return err
		}
	}
	return nil
}

// published checks the observed destination refs.
//
// Both halves of an entry are checked, not just the object. A branch that fast
// forwards while its source jumps sideways is the failure this catches: the
// object moving forward looks like an ordinary advance, and without the source
// rule the record would silently start claiming the branch was published from
// history it was not. The two are one observation and they have to move
// together.
//
// A tag is compared for equality under every condition, including a profile
// change, and on its source as well as its object. Re-deriving history is a
// decision the engine may make about commits it has not published; a tag is a
// promise it already made to whoever resolved it, and no later decision reopens
// that. Restating which source a published tag came from is reopening it.
func (m merger) published(ctx context.Context, prev, next []Published) error {
	index := make(map[string]Published, len(next))
	for _, entry := range next {
		index[entry.Ref] = entry
	}
	for _, before := range prev {
		after, ok := index[before.Ref]
		if !ok {
			if before.Kind == KindTag {
				return fmt.Errorf("%w: tag %q was observed at %s and is absent from the next document",
					ErrTagMoved, before.Ref, before.Object)
			}
			return fmt.Errorf("%w: published %q is observed at %s and is absent from the next document",
				ErrDropped, before.Ref, before.Object)
		}
		// The kind is not compared. A branch lives under refs/heads/ and a tag
		// under refs/tags/, both documents have been validated against that,
		// and the two namespaces are disjoint, so one ref cannot be a branch in
		// one document and a tag in the other. Relabelling a tag necessarily
		// moves it to another ref, which is the absence handled above.
		if before.Kind == KindTag {
			if after.Object != before.Object {
				return fmt.Errorf("%w: tag %q now points at %s, it was observed at %s",
					ErrTagMoved, before.Ref, after.Object, before.Object)
			}
			if after.Source != before.Source {
				return fmt.Errorf("%w: tag %q is now published from %s, it was observed as published from %s",
					ErrTagMoved, before.Ref, after.Source, before.Source)
			}
			continue
		}
		// A branch is required to descend from what was observed whatever the
		// epoch did, because the remote refuses a non fast forward push and a
		// record claiming otherwise would have the engine plan a push that
		// cannot land.
		if err := m.destinationRawDescends(ctx, "published "+before.Ref, before.Object, after.Object); err != nil {
			return err
		}
		// The source side advances under the ordinary rule. Unlike the
		// destination it is never re-derived, so a profile change does not relax
		// it: upstream history is what it is in every epoch.
		if err := m.sourceDescends(ctx, "published "+before.Ref+" source", before.Source, after.Source); err != nil {
			return err
		}
	}
	return nil
}

// descends reports a position that moved to a commit the recorded one does not
// reach.
//
// Two cases short circuit before the repository is asked. An unchanged position
// is trivially a descendant of itself, and asking would spend a subprocess to
// learn it. An empty recorded position is a position that was never taken, so
// the first commit recorded there is an advance from nothing rather than a move
// that has to be justified.
func descends(ctx context.Context, ancestry Ancestry, what, from, to string) error {
	if from == to || from == "" {
		return nil
	}
	if to == "" {
		return fmt.Errorf("%w: %s was recorded at %s and is now empty", ErrRewind, what, from)
	}
	ok, err := ancestry.IsAncestor(ctx, from, to)
	if err != nil {
		return fmt.Errorf("%s: ancestry of %s and %s: %w", what, from, to, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s moved to %s, which does not descend from %s", ErrRewind, what, to, from)
	}
	return nil
}

func (m merger) sourceDescends(ctx context.Context, what, from, to string) error {
	return descends(ctx, m.source, what, from, to)
}

func (m merger) destinationRawDescends(ctx context.Context, what, from, to string) error {
	return descends(ctx, m.destination, what, from, to)
}

// destinationDescends applies the descent rule only within one epoch.
func (m merger) destinationDescends(ctx context.Context, what, from, to string) error {
	if !m.sameEpoch {
		return nil
	}
	return m.destinationRawDescends(ctx, what, from, to)
}

// immutable reports a field that identifies the record and changed anyway.
func immutable(what, before, after string) error {
	if before == after {
		return nil
	}
	return fmt.Errorf("%w: %s is %s, it was %s", ErrImmutable, what, quote(after), quote(before))
}

// quote renders a value for a refusal, naming an empty one rather than showing
// an empty pair of quotes a reader has to interpret.
func quote(value string) string {
	if value == "" {
		return "unset"
	}
	return `"` + strings.ReplaceAll(value, `"`, `'`) + `"`
}
