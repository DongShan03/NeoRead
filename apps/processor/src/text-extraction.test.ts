import { describe, expect, it } from "vitest";
import { joinInlineText } from "./inline-math";
import { type TextSpan, assignText } from "./layout";
import omni from "./fixtures/omni-safety-inline-math.json";
import authors from "./fixtures/midas-author-footnotes.json";
import formally from "./fixtures/midas-sentence-math.json";
import { extractTextSpans, mathFontText } from "./text";
import { type PDFPageProxy } from "pdfjs-dist";
import { readFile } from "node:fs/promises";
import { getDocument, OPS } from "pdfjs-dist/legacy/build/pdf.mjs";
import { graphicPanels } from "./graphic-panels";
import { renderPage } from "./render-page";
import { repairTextBlocks } from "./repair";
import blocks from "./fixtures/oze-page7.json";

describe("inline-math", () => {
  it("combines MIDAS author footnotes and removes only the overprinted double dagger", () => {
    const text = joinInlineText(authors);
    expect(text).toContain(String.raw`Yilian Liu$^{1*}$`);
    expect(text).toContain(String.raw`Guoshun Nan$^{1\dagger}$`);
    expect(text).toContain(String.raw`Jiuyang Lyu$^{1\ddagger}$`);
    expect(text).not.toContain(String.raw`\ddagger \ddagger`);
    expect(text).toContain(String.raw`Zhongyi Zhai$^{3}$`);
  });

  it("keeps separately placed repeated footnote symbols", () => {
    expect(
      joinInlineText([
        span("Author Name", 0, 10, 10, 50, false),
        span("1", 50, 6, 7, 4, false),
        span("‡", 54, 6, 7, 4),
        span("‡", 58, 6, 7, 4),
      ]),
    ).toBe(String.raw`Author Name$^{1\ddagger \ddagger}$`);
  });

  it("keeps mathematical tau separate from Latin t and calligraphic T", () => {
    expect(
      joinInlineText([
        span("The threshold", 0, 10, 10, 50, false),
        span("𝜏", 52, 10, 10, 5),
        span("differs from", 60, 10, 10, 45, false),
        span("𝒯", 108, 10, 10, 6),
      ]),
    ).toBe(String.raw`The threshold $\tau $ differs from $\mathcal{T}$`);
    expect(joinInlineText([span("𝝉", 0, 10, 10, 5)])).toBe(
      String.raw`$\mathbf{\tau}$`,
    );
  });

  it("keeps MIDAS lower limits beside their braces and resolves the CMEX operator origin", async () => {
    const styles = Object.fromEntries(
      formally.spans.map((s, i) => [
        `f${i}`,
        { ascent: (s.baseline! - s.bounds.y) / s.fontSize! },
      ]),
    );
    const page = {
      getViewport: () => ({
        width: 1,
        height: 1,
        transform: [1, 0, 0, 1, 0, 0],
      }),
      getTextContent: async () => ({
        styles,
        items: formally.spans.map((s, i) => ({
          str: s.text,
          fontName: `f${i}`,
          transform: [s.fontSize, 0, 0, s.fontSize, s.bounds.x, s.baseline],
          width: s.bounds.width,
        })),
      }),
      getOperatorList: async () => ({}),
      commonObjs: {
        get: (id: string) => ({
          name:
            formally.spans[Number(id.slice(1))].text === "F"
              ? "CMEX10"
              : formally.spans[Number(id.slice(1))].math
                ? "CMMI10"
                : "Times-Roman",
        }),
      },
    } as unknown as PDFPageProxy;
    const spans = await extractTextSpans(page);
    const text = joinInlineText(spans);
    expect(text).toContain(String.raw`\{i_{k}\}_{k=1}^{n}`);
    expect(text).toContain(String.raw`\{q_{j}\}_{j=1}^{m}`);
    expect(text).toContain(String.raw`q=\bigsqcup _{j}q_{j}`);
    expect(text).not.toContain("Fk=1");
  });

  it("keeps Omni-Safety's mean bars and layer superscripts on h rather than v", () => {
    const text = joinInlineText(omni.spans);
    expect(text).toContain(
      String.raw`\mathbf{v}_{\mathrm{refu}}=\mathbf{v}_{\mathrm{text}}`,
    );
    expect(text).toContain(
      String.raw`\bar{\mathbf{h}}_{\mathrm{harm}}^{(l)}−\bar{\mathbf{h}}_{\mathrm{safe}}^{(l)}`,
    );
    expect(text).not.toContain(String.raw`¯^{(l)}`);
    expect(text).not.toContain(String.raw`\mathbf{v}^{(l)}`);
  });

  function span(
    text: string,
    x: number,
    baseline: number,
    fontSize: number,
    width: number,
    math = true,
  ): TextSpan {
    return {
      text,
      baseline,
      fontSize,
      math,
      bounds: { x, y: baseline - fontSize * 0.7, width, height: fontSize },
    };
  }

  it("keeps nested subscripts and accents on their actual glyphs across wrapped prose", () => {
    // Measurements from the reported paragraph, independent of a local PDF path.
    const spans = [
      span("By maximizing", 108, 357.71, 9.96, 218.81, false),
      span("ℳ", 328.82, 357.71, 9.96, 11.96),
      span("𝒯", 340.78, 359.21, 6.97, 4.52),
      span("s", 345.3, 360.2, 4.98, 3.34),
      span(",", 349.14, 359.21, 6.97, 2.37),
      span("𝒯", 351.5, 359.21, 6.97, 4.52),
      span("t", 356.02, 360.2, 4.98, 2.81),
      span(", each", 359.83, 357.71, 9.96, 22.84, false),
      span("˜", 385.21, 357.57, 9.96, 4.98),
      span("𝐱", 384.68, 357.71, 9.96, 6.05),
      span("s", 390.72, 354.1, 6.97, 3.76),
      span("i", 390.72, 360.31, 6.97, 2.82),
      span("effectively captures certain", 396.99, 357.71, 9.96, 107, false),
      span("semantic", 108, 368.62, 9.96, 35.42, false),
      span("ˆ", 146.44, 368.48, 9.96, 4.98),
      span("𝐱", 145.91, 368.62, 9.96, 6.05),
      span("t", 151.95, 365.01, 6.97, 3.01),
      span("i", 151.95, 371.21, 6.97, 2.82),
      span("from the target image.", 157.95, 368.62, 9.96, 88.36, false),
    ];
    const text = joinInlineText(spans);
    expect(text).toBe(
      "By maximizing $\\mathcal{M}_{\\mathcal{T}_{s},\\mathcal{T}_{t}}$ , each $\\tilde{\\mathbf{x}}_{i}^{s}$ effectively captures certain semantic $\\hat{\\mathbf{x}}_{i}^{t}$ from the target image.",
    );
    expect(text).not.toContain("\\tilde{\\mathcal{M}}");
  });

  it("uses prose baselines rather than font ascent to order mixed fonts", () => {
    const first = span("first", 0, 10, 10, 20, false),
      second = span("second", 22, 10, 10, 30, false),
      next = span("next line", 0, 21, 10, 40, false);
    second.bounds.y -= 3;
    expect(joinInlineText([next, second, first])).toBe(
      "first second next line",
    );
  });
});

