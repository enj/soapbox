package gitcli

import (
	"slices"
	"strings"
	"testing"
)

// TestLsTreeArgsCarryNoPathspec pins the shape of the listing vector.
//
// ListTree is the read back that proves a written tree holds what it was asked
// to hold, so anything that could make it answer with fewer entries defeats it.
// A pathspec is the only argument that can, and a bare "--" at the end is how
// an empty one gets added by accident, so the separator is asserted absent
// rather than trusted to stay that way.
func TestLsTreeArgsCarryNoPathspec(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	args := lsTreeArgs(revision)

	if got := args[len(args)-1]; got != revision {
		t.Errorf("vector ends with %q, want the revision", got)
	}
	if slices.Contains(args, "--") {
		t.Errorf("vector %q carries a pathspec separator", args)
	}
	// --end-of-options is what makes a revision beginning with a dash safe, and
	// it is the reason a separator is unnecessary in the first place.
	end := slices.Index(args, "--end-of-options")
	if end < 0 {
		t.Fatalf("vector %q does not stop option parsing", args)
	}
	if end != len(args)-2 {
		t.Errorf("vector %q puts an argument between --end-of-options and the revision", args)
	}
	// -z is what keeps a path with a space or a quote readable rather than
	// rendered, and --full-tree is what keeps the listing independent of the
	// process working directory.
	for _, want := range []string{"-r", "-z", "--full-tree"} {
		if !slices.Contains(args, want) {
			t.Errorf("vector %q is missing %s", args, want)
		}
	}
}

// TestIsReservedComponent pins which spellings of git's own directory are
// refused, and which lookalikes a generated module keeps.
func TestIsReservedComponent(t *testing.T) {
	tests := []struct {
		component string
		reserved  bool
	}{
		{component: ".git", reserved: true},
		{component: ".GIT", reserved: true},
		{component: ".Git", reserved: true},
		{component: ".git.", reserved: true},
		{component: ".git...", reserved: true},
		{component: ".git ", reserved: true},
		{component: ".git . .", reserved: true},
		{component: "git~1", reserved: true},
		{component: "GIT~1", reserved: true},
		{component: "git~1.", reserved: true},
		// Everything below is a real file name from a real module.
		{component: ".gitignore"},
		{component: ".gitattributes"},
		{component: ".gitmodules"},
		{component: ".github"},
		{component: "git"},
		{component: "gitutil"},
		{component: "git~10"},
		{component: "git~2"},
		{component: ".git.go"},
		{component: "doc.go"},
		{component: ""},
		{component: "..."},
	}
	for _, test := range tests {
		t.Run(test.component, func(t *testing.T) {
			if got := isReservedComponent(test.component); got != test.reserved {
				t.Errorf("isReservedComponent(%q) = %v, want %v", test.component, got, test.reserved)
			}
		})
	}
}

// TestReservedComponentsMatchGitsOwnRule states the rule this package models,
// so a future reader can tell a deliberate difference from a drift.
//
// Git refuses more than this on a repository configured for HFS+ or NTFS, and
// WriteTree does not try to reproduce that: it treats anything update-index
// declines as a failure instead. What is modelled here is the set git refuses
// everywhere, whatever the configuration.
func TestReservedComponentsMatchGitsOwnRule(t *testing.T) {
	for _, component := range []string{".git", "git~1"} {
		if !isReservedComponent(component) {
			t.Errorf("%q is refused by every git and must be refused here", component)
		}
		if !isReservedComponent(strings.ToUpper(component)) {
			t.Errorf("%q is refused case insensitively by git", strings.ToUpper(component))
		}
	}
}

