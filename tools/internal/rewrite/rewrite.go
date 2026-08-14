// Package rewrite performs the minimal syntax aware source changes that turn
// upstream Kubernetes files into files of the generated module.
//
// Every transformation is a byte range replacement in the original file. The
// package never reprints a file from its syntax tree, because printing would
// reformat import grouping, move comments, normalise spacing, and quietly
// rewrite everything the transformation was supposed to leave alone. Replacing
// exactly the ranges that must change means aliases, blank and dot imports,
// build constraints, cgo preambles, generated file markers, raw string
// literals, and every byte outside those ranges survive untouched by
// construction rather than by inspection.
//
// Eligibility is narrow. Only import paths rooted at the configured source
// prefix are rewritten. An import owned by an external module, such as
// k8s.io/api/rbac/v1, keeps its real identity, because relocating it would
// create a type that no longer equals the upstream type it must satisfy.
// Arbitrary string literals, API group strings, and annotation values are never
// touched even when they read like an import path.
//
// Every rewritten file is reparsed and its syntax tree compared with the
// original's, excluding the import path literals that were meant to change. A
// transformation that altered anything else fails the run instead of reaching a
// published tag.
package rewrite

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
)

// Rewrite sentinels. Callers use errors.Is to distinguish the failure.
var (
	// ErrPrefix reports an unusable source prefix, destination module, or
	// internal prefix.
	ErrPrefix = errors.New("configured prefix is not a usable import path")
	// ErrCarriageReturn reports a source file with CRLF line endings under the
	// rejecting policy.
	ErrCarriageReturn = errors.New("file uses carriage return line endings")
	// ErrMixedLineEndings reports a file that terminates some lines with CRLF
	// and others with LF. Under the preserving policy an inserted notice has to
	// adopt the file's convention, and a file with two conventions has none to
	// adopt.
	ErrMixedLineEndings = errors.New("file mixes carriage return and line feed line endings")
	// ErrShapeChanged reports a rewrite that altered something other than the
	// import paths it was allowed to change.
	ErrShapeChanged = errors.New("rewrite changed the syntax tree beyond its import paths")
	// ErrCommentsChanged reports a rewrite that lost or invented a comment it
	// did not record in its change report.
	ErrCommentsChanged = errors.New("rewrite changed comments it did not record")
	// ErrOverlappingEdits reports two transformations claiming one byte range,
	// which would make the result depend on their order.
	ErrOverlappingEdits = errors.New("two rewrites claim overlapping byte ranges")
	// ErrEmbedUnmatched reports a go:embed pattern that matches nothing in the
	// relocated file set.
	ErrEmbedUnmatched = errors.New("go:embed pattern matches no relocated file")
	// ErrEmbedEscape reports a go:embed pattern that leaves its package
	// directory, which the Go toolchain also refuses.
	ErrEmbedEscape = errors.New("go:embed pattern leaves its package directory")
	// ErrEmbedPattern reports a go:embed pattern list the Go toolchain would
	// refuse to parse, such as one holding an unterminated quoted pattern.
	ErrEmbedPattern = errors.New("go:embed pattern list is malformed")
)

// LineEndingPolicy decides what happens to a file with carriage returns.
type LineEndingPolicy uint8

const (
	// LineEndingReject refuses a file containing a carriage return. It is the
	// default because the pinned gofmt normalises line endings, so a CRLF file
	// that survived the rewrite unchanged would still produce a formatting diff
	// in the generated module, and the run has to fail where the cause is
	// visible rather than three steps later.
	LineEndingReject LineEndingPolicy = iota
	// LineEndingPreserve accepts carriage returns and leaves them exactly as
	// they are. Byte range replacement never normalises line endings, so a
	// preserved file round trips unchanged outside its rewritten ranges.
	LineEndingPreserve
)

// File is one source file to transform.
type File struct {
	// Path is the destination module relative path, which reports name.
	Path string
	// SourcePath is the upstream repository relative path, which the
	// modification notice and the provenance record name.
	SourcePath string
	// Contents are the file bytes.
	Contents []byte
	// Generated records that the file carries a Code generated marker.
	Generated bool
}