describe("text-boundaries", () => {
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
      const originals = blocks.map((b) => ({
        ...b,
        page: 7,
        text: "old text",
      }));
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
        expect(
          repaired.blocks.find((b) => b.id === original.id)?.bounds,
        ).toEqual(original.bounds);
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

  it("filters a rotated arXiv margin stamp without removing a normal body reference", async () => {
    const page = {
      getViewport: () => ({
        width: 100,
        height: 100,
        transform: [1, 0, 0, 1, 0, 0],
      }),
      getTextContent: async () => ({
        styles: { f1: { ascent: 0.8 } },
        items: [
          {
            str: "arXiv:2605.18915v1 [cs.CR]",
            fontName: "f1",
            transform: [0, 10, -10, 0, 3, 30],
            width: 50,
          },
          {
            str: "See arXiv:2605.18915v1 [cs.CR].",
            fontName: "f1",
            transform: [10, 0, 0, 10, 20, 30],
            width: 60,
          },
        ],
      }),
      getOperatorList: async () => ({ fnArray: [], argsArray: [] }),
      commonObjs: { get: () => ({ name: "Times-Roman" }) },
    } as unknown as PDFPageProxy;
    expect((await extractTextSpans(page)).map((s) => s.text)).toEqual([
      "See arXiv:2605.18915v1 [cs.CR].",
    ]);
  });
});