// TestValidateRawDate pins the one rule for a replayed date.
//
// It is exported and shared rather than reimplemented per package, because it
// had been reimplemented once and the two copies disagreed: this one accepted a
// negative count of seconds and treebuild's refused it, so the same upstream
// identity could be written into a tag and refused for a commit.
func TestValidateRawDate(t *testing.T) {
	tests := []struct {
		date  string
		valid bool
	}{
		{date: "1700000000 +0000", valid: true},
		{date: "1700000000 +0530", valid: true},
		{date: "1700000000 -0800", valid: true},
		{date: "0 +0000", valid: true},
		// Dates before 1970 are rare and real. Histories imported from CVS and
		// Subversion carry them and git stores them, so replay has to too.
		{date: "-100000 +0000", valid: true},
		{date: "-1 -0500", valid: true},
		{date: ""},
		{date: "1700000000"},
		{date: "2023-11-14T22:13:20Z"},
		{date: "yesterday"},
		{date: "1700000000 0000"},
		{date: "1700000000 +053"},
		{date: "1700000000 +05300"},
		{date: "1700000000 +05a0"},
		{date: "seconds +0000"},
		{date: "- +0000"},
		{date: "1700000000  +0000"},
	}
	for _, test := range tests {
		t.Run(test.date, func(t *testing.T) {
			err := ValidateRawDate(test.date)
			if test.valid && err != nil {
				t.Errorf("ValidateRawDate(%q) = %v, want no error", test.date, err)
			}
			if !test.valid && err == nil {
				t.Errorf("ValidateRawDate(%q) was accepted", test.date)
			}
		})
	}
}

const fullTagObjectFixture = "object 1111111111111111111111111111111111111111\n" +
	"type commit\n" +
	"tag v0.36.1\n" +
	"tagger Soapbox Bot <bot@example.com> 1700000000 +0000\n\n" +
	"release\n"

func TestParseFullTagObject(t *testing.T) {
	got, err := parseFullTagObject(fullTagObjectFixture)
	if err != nil {
		t.Fatalf("parse tag object: %v", err)
	}
	if got.TargetOID != "1111111111111111111111111111111111111111" {
		t.Errorf("target OID = %q", got.TargetOID)
	}
	if got.TargetType != "commit" {
		t.Errorf("target type = %q", got.TargetType)
	}
	if got.InternalName != "v0.36.1" {
		t.Errorf("internal name = %q", got.InternalName)
	}
	if got.Tagger != (Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0000"}) {
		t.Errorf("tagger = %#v", got.Tagger)
	}
	if got.Message != "release\n" {
		t.Errorf("message = %q", got.Message)
	}
}

func TestParseFullTagObjectRejectsMalformedMetadata(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "header without a value separator",
			body: strings.Replace(fullTagObjectFixture, "type commit\n", "type commit\nunrecognized\n", 1),
			want: "malformed header",
		},
		{
			name: "unknown header",
			body: strings.Replace(fullTagObjectFixture, "type commit\n", "type commit\nencoding UTF-8\n", 1),
			want: "unknown header",
		},
		{
			name: "duplicate header",
			body: strings.Replace(fullTagObjectFixture, "type commit\n", "type commit\ntype commit\n", 1),
			want: "duplicate type header",
		},
		{
			name: "short object name",
			body: strings.Replace(fullTagObjectFixture, strings.Repeat("1", 40), strings.Repeat("1", 39), 1),
			want: "not a full object name",
		},
		{
			name: "unsupported target type",
			body: strings.Replace(fullTagObjectFixture, "type commit", "type widget", 1),
			want: "not a valid git object type",
		},
		{
			name: "invalid internal name",
			body: strings.Replace(fullTagObjectFixture, "tag v0.36.1", "tag refs/tags/v0.36.1", 1),
			want: "must be a short name",
		},
		{
			name: "missing tagger name",
			body: strings.Replace(fullTagObjectFixture, "Soapbox Bot <", "<", 1),
			want: "tagger has no name",
		},
		{
			name: "missing tagger email",
			body: strings.Replace(fullTagObjectFixture, "bot@example.com", "", 1),
			want: "tagger has no email address",
		},
		{
			name: "invalid tagger date",
			body: strings.Replace(fullTagObjectFixture, "1700000000 +0000", "not-a-date", 1),
			want: "tagger date",
		},
		{
			name: "missing message separator",
			body: strings.Replace(fullTagObjectFixture, "\n\nrelease\n", "\nrelease\n", 1),
			want: "no message separator",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseFullTagObject(test.body)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}
