package backtest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

/* Chart labels are drawn as outlined glyphs rather than <text>.
 *
 * These charts are read on GitHub, which renders an SVG against the *viewer's*
 * installed fonts -- so `font-family="Satoshi"` would fall back to something
 * else for essentially everyone, and the chart would not match the rest of the
 * project's typography for anybody but its author. Outlining makes the file
 * self-contained: it renders identically in a browser, in a PDF, and in a
 * raster export, on a machine that has never heard of Satoshi.
 *
 * The atlas carries only the glyphs chart labels can use, and the font itself
 * is not vendored. Regenerate with:
 *
 *   uv run --with fonttools --with brotli --with uharfbuzz \
 *     python hack/branding/text-to-paths.py --atlas \
 *     > internal/backtest/satoshi_atlas.json
 */

//go:embed satoshi_atlas.json
var satoshiAtlasJSON []byte

type glyph struct {
	D   string  `json:"d"`
	Adv float64 `json:"adv"`
}

type face struct {
	UPEM      float64          `json:"upem"`
	CapHeight float64          `json:"capHeight"`
	Glyphs    map[string]glyph `json:"glyphs"`
}

var atlas map[string]face

func init() {
	if err := json.Unmarshal(satoshiAtlasJSON, &atlas); err != nil {
		panic(fmt.Sprintf("backtest: unreadable glyph atlas: %v", err))
	}
}

// Anchor positions a label horizontally against x.
type Anchor int

const (
	AnchorStart Anchor = iota
	AnchorMiddle
	AnchorEnd
)

// textWidth is the advance width of s at the given size.
func textWidth(s string, size float64, weight string) float64 {
	f, ok := atlas[weight]
	if !ok {
		return 0
	}
	var w float64
	for _, r := range s {
		if g, ok := f.Glyphs[string(r)]; ok {
			w += g.Adv
		} else if g, ok := f.Glyphs[" "]; ok {
			// An unknown glyph becomes a space rather than nothing, so a
			// stray character shifts the label instead of silently closing
			// the gap and mis-aligning everything after it.
			w += g.Adv
		}
	}
	return w * size / f.UPEM
}

// label renders s as outlined glyphs with its baseline at y.
func label(s string, x, y, size float64, weight, fill string, anchor Anchor) string {
	f, ok := atlas[weight]
	if !ok || s == "" {
		return ""
	}

	switch anchor {
	case AnchorMiddle:
		x -= textWidth(s, size, weight) / 2
	case AnchorEnd:
		x -= textWidth(s, size, weight)
	}

	scale := size / f.UPEM
	var b strings.Builder
	// Glyph outlines are y-up; SVG is y-down, hence the negative y scale.
	fmt.Fprintf(&b, `<g transform="translate(%.2f %.2f) scale(%.5f %.5f)" fill="%s">`,
		x, y, scale, -scale, fill)

	var pen float64
	for _, r := range s {
		g, ok := f.Glyphs[string(r)]
		if !ok {
			g = f.Glyphs[" "]
		}
		if g.D != "" {
			fmt.Fprintf(&b, `<path d="%s" transform="translate(%.1f 0)"/>`, g.D, pen)
		}
		pen += g.Adv
	}
	b.WriteString(`</g>`)
	return b.String()
}

// labelRotated renders a label rotated about (x, y), for axis titles.
func labelRotated(s string, x, y, size, degrees float64, weight, fill string, anchor Anchor) string {
	inner := label(s, 0, 0, size, weight, fill, anchor)
	if inner == "" {
		return ""
	}
	return fmt.Sprintf(`<g transform="translate(%.2f %.2f) rotate(%.1f)">%s</g>`, x, y, degrees, inner)
}
