// The app's design tokens (frontend/tokens.css) as numbers three.js can use. Tokens are written in oklch, which
// THREE.Color cannot parse, so each one is painted on a 1×1 canvas and read back as sRGB. Read once per style
// change; the fallbacks are the twin's classic constants, so a page without the tokens still draws.

export type TwinTheme = {
  paper: number;
  ink: number;
  dim: number;
  accent: number;
  blue: number;
  warning: number;
  danger: number;
};

const FALLBACK: TwinTheme = { paper: 0x0b1418, ink: 0xe6f2ee, dim: 0x5d7279, accent: 0xa7f3d0, blue: 0x80b7ff, warning: 0xf6c177, danger: 0xff8f70 };

const TOKENS: Record<keyof TwinTheme, string> = {
  paper: "--color-paper", ink: "--color-ink", dim: "--color-dim", accent: "--color-accent", blue: "--color-blue", warning: "--color-warning", danger: "--color-danger",
};

/** Any CSS colour → 0xRRGGBB (null when the browser cannot paint it). */
export function cssColor(value: string, ctx?: CanvasRenderingContext2D | null): number | null {
  const v = value.trim();
  if (!v) return null;
  const c = ctx ?? document.createElement("canvas").getContext("2d", { willReadFrequently: true });
  if (!c) return null;
  c.clearRect(0, 0, 1, 1);
  // An unparseable value leaves fillStyle unchanged; start from a sentinel so that case is detectable.
  c.fillStyle = "#010203";
  c.fillStyle = v;
  if (c.fillStyle === "#010203" && v.toLowerCase() !== "#010203") return null;
  c.fillRect(0, 0, 1, 1);
  const [r, g, b, a] = c.getImageData(0, 0, 1, 1).data;
  if (a === 0) return null;
  return (r << 16) | (g << 8) | b;
}

/** The current theme, read from the document's custom properties. */
export function readTheme(): TwinTheme {
  if (typeof document === "undefined") return { ...FALLBACK };
  const style = getComputedStyle(document.documentElement);
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 1;
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  const out = { ...FALLBACK };
  for (const key of Object.keys(TOKENS) as (keyof TwinTheme)[]) {
    const n = cssColor(style.getPropertyValue(TOKENS[key]), ctx);
    if (n !== null) out[key] = n;
  }
  return out;
}
