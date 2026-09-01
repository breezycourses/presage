# Branding assets

```bash
make branding      # re-render docs/assets/*.png
```

The output is committed, so this only needs running when the artwork changes.
Requires `rsvg-convert` (librsvg).

## The lettering

The wordmark and tagline are **Satoshi**, pre-outlined into SVG paths by
`text-to-paths.py` and stored in `lib/lettering.json`. The font file itself is
not in this repository.

Outlining is not a stylistic choice. `rsvg-convert` cannot be relied on to find
a font that is not installed system-wide, and on macOS its Pango is built
against CoreText, which ignores a scratch `FONTCONFIG_FILE` **silently** — it
falls back to Helvetica and renders a banner in the wrong typeface with no
error at all. Geometry removes the question: the render is identical on any
machine, and no font has to be present.

To change the lettering you need Satoshi, which lives in the Breezy asset
package (`packages/assets/files/fonts/satoshi-variable.woff2`):

```bash
uv run --with fonttools --with brotli --with uharfbuzz \
  python hack/branding/text-to-paths.py > hack/branding/lib/lettering.json
make branding
```

HarfBuzz does the shaping, so the spacing is the font's own kerning rather than
a naive sum of advance widths. The script fails hard if shaping produces
`.notdef` — a tofu wordmark renders without error and looks merely ugly, which
is exactly the kind of failure that reaches production.

## The artwork

`lib/sky-art.mjs` is a port of the Breezy asset pipeline's structure — same
variant table, same fractional cloud layout, same `rsvg-convert` step — with
three departures:

* **Neutral dark grey**, not blue. A coloured ground competes with the one
  accent the mark is allowed.
* **Clouds are drawn procedurally**, not composited from raster art, so this
  repository carries no other project's brand assets. They are seeded rather
  than random, so a re-render is byte-identical and does not turn an unrelated
  change into a binary diff in review.
* **No character.** The mark is a forecast curve with a widening uncertainty
  band — the observed half carries texture, the forecast half is smooth, and
  the band opens with the horizon. It is a picture of what presage computes.
