// Package qrcode renders sign-in codes.
//
// Used by both the admin page and a person's own profile page: an
// administrator can set up somebody's phone for them, and somebody can set up
// their own without having to ask.
package qrcode

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrSVG renders text as a QR code, as an inline SVG element.
//
// Inline rather than an image because the page's content security policy
// forbids loading anything at all, and an SVG written into the document is
// markup rather than a fetched resource. It also scales without blurring,
// which matters when the thing scanning it is a phone camera held at whatever
// distance.
func SVG(text string) (template.HTML, error) {
	// Medium recovery: enough redundancy for a screen photographed at an angle,
	// without inflating the code so far that the modules get small.
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", fmt.Errorf("encode QR code: %w", err)
	}

	const quiet = 4 // modules of margin, which scanners need to find the code
	size := code.Size + quiet*2

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" `+
		`shape-rendering="crispEdges" role="img" aria-label="Sign-in code">`, size, size)
	// The light background is drawn explicitly: a transparent code inherits
	// whatever is behind it, and on a dark theme that inverts the contrast a
	// scanner depends on.
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#ffffff"/>`, size, size)

	for y := range code.Size {
		// Horizontal runs are emitted as single rectangles rather than one per
		// module, which cuts the markup for a dense code by roughly half.
		x := 0
		for x < code.Size {
			if !code.Black(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < code.Size && code.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="1" fill="#000000"/>`,
				x+quiet, y+quiet, run)
			x += run
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), nil //nolint:gosec // built here from a QR matrix
}