// Options configures every transformation.
type Options struct {
	// SourcePrefix is the upstream module path, such as k8s.io/kubernetes. Only
	// imports rooted here are eligible.
	SourcePrefix string
	// DestinationModule is the generated module path, such as
	// monis.app/kk/rbac_authorizer.
	DestinationModule string
	// InternalPrefix is the module relative directory relocated packages live
	// below, such as internal/kk.
	InternalPrefix string
	// SourceRepository and SourceSHA identify the upstream commit for notices
	// and provenance records.
	SourceRepository string
	SourceSHA        string
	// LineEndings selects the carriage return policy.
	LineEndings LineEndingPolicy
	// Directives holds the key scoped generator comment rules. The zero value
	// keeps every directive, so a caller that has not thought about markers
	// cannot lose one by accident.
	Directives DirectiveRules
	// NoNotice suppresses the modification notice. Notices are on by default
	// because a reader who opens a relocated file has to be able to tell that it
	// is not the upstream original.
	NoNotice bool
}

// validate checks the configured paths.
func (o Options) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"source prefix", o.SourcePrefix},
		{"destination module", o.DestinationModule},
	} {
		if err := config.ValidateModulePath(field.value); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrPrefix, field.name, err)
		}
	}
	if err := config.ValidatePackagePath(o.InternalPrefix); err != nil {
		return fmt.Errorf("%w: internal prefix: %w", ErrPrefix, err)
	}
	return nil
}

// destination reports the relocated import path for an upstream import, and
// whether the import was eligible at all.
//
// Eligibility is an exact prefix match on a path boundary. A module named
// k8s.io/kubernetes-extra shares a textual prefix with k8s.io/kubernetes and
// must not be rewritten, which a plain string prefix test would get wrong.
func (o Options) destination(path string) (string, bool) {
	root := o.DestinationModule + "/" + o.InternalPrefix
	switch {
	case path == o.SourcePrefix:
		return root, true
	case strings.HasPrefix(path, o.SourcePrefix+"/"):
		return root + strings.TrimPrefix(path, o.SourcePrefix), true
	default:
		return "", false
	}
}

// ChangeKind classifies one recorded change.
type ChangeKind string

const (
	// ChangeImport is a rewritten import path literal.
	ChangeImport ChangeKind = "import"
	// ChangeDirective is a rewritten generator directive value.
	ChangeDirective ChangeKind = "directive"
	// ChangeMarkerRemoval is a removed generator directive line.
	ChangeMarkerRemoval ChangeKind = "marker-removal"
	// ChangeCommentRemoval is a removed empty documentation comment line that a
	// marker removal would otherwise have stranded.
	ChangeCommentRemoval ChangeKind = "comment-removal"
	// ChangeNotice is an inserted modification notice.
	ChangeNotice ChangeKind = "notice"
	// ChangeProtoOption is a rewritten proto go_package option.
	ChangeProtoOption ChangeKind = "proto-option"
	// ChangeCompatibility is an intentional API or implementation edit selected
	// by an output compatibility mode.
	ChangeCompatibility ChangeKind = "compatibility"
)

// Change is one recorded transformation. The change report is what a
// provenance record renders and what a reviewer reads to see exactly which
// bytes the engine claimed.
type Change struct {
	// Kind classifies the change.
	Kind ChangeKind
	// Path is the file the change applies to.
	Path string
	// Line is the one based line of the original file.
	Line int
	// From is the replaced text, empty for an insertion.
	From string
	// To is the replacement text, empty for a removal.
	To string
}

// String renders the change for a report.
func (c Change) String() string {
	from, to := summarize(c.From), summarize(c.To)
	switch {
	case from == "":
		return fmt.Sprintf("%s:%d %s + %s", c.Path, c.Line, c.Kind, to)
	case to == "":
		return fmt.Sprintf("%s:%d %s - %s", c.Path, c.Line, c.Kind, from)
	default:
		return fmt.Sprintf("%s:%d %s %s -> %s", c.Path, c.Line, c.Kind, from, to)
	}
}

// summarize renders a possibly multi line value on one line, so a report stays
// one change per line. An inserted notice is the only multi line value, and its
// first line identifies it well enough for a report; the full text is still in
// the change itself for anything that needs it.
func summarize(text string) string {
	text = strings.TrimRight(text, "\n")
	if first, _, found := strings.Cut(text, "\n"); found {
		return first + " ..."
	}
	return text
}

