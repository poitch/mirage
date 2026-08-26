package dav

import (
	"strings"
	"testing"
)

func TestParsePropfindNamedProps(t *testing.T) {
	body := `<?xml version="1.0"?>
	<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
	  <d:prop><d:getetag/><d:resourcetype/><oc:fileid/></d:prop>
	</d:propfind>`

	names, allProps, err := parsePropfind(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parsePropfind: %v", err)
	}
	if allProps {
		t.Fatal("named props were read as allprop")
	}
	want := []PropName{
		{NSDav, "getetag"}, {NSDav, "resourcetype"}, {NSOwnCloud, "fileid"},
	}
	if len(names) != len(want) {
		t.Fatalf("got %d props, want %d: %v", len(names), len(want), names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("prop %d = %v, want %v", i, names[i], w)
		}
	}
}

// TestParsePropfindDefaultsToAllProps: an empty body means allprop under
// RFC 4918, and clients do send one.
func TestParsePropfindDefaultsToAllProps(t *testing.T) {
	for _, body := range []string{
		"",
		"   \n  ",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`,
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop></d:prop></d:propfind>`,
	} {
		_, allProps, err := parsePropfind(strings.NewReader(body))
		if err != nil {
			t.Fatalf("parsePropfind(%q): %v", body, err)
		}
		if !allProps {
			t.Errorf("parsePropfind(%q) did not request all properties", body)
		}
	}
}

func TestParsePropfindRejectsMalformed(t *testing.T) {
	if _, _, err := parsePropfind(strings.NewReader("<not-xml")); err == nil {
		t.Error("malformed XML was accepted")
	}
}

// TestEscapeTextLeavesQuotes keeps ETags looking the way clients see them from
// Nextcloud, rather than as &#34;-encoded values.
func TestEscapeTextLeavesQuotes(t *testing.T) {
	if got := escapeText(`"abc123"`); got != `"abc123"` {
		t.Errorf("escapeText = %q, want quotes left alone", got)
	}
}

func TestEscapeTextEscapesMarkup(t *testing.T) {
	got := escapeText(`a & b <tag> c`)
	want := `a &amp; b &lt;tag&gt; c`
	if got != want {
		t.Errorf("escapeText = %q, want %q", got, want)
	}
}

// TestEscapeTextDropsInvalidXMLChars: a filename may contain a control
// character that XML 1.0 cannot represent at all. Dropping it keeps the
// response parseable, where escaping it would not.
func TestEscapeTextDropsInvalidXMLChars(t *testing.T) {
	got := escapeText("na\x00me\x08here")
	if got != "namehere" {
		t.Errorf("escapeText = %q, want %q", got, "namehere")
	}
	// Tab, newline and carriage return are valid and must survive.
	if escapeText("a\tb\nc\rd") != "a\tb\nc\rd" {
		t.Error("escapeText dropped valid whitespace characters")
	}
}

func TestPropNameTag(t *testing.T) {
	tests := []struct {
		name    PropName
		wantTag string
		wantNS  string
	}{
		{PropName{NSDav, "getetag"}, "d:getetag", ""},
		{PropName{NSOwnCloud, "fileid"}, "oc:fileid", ""},
		{PropName{NSNextcloud, "has-preview"}, "nc:has-preview", ""},
		// An unknown namespace still has to be echoed back, since clients match
		// the response against what they asked for.
		{PropName{"http://example.com/ns", "custom"}, "x:custom", ` xmlns:x="http://example.com/ns"`},
	}
	for _, tc := range tests {
		tag, ns := tc.name.tag()
		if tag != tc.wantTag || ns != tc.wantNS {
			t.Errorf("%v.tag() = (%q, %q), want (%q, %q)", tc.name, tag, ns, tc.wantTag, tc.wantNS)
		}
	}
}
