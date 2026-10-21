package web

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// TerminalQR renders text as a QR code using half-block characters, two
// modules per character row, with a quiet zone. Scannable from a phone.
func TerminalQR(text string) string {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return ""
	}
	const quiet = 2
	n := code.Size
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= n || y >= n {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < n+quiet; y += 2 {
		b.WriteString("  ")
		for x := -quiet; x < n+quiet; x++ {
			top, bottom := black(x, y), black(x, y+1)
			// Light-on-dark terminals: draw the light modules.
			switch {
			case !top && !bottom:
				b.WriteRune('█')
			case !top && bottom:
				b.WriteRune('▀')
			case top && !bottom:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// SVGQR renders text as a QR code SVG (dark modules on a light square).
func SVGQR(text string) string {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return ""
	}
	const quiet = 3
	size := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, size, size)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, size, size)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
