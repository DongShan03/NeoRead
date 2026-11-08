import type { PDFLocation } from "@neoread/core";

export type PDFRect = NonNullable<PDFLocation["rects"]>[number];

/**
 * Join neighbouring rects into visual lines without changing saved geometry.
 * pageAspectRatio is displayed page width / height, including page rotation.
 * Each line uses the union so every selected glyph stays covered and shares
 * one bottom edge for underlines.
 */
export function lineBoxes(
  rects: readonly PDFRect[],
  pageAspectRatio: number,
): PDFRect[] {
  const lines: PDFRect[] = [];
  // Left-to-right insertion keeps top/height jitter from splitting a run into
  // disconnected fragments before the intervening glyphs are visited.
  for (const rect of [...rects].sort((a, b) => a.x - b.x || a.y - b.y)) {
    const line = lines.find((line) => {
      const height = Math.min(line.height, rect.height);
      const overlap =
        Math.min(line.y + line.height, rect.y + rect.height) -
        Math.max(line.y, rect.y);
      const gap =
        Math.max(line.x, rect.x) -
        Math.min(line.x + line.width, rect.x + rect.width);
      // Heights are fractions of page height, gaps of page width. Join gaps
      // up to 0.65 line heights in physical page coordinates.
      return overlap > height / 2 && gap <= (height * 0.65) / pageAspectRatio;
    });
    if (!line) {
      lines.push({ ...rect });
      continue;
    }
    const right = Math.max(line.x + line.width, rect.x + rect.width);
    const bottom = Math.max(line.y + line.height, rect.y + rect.height);
    line.x = Math.min(line.x, rect.x);
    line.y = Math.min(line.y, rect.y);
    line.width = right - line.x;
    line.height = bottom - line.y;
  }
  return lines.sort((a, b) => a.y - b.y || a.x - b.x);
}
