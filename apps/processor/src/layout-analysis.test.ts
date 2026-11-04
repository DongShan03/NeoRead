import { describe, expect, it } from "vitest";
import { createCanvas } from "@napi-rs/canvas";
import * as ort from "onnxruntime-node";
import { detectRegions } from "./detect";
import { digest, model } from "./model";
import {
  assignText,
  decodeRegions,
  isAsset,
  readingRegions,
  readingTextSpans,
} from "./layout";
import forceFigures from "./fixtures/force-figure-groups.json";
import { orderPageBlocks } from "./reading-order";
import blocks from "./fixtures/oze-page7.json";

describe("detect", () => {
  // Opt-in: NEOREAD_TEST_LAYOUT_MODEL points at a downloaded copy of the pinned model.
  const path = process.env.NEOREAD_TEST_LAYOUT_MODEL;

  it.skipIf(!path)(
    "maps detections from the square model input back to the page",
    async () => {
      expect(await digest(path!)).toBe(model.sha256);
      const session = await ort.InferenceSession.create(path!);
      // A tall page catches decoding against page pixels instead of model input.
      const page = createCanvas(1000, 1400),
        c = page.getContext("2d");
      c.fillStyle = "white";
      c.fillRect(0, 0, 1000, 1400);
      c.fillStyle = "black";
      c.font = "bold 30px sans-serif";
      c.fillText("A Synthetic Study of Layout", 220, 110);
      c.font = "17px serif";
      const words =
        "the quick brown fox jumps over a lazy dog while reading layout models detect".split(
          " ",
        );
      for (let y = 180; y < 760; y += 22)
        c.fillText(
          Array.from(
            { length: 9 },
            (_, i) => words[(y + i * 7) % words.length],
          ).join(" "),
          90,
          y,
        );
      c.lineWidth = 3;
      c.strokeRect(150, 800, 700, 420);
      for (let i = 0; i < 8; i++) {
        c.fillStyle = `hsl(${i * 40},70%,50%)`;
        c.fillRect(200 + i * 80, 1180 - (i + 1) * 40, 50, (i + 1) * 40);
      }
      c.fillStyle = "black";
      c.fillText(
        "Figure 1: Bars rendered for a synthetic layout check.",
        260,
        1260,
      );
      const regions = await detectRegions(session, page);
      const chart = regions.find((r) => r.label === "chart");
      expect(chart?.bounds.x).toBeCloseTo(0.15, 1);
      expect(chart?.bounds.y).toBeCloseTo(800 / 1400, 1);
      expect(chart?.bounds.width).toBeCloseTo(0.7, 1);
      expect(chart?.bounds.height).toBeCloseTo(420 / 1400, 1);
      const text = regions.find((r) => r.label === "text");
      expect(text!.order).toBeLessThan(chart!.order);
    },
    30000,
  );
});