// compareChanges orders changes by position and then by content, so a report is
// byte identical across runs.
func compareChanges(a, b Change) int {
	return cmp.Or(
		cmp.Compare(a.Path, b.Path),
		cmp.Compare(a.Line, b.Line),
		cmp.Compare(string(a.Kind), string(b.Kind)),
		cmp.Compare(a.From, b.From),
		cmp.Compare(a.To, b.To),
	)
}

// Result is a transformed file.
type Result struct {
	// Contents are the rewritten bytes. When no change applied they are the
	// original bytes, not a copy.
	Contents []byte
	// Changes are every recorded transformation, in report order.
	Changes []Change
}

// Changed reports whether anything was rewritten.
func (r Result) Changed() bool { return len(r.Changes) > 0 }

// edit is one byte range replacement in the original file.
type edit struct {
	// start and end bound the replaced range. An insertion has start == end.
	start int
	end   int
	// text is the replacement.
	text string
	// change is the report entry this edit produces.
	change Change
}

// applyEdits replaces every claimed range in one pass over the original bytes.
//
// Working from the original offsets keeps each transformation independent: no
// step has to know how much earlier steps grew or shrank the file, which is
// what makes the result independent of the order the transformations were
// discovered in.
func applyEdits(src []byte, edits []edit) ([]byte, []Change, error) {
	if len(edits) == 0 {
		return src, nil, nil
	}
	slices.SortFunc(edits, func(a, b edit) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end))
	})
	for i := 1; i < len(edits); i++ {
		if edits[i].start < edits[i-1].end {
			return nil, nil, fmt.Errorf("%w: [%d,%d) and [%d,%d)",
				ErrOverlappingEdits, edits[i-1].start, edits[i-1].end, edits[i].start, edits[i].end)
		}
	}

	var out strings.Builder
	out.Grow(len(src))
	changes := make([]Change, 0, len(edits))
	cursor := 0
	for _, e := range edits {
		if e.start < cursor || e.end > len(src) {
			return nil, nil, fmt.Errorf("%w: [%d,%d) is outside the file", ErrOverlappingEdits, e.start, e.end)
		}
		out.Write(src[cursor:e.start])
		out.WriteString(e.text)
		cursor = e.end
		changes = append(changes, e.change)
	}
	out.Write(src[cursor:])
	slices.SortFunc(changes, compareChanges)
	return []byte(out.String()), changes, nil
}

// crlf and lf are the two line terminators a source file can use. An inserted
// notice adopts whichever one the file it joins already uses.
const (
	crlf = "\r\n"
	lf   = "\n"
)

// checkLineEndings applies the carriage return policy and reports the line
// terminator an insertion into this file has to use.
//
// Under the preserving policy a file that terminates some lines with CRLF and
// others with LF is refused. A notice inserted into such a file would have to
// pick one convention and would make the file inconsistent whichever it picked,
// and the mixture is itself a sign of a file that has already been mangled in
// transit.
func checkLineEndings(file File, policy LineEndingPolicy) (string, error) {
	if policy == LineEndingReject {
		offset := bytes.IndexByte(file.Contents, '\r')
		if offset < 0 {
			return lf, nil
		}
		line := bytes.Count(file.Contents[:offset], []byte(lf)) + 1
		return "", fmt.Errorf("%w: first carriage return on line %d", ErrCarriageReturn, line)
	}

	windows, unix := countTerminators(file.Contents)
	switch {
	case windows > 0 && unix > 0:
		return "", fmt.Errorf("%w: %d lines end with CRLF and %d with LF", ErrMixedLineEndings, windows, unix)
	case windows > 0:
		return crlf, nil
	default:
		return lf, nil
	}
}

// countTerminators reports how many lines end with CRLF and how many with a
// bare LF.
func countTerminators(src []byte) (windows, unix int) {
	for offset := 0; ; {
		at := bytes.IndexByte(src[offset:], '\n')
		if at < 0 {
			return windows, unix
		}
		at += offset
		if at > 0 && src[at-1] == '\r' {
			windows++
		} else {
			unix++
		}
		offset = at + 1
	}
}
