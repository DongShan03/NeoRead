import { expect, it } from "vitest";
import { pdfFontAscent } from "./pdf-text";
import { isPDFPageDecoration } from "./pdf-content";
it("falls back for zero or invalid ascent while honoring usable font metrics", () => {
  expect(pdfFontAscent({ ascent: 0, descent: 0 })).toBe(0.8);
  expect(pdfFontAscent({ ascent: 0, descent: -0.25 })).toBe(0.75);
  expect(pdfFontAscent({ ascent: 0.714 })).toBe(0.714);
  expect(pdfFontAscent({ ascent: NaN, descent: -2 })).toBe(0.8);
  expect(pdfFontAscent()).toBe(0.8);
});

it("excludes arXiv margin stamps while retaining arXiv references in body text", () => {
  const margin = {
    label: "text",
    text: "arXiv:2605.18915v1 [cs.CR]",
    bounds: { x: 0.02, y: 0.2, width: 0.02, height: 0.3 },
  };
  expect(isPDFPageDecoration(margin)).toBe(true);
  expect(
    isPDFPageDecoration({
      ...margin,
      text: margin.text + " 12 May 2026",
      bounds: { ...margin.bounds, x: 0.95 },
    }),
  ).toBe(true);
  expect(
    isPDFPageDecoration({
      ...margin,
      bounds: { x: 0.15, y: 0.2, width: 0.6, height: 0.02 },
    }),
  ).toBe(false);
  expect(
    isPDFPageDecoration({
      ...margin,
      text: "See " + margin.text + " for details.",
    }),
  ).toBe(false);
});
