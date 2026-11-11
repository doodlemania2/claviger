// Package qr draws a QR code as an SVG document, with no network calls and no
// image dependencies. It wraps github.com/skip2/go-qrcode (MIT, no
// dependencies of its own) so the rest of the code never imports it directly.
package qr

import (
	"errors"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// quietZone is the blank border, in modules, the QR standard asks for.
const quietZone = 4

// SVG returns the QR code for text as a standalone SVG document.
//
// The document has a viewBox and no fixed width or height, so CSS sizes it.
// It uses presentation attributes only (no style attribute, no script, no
// external references), which keeps it valid under a strict
// Content-Security-Policy when it is placed in the page. Dark modules are
// drawn on a white background so the code scans in dark mode too.
//
// title becomes the accessible name (role="img" with a <title>); pass the text
// a screen reader should announce, never the encoded content.
func SVG(text, title string) (string, error) {
	if text == "" {
		return "", errors.New("qr: nothing to encode")
	}

	code, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return "", err
	}

	// no library quiet zone: it is added here as part of the viewBox
	code.DisableBorder = true
	bitmap := code.Bitmap()
	size := len(bitmap)
	full := size + 2*quietZone

	var path strings.Builder

	for y, row := range bitmap {
		for x := 0; x < len(row); {
			if !row[x] {
				x++
				continue
			}
			// one rectangle per horizontal run keeps the path short
			start := x
			for x < len(row) && row[x] {
				x++
			}
			path.WriteString("M" + strconv.Itoa(start+quietZone) + " " + strconv.Itoa(y+quietZone) +
				"h" + strconv.Itoa(x-start) + "v1h-" + strconv.Itoa(x-start) + "z")
		}
	}

	dim := strconv.Itoa(full)

	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + dim + " " + dim +
		`" role="img" aria-label="` + escape(title) + `" shape-rendering="crispEdges">`)
	b.WriteString(`<title>` + escape(title) + `</title>`)
	b.WriteString(`<rect width="` + dim + `" height="` + dim + `" fill="#fff"/>`)
	b.WriteString(`<path fill="#000" d="` + path.String() + `"/>`)
	b.WriteString(`</svg>`)

	return b.String(), nil
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
