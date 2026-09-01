// Builds the social share images and the README banner.
//
//   node hack/branding/render-social.mjs
//
// Requires librsvg (`rsvg-convert`) on PATH. Output is committed, so this only
// needs running when the artwork changes.
import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { build } from './lib/sky-art.mjs';

const root = new URL('../..', import.meta.url).pathname;
const out = join(root, 'docs/assets');
const scratch = join(tmpdir(), `presage-social-${process.pid}`);

const VARIANTS = [
  // The README banner. Wide and short so it does not push the prose below the
  // fold on a laptop.
  { name: 'banner', width: 1600, height: 480, markScale: 0.34, wordScale: 0.074 },
  // The universal social card: Discord, Slack, LinkedIn, Facebook, and
  // Twitter's summary_large_image all read this one.
  { name: 'og', width: 1200, height: 630, markScale: 0.34, wordScale: 0.085 },
  /* Square renders small and is often cropped to a circle, where a tagline is
   * unreadable and the first thing clipped. */
  { name: 'og-square', width: 1200, height: 1200, markScale: 0.46, wordScale: 0.11, showTagline: false },
];

mkdirSync(out, { recursive: true });
mkdirSync(scratch, { recursive: true });

try {
  for (const variant of VARIANTS) {
    const svgPath = join(scratch, `${variant.name}.svg`);
    writeFileSync(svgPath, build(variant));
    execFileSync('rsvg-convert', [
      '-w', String(variant.width),
      '-h', String(variant.height),
      svgPath,
      '-o', join(out, `${variant.name}.png`),
    ]);
    console.log(`${variant.name}.png  ${variant.width}x${variant.height}`);
  }
} finally {
  rmSync(scratch, { recursive: true, force: true });
}