describe("layout", () => {
  describe("layout boundary", () => {
    it("rejects invalid boxes, clips coordinates and retains logical reading order", () => {
      const regions = decodeRegions(
        [
          22,
          0.9,
          -2,
          20,
          70,
          40,
          2,
          21,
          0.8,
          10,
          0,
          90,
          18,
          1,
          3,
          0.99,
          NaN,
          0,
          100,
          10,
          0,
          22,
          0.4,
          0,
          0,
          50,
          50,
          0,
        ],
        100,
        100,
      );
      expect(regions.map((r) => r.label)).toEqual(["table", "text"]);
      expect(regions[1].bounds.x).toBe(0);
    });
    it("suppresses duplicate detections without dropping separate nearby blocks", () => {
      expect(
        decodeRegions(
          [
            21, 0.9, 0, 0, 80, 30, 0, 21, 0.8, 1, 1, 80, 30, 1, 21, 0.8, 0, 35,
            80, 70, 2,
          ],
          100,
          100,
        ),
      ).toHaveLength(2);
    });
    it("preserves unmatched text and does not duplicate spans in overlapping blocks", () => {
      const regions = decodeRegions(
        [22, 0.9, 0, 0, 50, 50, 0, 22, 0.8, 20, 20, 70, 70, 1],
        100,
        100,
      );
      const result = assignText(regions, [
        { text: "shared", bounds: { x: 0.3, y: 0.3, width: 0.1, height: 0.1 } },
        {
          text: "outside",
          bounds: { x: 0.8, y: 0.8, width: 0.1, height: 0.1 },
        },
      ]);
      expect(result.texts.join(" ").match(/shared/g)).toHaveLength(1);
      expect(result.unmatched).toBe("outside");
    });
  });

  it("keeps DMN Figure 5's text-filled panels inside the original graphic", () => {
    const panels = [
      { x: 0.133937, y: 0.277039, width: 0.725807, height: 0.050037 },
      { x: 0.136018, y: 0.091118, width: 0.725807, height: 0.119998 },
      { x: 0.133937, y: 0.218965, width: 0.725807, height: 0.049999 },
    ];
    const regions = [
      {
        label: "image",
        confidence: 0.9,
        order: 1,
        bounds: { x: 0.137695, y: 0.093048, width: 0.714355, height: 0.112 },
      },
      {
        label: "figure_title",
        confidence: 0.9,
        order: 2,
        bounds: { x: 0.140137, y: 0.225647, width: 0.712891, height: 0.039673 },
      },
      {
        label: "figure_title",
        confidence: 0.9,
        order: 3,
        bounds: { x: 0.140381, y: 0.282455, width: 0.719238, height: 0.039581 },
      },
      {
        label: "figure_title",
        confidence: 0.9,
        order: 4,
        bounds: { x: 0.115234, y: 0.340385, width: 0.769531, height: 0.029678 },
      },
      {
        label: "text",
        confidence: 0.9,
        order: 5,
        bounds: { x: 0.1, y: 0.4, width: 0.8, height: 0.1 },
      },
    ];
    const texts = [
      "Input image sequence:",
      "Input text: diagram instructions",
      "GPT-5 output: diagram example",
      "Figure 5: An example of input sequence and output.",
      "Ordinary prose after the figure.",
    ];
    const spans = regions.map((r, i) => ({ text: texts[i], bounds: r.bounds }));
    const output = readingRegions(regions, spans, panels);
    const image = output.find((r) => r.label === "image")!;
    expect(image.bounds.y).toBeLessThanOrEqual(panels[1].y);
    expect(image.bounds.y + image.bounds.height).toBeGreaterThanOrEqual(
      panels[0].y + panels[0].height,
    );
    expect(
      output.filter((r) => r.label === "figure_title").map((r) => r.text),
    ).toEqual([texts[3]]);
    expect(output.some((r) => r.label === "text" && r.text === texts[4])).toBe(
      true,
    );
  });

  it("does not turn ordinary boxed prose into an image without a detected figure", () => {
    const regions = [
      {
        label: "text",
        confidence: 0.9,
        order: 1,
        bounds: { x: 0.1, y: 0.1, width: 0.8, height: 0.2 },
      },
      {
        label: "figure_title",
        confidence: 0.9,
        order: 2,
        bounds: { x: 0.1, y: 0.32, width: 0.8, height: 0.04 },
      },
    ];
    const output = readingRegions(
      regions,
      [
        { text: "A theorem and its explanation.", bounds: regions[0].bounds },
        { text: "Figure 1: A following example.", bounds: regions[1].bounds },
      ],
      [regions[0].bounds],
    );
    expect(output.some((r) => r.label === "image")).toBe(false);
    expect(output.some((r) => r.text.includes("theorem"))).toBe(true);
  });

  it.each([1, 5, 10])(
    "removes gutter line numbers with step %s while retaining table numbers",
    (step) => {
      const numbers = [1, 2, 3, 4].map((n) => ({
        text: String(n * step),
        bounds: { x: 0.045, y: 0.2 + n * 0.02, width: 0.015, height: 0.01 },
      }));
      const text = {
        text: "Normal paragraph 32.",
        bounds: { x: 0.12, y: 0.2, width: 0.5, height: 0.1 },
      };
      expect(readingTextSpans([], [...numbers, text])).toEqual([text]);
      expect(
        readingTextSpans(
          [
            {
              label: "table",
              order: 1,
              confidence: 0.9,
              bounds: { x: 0.03, y: 0.2, width: 0.7, height: 0.2 },
            },
          ],
          numbers,
        ),
      ).toEqual(numbers);
      expect(
        readingTextSpans(
          [],
          numbers.map((s) => ({ ...s, bounds: { ...s.bounds, x: 0.2 } })),
        ),
      ).toHaveLength(4);
    },
  );

  // Actual PP-DocLayoutV3 detections and PDF.js labels from FORCE figures 3 and 4.
  it.each(forceFigures)(
    "keeps the complete FORCE figure on page $page",
    ({ detections, spans, titles }) => {
      const regions = readingRegions(
        decodeRegions(detections, 800, 800),
        spans,
      );
      const figures = regions.filter((r) =>
        ["chart", "image"].includes(r.label),
      );
      expect(figures).toHaveLength(1);
      for (const title of titles) expect(figures[0].text).toContain(title);
      const caption = regions.find(
        (r) => r.label === "figure_title" && /^Figure [34]\./.test(r.text),
      );
      expect(caption).toBeDefined();
      expect(
        figures[0].bounds.y + figures[0].bounds.height,
      ).toBeLessThanOrEqual(caption!.bounds.y);
      expect(figures[0].text).not.toContain(caption!.text);
    },
  );

  it("discards an isolated low-confidence graphic without a numbered caption", () => {
    expect(
      readingRegions(decodeRegions([3, 0.4, 10, 10, 90, 40, 0], 100, 100), []),
    ).toEqual([]);
  });

  it("does not join panels across intervening prose", () => {
    const regions = decodeRegions(
      [
        3, 0.9, 10, 5, 45, 15, 0, 22, 0.9, 10, 16, 45, 19, 1, 3, 0.9, 10, 20,
        45, 30, 2, 7, 0.9, 10, 32, 45, 36, 3,
      ],
      100,
      100,
    );
    const blocks = readingRegions(regions, [
      {
        text: "Intervening paragraph",
        bounds: { x: 0.11, y: 0.16, width: 0.3, height: 0.03 },
      },
      {
        text: "Figure 2. Separate figure",
        bounds: { x: 0.11, y: 0.32, width: 0.3, height: 0.03 },
      },
    ]);
    expect(blocks.filter((b) => b.label === "chart")).toHaveLength(2);
  });

  it("keeps inline symbols in prose and groups panels under their shared numbered caption", () => {
    const r = decodeRegions(
      [
        3, 0.9, 10, 10, 30, 30, 0, 3, 0.9, 60, 10, 80, 30, 1, 7, 0.9, 8, 33, 83,
        40, 2, 22, 0.9, 10, 50, 80, 70, 3, 15, 0.9, 20, 55, 25, 60, 4,
      ],
      100,
      100,
    );
    const blocks = readingRegions(r, [
      {
        text: "Figure 1: comparison",
        bounds: { x: 0.1, y: 0.34, width: 0.7, height: 0.05 },
      },
      { text: "prose", bounds: { x: 0.1, y: 0.51, width: 0.1, height: 0.05 } },
      { text: "x", bounds: { x: 0.2, y: 0.55, width: 0.05, height: 0.05 } },
    ]);
    expect(blocks).toHaveLength(3);
    expect(blocks[0].members).toEqual([0, 1]);
    expect(blocks[0].bounds.height).toBeCloseTo(0.2);
    expect(blocks[1].bounds).toEqual(r[2].bounds);
    expect(blocks[1].text).toBe("Figure 1: comparison");
    expect(blocks[2].text).toBe("prose x");
  });

  it("does not merge neighboring figures with different captions", () => {
    const r = decodeRegions(
      [
        3, 0.9, 5, 5, 40, 20, 0, 3, 0.9, 60, 5, 90, 20, 1, 7, 0.9, 5, 23, 40,
        30, 2, 7, 0.9, 60, 23, 90, 30, 3,
      ],
      100,
      100,
    );
    const blocks = readingRegions(r, [
      {
        text: "Figure 1: left",
        bounds: { x: 0.06, y: 0.24, width: 0.3, height: 0.05 },
      },
      {
        text: "Figure 2: right",
        bounds: { x: 0.61, y: 0.24, width: 0.28, height: 0.05 },
      },
    ]);
    expect(blocks).toHaveLength(4);
    expect(
      blocks.filter((b) => b.members).map((b) => b.members?.length),
    ).toEqual([1, 1]);
  });

  it("does not absorb an adjacent uncaptioned chart into a narrow caption", () => {
    const r = decodeRegions(
      [
        3, 0.9, 52, 10, 71, 20, 0, 3, 0.9, 72, 10, 91, 20, 1, 7, 0.9, 51, 22,
        71, 25, 2,
      ],
      100,
      100,
    );
    const blocks = readingRegions(r, [
      {
        text: "Figure 11: first chart",
        bounds: { x: 0.52, y: 0.23, width: 0.18, height: 0.01 },
      },
    ]);
    expect(blocks).toHaveLength(3);
    expect(blocks.find((b) => b.members)?.bounds.width).toBeLessThan(0.22);
  });

  it("expands a grouped crop to include full subfigure captions", () => {
    const r = decodeRegions(
      [
        3, 0.9, 15, 5, 40, 20, 0, 7, 0.9, 10, 21, 41, 25, 1, 7, 0.9, 15, 28, 40,
        32, 2,
      ],
      100,
      100,
    );
    const blocks = readingRegions(r, [
      {
        text: "(a) wide subcaption",
        bounds: { x: 0.11, y: 0.22, width: 0.29, height: 0.02 },
      },
      {
        text: "Figure 1: main",
        bounds: { x: 0.16, y: 0.29, width: 0.23, height: 0.02 },
      },
    ]);
    expect(blocks).toHaveLength(2);
    expect(blocks[0].bounds.x).toBe(0.1);
    expect(blocks[0].bounds.width).toBeCloseTo(0.31);
  });

  it("keeps the lines and indentation of algorithm blocks", () => {
    const region = (label: string, y: number) => ({
      label,
      confidence: 1,
      order: 0,
      bounds: { x: 0.05, y, width: 0.6, height: 0.2 },
    });
    // Every glyph is 0.005 wide, so a 0.025 gap is five spaces.
    const span = (text: string, x: number, y: number) => ({
      text,
      bounds: { x, y, width: text.length * 0.005, height: 0.01 },
    });
    const { texts } = assignText(
      [region("algorithm", 0.05), region("text", 0.5)],
      [
        span("2", 0.1, 0.14),
        span("latch(record)", 0.15, 0.14),
        span("Algorithm 1: Read phase", 0.1, 0.1),
        span("1", 0.1, 0.12),
        span("Function read(txn, record)", 0.13, 0.12),
        span("Prose wraps", 0.1, 0.55),
        span("onto one line.", 0.1, 0.57),
      ],
    );
    expect(texts[0]).toBe(
      "Algorithm 1: Read phase\n1     Function read(txn, record)\n2         latch(record)",
    );
    expect(texts[1]).toBe("Prose wraps onto one line.");
  });

  it("keeps algorithm regions as standalone image assets with searchable text", () => {
    const regions = decodeRegions([1, 0.886, 10, 10, 90, 70, 0], 100, 100);
    const blocks = readingRegions(regions, [
      {
        text: "Algorithm 1: Read phase",
        bounds: { x: 0.12, y: 0.12, width: 0.5, height: 0.03 },
      },
      {
        text: "latch(record)",
        bounds: { x: 0.15, y: 0.2, width: 0.3, height: 0.03 },
      },
    ]);
    expect(blocks).toHaveLength(1);
    expect(blocks[0].label).toBe("algorithm");
    expect(blocks[0].text).toContain("latch(record)");
    expect(isAsset(blocks[0].label)).toBe(true);
  });

  it("keeps a table caption above the table as a separate paragraph at its original bounds", () => {
    const raw = decodeRegions(
      [7, 0.9, 10, 10, 90, 20, 0, 21, 0.9, 10, 22, 90, 65, 1],
      100,
      100,
    );
    const blocks = readingRegions(raw, [
      {
        text: "Table 5: Results",
        bounds: { x: 0.1, y: 0.11, width: 0.6, height: 0.08 },
      },
      {
        text: "Model 90.1",
        bounds: { x: 0.1, y: 0.3, width: 0.6, height: 0.08 },
      },
    ]);
    expect(blocks).toHaveLength(2);
    expect(blocks[0]).toMatchObject({
      label: "figure_title",
      text: "Table 5: Results",
      bounds: raw[0].bounds,
    });
    expect(blocks[1]).toMatchObject({
      label: "table",
      text: "Model 90.1",
      bounds: raw[1].bounds,
    });
    expect(blocks[1].caption).toBeUndefined();
  });
});

