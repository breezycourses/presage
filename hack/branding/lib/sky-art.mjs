// presage's sky, as art that can be rasterised at any size.
//
// Structurally a port of the Breezy asset pipeline (packages/assets), with
// three deliberate departures:
//
//   * The gradient is dark. presage is infrastructure tooling, and the mark
//     needs to hold its edge against a README rendered in either theme.
//   * The clouds are drawn procedurally rather than composited from raster
//     art. That keeps this repository self-contained and free of another
//     project's brand assets, which matters for something Apache-2.0.
//   * There is no character. The mark is a forecast curve with a widening
//     uncertainty band, which is literally what presage computes.
//
// Requires librsvg (`rsvg-convert`) on PATH. No ImageMagick: nothing here is
// raster.

/** Top-to-bottom sky. Dark enough that light grey clouds read as clouds. */
export const GRADIENT = [
  ['0%', '#05070D'],
  ['28%', '#0A1122'],
  ['56%', '#111B33'],
  ['82%', '#18263F'],
  ['100%', '#1F3050'],
];

export const CLOUD_COLOR = '#C7CEDA';

/* Clouds as fractions of the canvas, so one layout serves every aspect.
 * Kept clear of the middle band: the mark sits there and needs the darkest
 * part of the sky behind it to hold contrast. */
export const CLOUDS = [
  { x: -0.06, y: 0.06, w: 0.30, opacity: 0.16, seed: 1 },
  { x: 0.78, y: 0.02, w: 0.26, opacity: 0.13, seed: 2, flip: true },
  { x: 0.16, y: 0.20, w: 0.18, opacity: 0.09, seed: 6, flip: true },
  { x: -0.04, y: 0.70, w: 0.28, opacity: 0.20, seed: 3 },
  { x: 0.74, y: 0.66, w: 0.32, opacity: 0.18, seed: 4, flip: true },
  { x: 0.30, y: 0.84, w: 0.26, opacity: 0.12, seed: 5 },
];

/* A deterministic generator: the same banner has to come out of the same
 * commit, or every regeneration is a spurious diff in review. */
function rng(seed) {
  let s = seed * 1103515245 + 12345;
  return () => {
    s = (s * 1103515245 + 12345) & 0x7fffffff;
    return s / 0x7fffffff;
  };
}

/**
 * One cloud, as overlapping ellipses under a blur. Cumulus is mostly a
 * silhouette of stacked lobes, so a handful of ellipses with a soft edge
 * reads as cloud at banner scale without any raster art.
 */
function cloud({ x, y, w, opacity, seed, flip }, index, height) {
  const rand = rng(seed);
  const h = w * 0.46;
  const lobes = [];

  // A base slab keeps the underside flat, the way a cumulus bottom is.
  lobes.push({ cx: w * 0.5, cy: h * 0.70, rx: w * 0.42, ry: h * 0.17 });

  // More, smaller lobes than the obvious handful: the lobed silhouette is what
  // makes a cloud legible, and a few fat ellipses under a heavy blur just read
  // as a smudge.
  for (let i = 0; i < 11; i++) {
    const t = i / 10;
    const arch = Math.sin(Math.PI * t) ** 0.75;
    lobes.push({
      cx: w * (0.12 + 0.76 * t + (rand() - 0.5) * 0.05),
      cy: h * (0.66 - arch * 0.36 + (rand() - 0.5) * 0.07),
      rx: w * (0.07 + rand() * 0.06) * (0.55 + arch * 0.75),
      ry: h * (0.13 + rand() * 0.1) * (0.6 + arch * 0.7),
    });
  }

  const shapes = lobes
    .map((l) => `<ellipse cx="${l.cx.toFixed(1)}" cy="${l.cy.toFixed(1)}" rx="${l.rx.toFixed(1)}" ry="${l.ry.toFixed(1)}"/>`)
    .join('');

  const transform = flip ? ` transform="translate(${w.toFixed(1)} 0) scale(-1 1)"` : '';
  return `<g transform="translate(${x.toFixed(1)} ${y.toFixed(1)})" opacity="${opacity}" filter="url(#soft${index})">
    <g fill="${CLOUD_COLOR}"${transform}>${shapes}</g>
  </g>`;
}

/**
 * The mark: a point forecast with a widening quantile band around it.
 *
 * The band widens to the right because that is what a predictive distribution
 * does as the horizon extends -- the picture is the product's actual claim,
 * not decoration.
 */
