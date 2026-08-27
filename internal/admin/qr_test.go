package admin

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"rsc.io/qr"
)

func TestQRSVG(t *testing.T) {
	const text = "nc://login/server:https://mirage.example.com&user:alice&password:abc123"
	svg, err := qrSVG(text)
	if err != nil {
		t.Fatalf("qrSVG: %v", err)
	}
	s := string(svg)

	if !strings.HasPrefix(s, "<svg ") || !strings.HasSuffix(s, "</svg>") {
		t.Fatal("output is not a complete SVG element")
	}
	// A scanner needs the quiet margin and a light background; a transparent
	// code inherits the page behind it and inverts on a dark theme.
	if !strings.Contains(s, `fill="#ffffff"`) {
		t.Error("no light background, so the code would invert on a dark theme")
	}
	if !strings.Contains(s, "viewBox=") {
		t.Error("no viewBox, so the code cannot scale")
	}

	// The encoded content must survive a round trip.
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if code.Size <= 0 {
		t.Fatal("empty QR matrix")
	}
	// The margin has to be outside the matrix on both sides.
	if !strings.Contains(s, "0 0 "+itoa(int64(code.Size+8))) {
		t.Errorf("viewBox does not include the quiet margin; got %.80s", s)
	}
}

func TestQRSVGRejectsOversizedInput(t *testing.T) {
	// Beyond what any QR version can hold; better an error than a code that
	// silently encodes nothing useful.
	if _, err := qrSVG(strings.Repeat("x", 10000)); err == nil {
		t.Error("an input too large to encode was accepted")
	}
}

// TestQRSVGMatchesTheMatrix parses the rendered SVG back into a grid and
// compares it against the QR matrix module by module.
//
// Looking right is not the same as being right: the rendering merges runs of
// dark modules into single rectangles, and an off-by-one in that would produce
// something that still looks like a QR code and decodes to nothing. Nobody
// notices until a phone refuses to scan it.
func TestQRSVGMatchesTheMatrix(t *testing.T) {
	const text = "nc://login/server:https://mirage.example.com&user:ana&password:" +
		"MaMq2iQQpoKZgQ9OxeiEN4jkRCX3biNMn6T0qjGycJgzijqJz3rCfATxDnzj66ovm6xDswm6"

	code, err := qr.Encode(text, qr.M)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	svg, err := qrSVG(text)
	if err != nil {
		t.Fatalf("qrSVG: %v", err)
	}

	const quiet = 4
	size := code.Size + quiet*2
	painted := make([][]bool, size)
	for i := range painted {
		painted[i] = make([]bool, size)
	}

	// Every dark rectangle, ignoring the background one that covers everything.
	re := regexp.MustCompile(`<rect x="(\d+)" y="(\d+)" width="(\d+)" height="1" fill="#000000"/>`)
	matches := re.FindAllStringSubmatch(string(svg), -1)
	if len(matches) == 0 {
		t.Fatal("no dark modules in the rendered code")
	}
	for _, m := range matches {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		for i := range w {
			if y >= size || x+i >= size {
				t.Fatalf("a rectangle at (%d,%d) width %d falls outside the %dx%d canvas", x, y, w, size, size)
			}
			painted[y][x+i] = true
		}
	}

	for y := range code.Size {
		for x := range code.Size {
			want := code.Black(x, y)
			got := painted[y+quiet][x+quiet]
			if got != want {
				t.Fatalf("module (%d,%d): rendered %v, matrix says %v", x, y, got, want)
			}
		}
	}

	// The quiet margin has to be genuinely empty, or scanners cannot find the
	// code against the page behind it.
	for y := range size {
		for x := range size {
			inCode := x >= quiet && x < quiet+code.Size && y >= quiet && y < quiet+code.Size
			if !inCode && painted[y][x] {
				t.Fatalf("the quiet margin is painted at (%d,%d)", x, y)
			}
		}
	}
}