describe("reading-order", () => {
  it("moves the premature heading below Graph without changing IDs or boxes", () => {
    const ordered = orderPageBlocks(blocks);
    expect(ordered.map((b) => b.id)).toEqual([
      "p7-b2",
      "p7-b3",
      "p7-b5",
      "p7-b4",
      "p7-b6",
      "p7-b7",
    ]);
    expect(new Set(ordered)).toEqual(new Set(blocks));
  });

  it("retains left-before-right columns around spanning bands", () => {
    const block = (
      id: string,
      x: number,
      y: number,
      width: number,
      height: number,
    ) => ({ id, bounds: { x, y, width, height } });
    const title = block("title", 0.1, 0.05, 0.8, 0.05),
      left1 = block("l1", 0.1, 0.2, 0.35, 0.15),
      left2 = block("l2", 0.1, 0.4, 0.35, 0.15),
      right1 = block("r1", 0.55, 0.2, 0.35, 0.15),
      right2 = block("r2", 0.55, 0.4, 0.35, 0.15),
      wide = block("wide", 0.1, 0.6, 0.8, 0.1),
      end = block("end", 0.1, 0.8, 0.35, 0.1);
    expect(
      orderPageBlocks([title, left2, left1, right2, right1, wide, end]).map(
        (b) => b.id,
      ),
    ).toEqual(["title", "l1", "l2", "r1", "r2", "wide", "end"]);
  });
});
