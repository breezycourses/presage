#!/usr/bin/env python3
"""Outline the wordmark and tagline to SVG paths.

rsvg-convert cannot be relied on to find a font that is not installed
system-wide. On macOS its Pango is built against CoreText, so a
FONTCONFIG_FILE pointing at a scratch directory is ignored entirely -- and
ignored *silently*, falling back to Helvetica and producing a banner in the
wrong typeface that looks fine unless you know what Satoshi looks like. That
failure mode is worse than a hard error.

Outlining removes the problem: the rendered SVG carries geometry, so the
output is identical on any machine and no font has to be present at render
time. It is also what the Breezy asset pipeline does for its own lockup.

Only the glyphs actually used are emitted, and the font itself is never
committed to this repository.

    uv run --with fonttools --with brotli --with uharfbuzz \\
        python hack/branding/text-to-paths.py > hack/branding/lib/lettering.json
"""

from __future__ import annotations

import json
import pathlib
import sys

import uharfbuzz as hb
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.ttLib import TTFont
from fontTools.varLib import instancer

# Satoshi lives in the Breezy asset package; presage does not vendor it.
SOURCE = pathlib.Path(
    "~/IdeaProjects/Organizations/Breezy/breezy-websources/packages/assets/files/fonts/satoshi-variable.woff2"
).expanduser()

RUNS = {
    "wordmark": {"text": "presage", "weight": 700, "tracking": 0.0},
    "tagline": {
        "text": "forecast-driven autoscaling for Kubernetes and Agones",
        "weight": 500,
        # A little positive tracking: the tagline sets small, and Satoshi's
        # default fit is tight at that size.
        "tracking": 0.02,
    },
}


def outline(text: str, weight: int, tracking: float) -> dict:
    static = instancer.instantiateVariableFont(TTFont(SOURCE), {"wght": weight}, inplace=True)
    upem = static["head"].unitsPerEm
    glyph_set = static.getGlyphSet()
    order = static.getGlyphOrder()

    # HarfBuzz for shaping, so kerning is the font's own rather than a naive
    # sum of advance widths.
    buf = hb.Buffer()
    buf.add_str(text)
    buf.guess_segment_properties()

    blob = hb.Blob.from_file_path(str(_as_ttf(static)))
    face = hb.Face(blob)
    font = hb.Font(face)
    hb.shape(font, buf)

    track = tracking * upem
    parts, x = [], 0.0
    for info, pos in zip(buf.glyph_infos, buf.glyph_positions):
        if info.codepoint == 0:
            raise SystemExit(
                f"shaping produced .notdef for {text!r}: the font did not load. "
                "A tofu wordmark renders without error and looks merely ugly, so "
                "this is a hard failure rather than a warning."
            )
        name = order[info.codepoint]
        pen = SVGPathPen(glyph_set)
        glyph_set[name].draw(pen)
        if d := pen.getCommands():
            parts.append(f'<path d="{d}" transform="translate({x + pos.x_offset:.1f} 0)"/>')
        x += pos.x_advance + track

    return {
        "glyphs": "".join(parts),
        "advance": round(x, 1),
        "upem": upem,
        "capHeight": getattr(static.get("OS/2"), "sCapHeight", None) or round(upem * 0.72),
        "ascender": static["hhea"].ascender,
        "descender": static["hhea"].descender,
    }


_TTF_CACHE: dict[int, pathlib.Path] = {}


def _as_ttf(static: TTFont) -> pathlib.Path:
    """HarfBuzz needs a file; fontTools has one only in memory.

    The flavor has to be cleared first. TTFont carries it over from the source,
    so a font loaded from woff2 saves back *as woff2* whatever the filename
    says -- HarfBuzz then fails to parse it and maps every character to
    .notdef, producing a wordmark made entirely of tofu rectangles.
    """
    key = id(static)
    if key not in _TTF_CACHE:
        import tempfile

        static.flavor = None
        path = pathlib.Path(tempfile.mkstemp(suffix=".ttf")[1])
        static.save(path)
        _TTF_CACHE[key] = path
    return _TTF_CACHE[key]


def main() -> int:
    if not SOURCE.exists():
        print(f"font not found: {SOURCE}", file=sys.stderr)
        print("Satoshi is not vendored here; see hack/branding/README.md", file=sys.stderr)
        return 1

    out = {name: outline(**spec) for name, spec in RUNS.items()}
    out["_source"] = "Satoshi Variable, outlined; the font itself is not vendored"
    json.dump(out, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
