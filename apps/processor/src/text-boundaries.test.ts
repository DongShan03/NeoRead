import { readFile } from "node:fs/promises";
import { expect, it } from "vitest";
import { getDocument, OPS } from "pdfjs-dist/legacy/build/pdf.mjs";
import type { PDFPageProxy } from "pdfjs-dist";
import { graphicPanels } from "./graphic-panels";
import { repairTextBlocks } from "./repair";
import { assignText } from "./layout";
import { extractTextSpans } from "./text";
import blocks from "./fixtures/oze-page7.json";

it("keeps Record's final line inside its unchanged hover box in the actual Oze PDF", async () => {
  const task = getDocument({
    data: new Uint8Array(
      await readFile(
        new URL(
          "../../../docs/66_VLDB25- Oze_ Decentralized Graph-based Concurrency Control for Long-running Update Transactions.pdf",
          import.meta.url,
        ),
      ),
    ),
    useSystemFonts: true,
  });
  try {
    const page = await (await task.promise).getPage(7);
    const texts = assignText(blocks, await extractTextSpans(page, 2)).texts;
    expect(texts[1]).toMatch(/^Record:/);
    expect(texts[1]).toMatch(/and the status\.$/);
    expect(texts[3]).toMatch(/^Graph:/);
    expect(texts[3]).toContain("whose key is txid");
    expect(texts[4]).toMatch(/write protocols in the read phase below\.$/);
    expect(texts[5]).toMatch(/^Lines 1/);
    const originals = blocks.map((b) => ({ ...b, page: 7, text: "old text" }));
    const repaired = await repairTextBlocks(await task.promise, originals);
    expect(repaired.blocks.map((b) => b.id)).toEqual([
      "p7-b2",
      "p7-b3",
      "p7-b5",
      "p7-b4",
      "p7-b6",
      "p7-b7",
    ]);
    for (const original of originals)
      expect(repaired.blocks.find((b) => b.id === original.id)?.bounds).toEqual(
        original.bounds,
      );
    expect(originals.every((b) => b.text === "old text")).toBe(true);
  } finally {
    await task.destroy();
  }
});

it("reads native filled panels with graphics transforms without treating white fills or strokes as panels", async () => {
  const operators = [
    [OPS.save, []],
    [OPS.transform, [2, 0, 0, 1, 5, 0]],
    [OPS.setFillRGBColor, ["#fbe5d6"]],
    [
      OPS.constructPath,
      [
        OPS.fill,
        [new Float32Array([0, 2, 4])],
        new Float32Array([5, 25, 35, 45]),
      ],
    ],
    [OPS.paintFormXObjectBegin, [[1, 0, 0, 1, 10, 0]]],
    [
      OPS.constructPath,
      [
        OPS.fill,
        [new Float32Array([0, 2, 4])],
        new Float32Array([0, 50, 20, 70]),
      ],
    ],
    [OPS.paintFormXObjectEnd, []],
    [OPS.restore, []],
    [OPS.setFillRGBColor, ["#ffffff"]],
    [
      OPS.constructPath,
      [
        OPS.fill,
        [new Float32Array([0, 2, 4])],
        new Float32Array([10, 20, 80, 40]),
      ],
    ],
    [OPS.setFillRGBColor, ["#000000"]],
    [
      OPS.constructPath,
      [
        OPS.stroke,
        [new Float32Array([0, 2, 4])],
        new Float32Array([10, 20, 80, 40]),
      ],
    ],
  ];
  const page = {
    getViewport: () => ({
      width: 100,
      height: 100,
      transform: [1, 0, 0, -1, 0, 100],
    }),
    getOperatorList: async () => ({
      fnArray: operators.map((o) => o[0]),
      argsArray: operators.map((o) => o[1]),
    }),
  } as unknown as PDFPageProxy;
  const panels = await graphicPanels(page);
  expect(panels).toHaveLength(2);
  expect(panels[0].x).toBeCloseTo(0.15);
  expect(panels[0].y).toBeCloseTo(0.55);
  expect(panels[0].width).toBeCloseTo(0.6);
  expect(panels[0].height).toBeCloseTo(0.2);
  expect(panels[1].x).toBeCloseTo(0.25);
  expect(panels[1].y).toBeCloseTo(0.3);
});