export function forecastMark({ width, height, id = 'mk', stroke = '#7AA7FF', band = '#3B6FD4' }) {
  const w = width;
  const h = height;
  const n = 64;
  const split = 0.52; // where observed history ends and the forecast begins

  /* Rising, because the case for forecasting is the ramp you would otherwise
   * serve one lead time late. The observed half carries some texture; the
   * forecast half is smooth, which is what a forecast looks like. */
  const curve = (t) => {
    const trend = 0.82 - 0.56 * Math.pow(t, 1.7);
    const texture = t < split ? 0.028 * Math.sin(t * Math.PI * 7.5) : 0;
    return h * (trend + texture);
  };

  const pts = [];
  for (let i = 0; i <= n; i++) pts.push([(i / n) * w, curve(i / n)]);

  const line = (from) =>
    pts
      .filter(([px]) => px >= from * w - 1e-6)
      .map(([px, py], i) => `${i === 0 ? 'M' : 'L'}${px.toFixed(1)},${py.toFixed(1)}`)
      .join(' ');

  // The band opens from the split point and widens with the horizon.
  const spread = (t) => (t <= split ? 0 : h * 0.42 * ((t - split) / (1 - split)) ** 1.25);
  const upper = pts.map(([px, py], i) => [px, py - spread(i / n)]);
  const lower = pts.map(([px, py], i) => [px, py + spread(i / n)]);
  const bandPath =
    upper.map(([px, py], i) => `${i === 0 ? 'M' : 'L'}${px.toFixed(1)},${py.toFixed(1)}`).join(' ') +
    ' ' +
    lower
      .slice()
      .reverse()
      .map(([px, py]) => `L${px.toFixed(1)},${py.toFixed(1)}`)
      .join(' ') +
    ' Z';

  const splitX = (split * w).toFixed(1);

  return `
    <defs>
      <linearGradient id="${id}-band" x1="0" y1="0" x2="1" y2="0">
        <stop offset="${split}" stop-color="${band}" stop-opacity="0.55"/>
        <stop offset="0.82" stop-color="${band}" stop-opacity="0.30"/>
        <stop offset="1" stop-color="${band}" stop-opacity="0"/>
      </linearGradient>
      <linearGradient id="${id}-edge" x1="0" y1="0" x2="1" y2="0">
        <stop offset="${split}" stop-color="${band}" stop-opacity="0.75"/>
        <stop offset="1" stop-color="${band}" stop-opacity="0"/>
      </linearGradient>
      <linearGradient id="${id}-line" x1="0" y1="0" x2="1" y2="0">
        <stop offset="${split}" stop-color="${stroke}" stop-opacity="1"/>
        <stop offset="1" stop-color="${stroke}" stop-opacity="0.25"/>
      </linearGradient>
    </defs>
    <path d="${bandPath}" fill="url(#${id}-band)"/>
    <path d="${bandPath}" fill="none" stroke="url(#${id}-edge)" stroke-width="${(h * 0.012).toFixed(2)}"/>
    <line x1="${splitX}" y1="${(h * 0.06).toFixed(1)}" x2="${splitX}" y2="${(h * 0.94).toFixed(1)}"
          stroke="${stroke}" stroke-width="${(h * 0.01).toFixed(2)}" stroke-dasharray="${(h * 0.05).toFixed(1)} ${(h * 0.04).toFixed(1)}" opacity="0.45"/>
    <path d="${line(0)}" fill="none" stroke="${stroke}" stroke-width="${(h * 0.028).toFixed(2)}"
          stroke-linecap="round" stroke-linejoin="round" opacity="0.35"/>
    <path d="${line(split)}" fill="none" stroke="url(#${id}-line)" stroke-width="${(h * 0.032).toFixed(2)}"
          stroke-linecap="round" stroke-linejoin="round"/>
  `;
}

/**
 * @param showTagline banners are wide enough for a line of prose; the square
 *   crop is not, and a tagline is the first thing an avatar slot clips.
 */
export function build({ width, height, markScale, wordScale, showTagline = true, showClouds = true }) {
  const stops = GRADIENT.map(([o, c]) => `<stop offset="${o}" stop-color="${c}"/>`).join('');

  const clouds = showClouds
    ? CLOUDS.map((c, i) =>
        cloud(
          { ...c, x: c.x * width, y: c.y * height, w: c.w * width },
          i,
          height,
        ),
      ).join('')
    : '';

  const filters = CLOUDS.map(
    (c, i) =>
      `<filter id="soft${i}" x="-30%" y="-40%" width="160%" height="180%">
         <feGaussianBlur stdDeviation="${(width * 0.0045).toFixed(2)}"/>
       </filter>`,
  ).join('');

  const markW = width * markScale;
  const markH = markW * 0.30;
  const markX = (width - markW) / 2;

  const fontSize = width * wordScale;
  const taglineSize = fontSize * 0.25;

  /* Measured as a stack and centred as one block. Positioning each element
   * from the canvas centre independently leaves a hole under the mark and
   * pushes the tagline off the bottom edge. */
  const capHeight = fontSize * 0.72;
  const gapMarkWord = fontSize * 0.34;
  const gapWordTagline = taglineSize * 1.15;
  const taglineBlock = showTagline ? gapWordTagline + taglineSize * 0.75 : 0;

  const stack = markH + gapMarkWord + capHeight + taglineBlock;
  const top = (height - stack) / 2;

  const markY = top;
  const wordBaseline = top + markH + gapMarkWord + capHeight;
  const taglineBaseline = wordBaseline + gapWordTagline + taglineSize * 0.75;

  const tagline = showTagline
    ? `<text x="${width / 2}" y="${taglineBaseline.toFixed(1)}" text-anchor="middle"
             font-family="Helvetica Neue, Helvetica, Arial, sans-serif" font-size="${taglineSize.toFixed(1)}"
             fill="#8FA3C4" letter-spacing="${(taglineSize * 0.06).toFixed(2)}">forecast-driven autoscaling for Kubernetes and Agones</text>`
    : '';

  return `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">
  <defs>
    <linearGradient id="sky" x1="0" y1="0" x2="0" y2="1">${stops}</linearGradient>
    ${filters}
  </defs>
  <rect width="${width}" height="${height}" fill="url(#sky)"/>
  ${clouds}
  <g transform="translate(${markX.toFixed(1)} ${markY.toFixed(1)})">
    <svg width="${markW.toFixed(1)}" height="${markH.toFixed(1)}" viewBox="0 0 ${markW.toFixed(1)} ${markH.toFixed(1)}">
      ${forecastMark({ width: markW, height: markH })}
    </svg>
  </g>
  <text x="${width / 2}" y="${wordBaseline.toFixed(1)}" text-anchor="middle"
        font-family="Helvetica Neue, Helvetica, Arial, sans-serif" font-weight="600"
        font-size="${fontSize.toFixed(1)}" fill="#F2F5FA"
        letter-spacing="${(fontSize * 0.02).toFixed(2)}">presage</text>
  ${tagline}
</svg>`;
}
