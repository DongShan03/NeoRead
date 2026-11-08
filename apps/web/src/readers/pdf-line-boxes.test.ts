import { describe, expect, it } from "vitest";
import { lineBoxes } from "./pdf-line-boxes";

describe("PDF line boxes", () => {
  it("merges CJK glyphs with top and height jitter into their union", () => {
    expect(
      lineBoxes(
        [
          { x: 0.16, y: 0.102, width: 0.018, height: 0.02 },
          { x: 0.1, y: 0.101, width: 0.018, height: 0.02 },
          { x: 0.18, y: 0.1, width: 0.018, height: 0.022 },
          { x: 0.14, y: 0.103, width: 0.018, height: 0.021 },
          { x: 0.12, y: 0.1, width: 0.018, height: 0.019 },
        ],
        0.75,
      ),
    ).toEqual([
      {
        x: 0.1,
        y: 0.1,
        width: expect.closeTo(0.098),
        height: expect.closeTo(0.024),
      },
    ]);
  });

  it("merges overlapping and duplicate boxes", () => {
    const rect = { x: 0.1, y: 0.101, width: 0.25, height: 0.019 };
    expect(
      lineBoxes(
        [{ x: 0.3, y: 0.1, width: 0.2, height: 0.02 }, rect, rect],
        0.75,
      ),
    ).toEqual([
      {
        x: 0.1,
        y: 0.1,
        width: expect.closeTo(0.4),
        height: expect.closeTo(0.02),
      },
    ]);
  });

  it("keeps same-height columns apart", () => {
    const left = { x: 0.08, y: 0.4, width: 0.38, height: 0.02 };
    const right = { x: 0.54, y: 0.401, width: 0.38, height: 0.02 };
    expect(lineBoxes([right, left], 0.75)).toEqual([left, right]);
    expect(
      lineBoxes([left, { ...right, x: left.x + left.width + 0.005 }], 0.75),
    ).toHaveLength(1);
  });

  it("returns separate boxes in reading order for multiline selections", () => {
    const first = { x: 0.1, y: 0.1, width: 0.2, height: 0.02 };
    const second = { x: 0.1, y: 0.14, width: 0.3, height: 0.02 };
    expect(
      lineBoxes(
        [second, { x: 0.302, y: 0.101, width: 0.1, height: 0.019 }, first],
        0.75,
      ),
    ).toEqual([
      { ...first, width: expect.closeTo(0.302), height: expect.closeTo(0.02) },
      second,
    ]);
  });

  it.each([0.5, 2])(
    "scales the gap to line height on pages with aspect ratio %s",
    (aspectRatio) => {
      const left = { x: 0.1, y: 0.1, width: 0.1, height: 0.02 };
      const right = (gap: number) => ({
        ...left,
        x: left.x + left.width + (left.height * gap) / aspectRatio,
      });
      expect(lineBoxes([left, right(0.6)], aspectRatio)).toHaveLength(1);
      expect(lineBoxes([left, right(0.8)], aspectRatio)).toHaveLength(2);
    },
  );

  it("requires vertical overlap greater than half the smaller height", () => {
    const first = { x: 0.1, y: 0, width: 0.1, height: 0.02 };
    expect(lineBoxes([first, { ...first, y: 0.01 }], 1)).toHaveLength(2);
    expect(lineBoxes([first, { ...first, y: 0.009 }], 1)).toHaveLength(1);
  });

  it("leaves saved rectangles and their order untouched", () => {
    const rects = Object.freeze([
      Object.freeze({ x: 0.2, y: 0.1, width: 0.1, height: 0.02 }),
      Object.freeze({ x: 0.1, y: 0.1, width: 0.1, height: 0.02 }),
    ]);
    expect(lineBoxes(rects, 0.75)).toHaveLength(1);
    expect(rects[0].x).toBe(0.2);
    expect(rects[1].width).toBe(0.1);
    expect(lineBoxes([], 0.75)).toEqual([]);
  });
});