describe("text-symbols", () => {
  it("preserves TeX mathematical alphabets without changing ordinary italic letters or symbols", () => {
    expect(mathFontText("T ∈ ∅ ∩", "VOGGQR+CMSY10")).toBe("𝒯 ∈ ∅ ∩");
    expect(mathFontText("B E F H I L M R", "CMSY7")).toBe("ℬ ℰ ℱ ℋ ℐ ℒ ℳ ℛ");
    expect(mathFontText("R C N Z", "XSBJNE+MSBM10")).toBe("ℝ ℂ ℕ ℤ");
    expect(mathFontText("T", "PFXWVE+CMMI10")).toBe("T");
    expect(mathFontText("x", "RTXNFC+CMBX10")).toBe("𝐱");
    expect(mathFontText("e", "YIEXCM+CMEX10")).toBe("˜");
    expect(mathFontText("c", "YIEXCM+CMEX10")).toBe("ˆ");
    expect(mathFontText("Text", "Times-Roman")).toBe("Text");
    expect(mathFontText("F", "OXAILR+CMEX10")).toBe("⨆");
    expect(mathFontText("F", "CMMI10")).toBe("F");
  });

  it("reads a CMSY font through PDF.js before assigning its extracted T to prose", async () => {
    // Same BaseFont/ASCII encoding as the reported paper; no external font asset.
    const objects = [
      "<< /Type /Catalog /Pages 2 0 R >>",
      "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
      "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
      "<< /Type /Font /Subtype /Type1 /BaseFont /VOGGQR+CMSY10 /Encoding << /Type /Encoding /Differences [84 /T] >> >>",
    ];
    const content = "BT /F1 10 Tf 152 590 Td (T) Tj ET";
    objects.push(
      `<< /Length ${content.length} >>\nstream\n${content}\nendstream`,
    );
    let pdf = "%PDF-1.7\n";
    const offsets = [0];
    objects.forEach((object, i) => {
      offsets.push(pdf.length);
      pdf += `${i + 1} 0 obj\n${object}\nendobj\n`;
    });
    const xref = pdf.length;
    pdf += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
    for (const offset of offsets.slice(1))
      pdf += `${String(offset).padStart(10, "0")} 00000 n \n`;
    pdf += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF`;
    const task = getDocument({
      data: new TextEncoder().encode(pdf),
      verbosity: 0,
    });
    try {
      const page = await (await task.promise).getPage(1);
      expect((await page.getTextContent()).items[0]).toMatchObject({
        str: "T",
      });
      expect((await extractTextSpans(page))[0].text).toBe("𝒯");
    } finally {
      await task.destroy();
    }
  });
});

it("renders a PDF paragraph crop with the same pixels as its full original page", async () => {
  const { DOMMatrix, ImageData, Path2D } = await import("@napi-rs/canvas");
  Object.assign(globalThis, { DOMMatrix, ImageData, Path2D });
  const task = getDocument({
    data: new Uint8Array(
      await readFile(
        new URL("../../web/public/samples/reading-notes.pdf", import.meta.url),
      ),
    ),
    useSystemFonts: true,
  });
  try {
    const page = await (await task.promise).getPage(1);
    const bounds = { x: 0.1, y: 0.1, width: 0.6, height: 0.3 };
    const full = await renderPage(page);
    const cropped = await renderPage(page, bounds);
    const actual = page.getViewport({ scale: full.scale });
    const x = Math.floor(bounds.x * actual.width),
      y = Math.floor(bounds.y * actual.height);
    expect(cropped.canvas.width).toBe(
      Math.ceil((bounds.x + bounds.width) * actual.width) - x,
    );
    expect(cropped.canvas.height).toBe(
      Math.ceil((bounds.y + bounds.height) * actual.height) - y,
    );
    const expected = full.canvas
      .getContext("2d")
      .getImageData(x, y, cropped.canvas.width, cropped.canvas.height).data;
    const pixels = cropped.canvas
      .getContext("2d")
      .getImageData(0, 0, cropped.canvas.width, cropped.canvas.height).data;
    expect(pixels).toEqual(expected);
    full.canvas.width =
      full.canvas.height =
      cropped.canvas.width =
      cropped.canvas.height =
        1;
    await expect(
      renderPage(page, { x: -0.1, y: 0, width: 0.2, height: 0.2 }),
    ).rejects.toThrow("Invalid crop");
  } finally {
    await task.destroy();
  }
});
