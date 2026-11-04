import { describe, expect, it } from "vitest";
import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import { readFile } from "node:fs/promises";
import { PDFDocument, PDFName, StandardFonts, degrees, rgb } from "pdf-lib";
import { type PDFBlock, type TranslationBlock } from "@neoread/core";
import { translatedPDF, type ExportCanvas } from "./pdf-export";
import { embedExportFonts, type FontBytes } from "./pdf-fonts";
import { preparePDFText, compileFormula } from "./pdf-math";
import { fitText, wrapText } from "./pdf-layout";
import { exportText } from "./pdf-content";
import { cubicGlyphPath } from "./pdf-svg-path";

describe("pdf-export", () => {
  const require = createRequire(new URL("../../package.json", import.meta.url));

  const processorRequire = createRequire(
    new URL("../../../processor/package.json", import.meta.url),
  );

  const { createCanvas } = processorRequire("@napi-rs/canvas") as {
    createCanvas(
      width: number,
      height: number,
    ): {
      width: number;
      height: number;
      getContext(type: "2d"): unknown;
    };
  };

  const fontBytes: Promise<FontBytes> = Promise.all([
    readFile(
      new URL(
        "../../public/pdf-assets/export-fonts/NotoSansSC-Regular.ttf",
        import.meta.url,
      ),
    ),
    readFile(
      require.resolve("pdfjs-dist/standard_fonts/LiberationSans-Regular.ttf"),
    ),
  ]).then(([cjk, latin]) => ({ cjk, latin }));

  function canvas(width: number, height: number): ExportCanvas {
    const c = createCanvas(width, height);
    return {
      canvas: c as unknown as HTMLCanvasElement,
      context: c.getContext("2d") as unknown as CanvasRenderingContext2D,
      dispose: () => {
        c.width = c.height = 1;
      },
    };
  }

  function translation(
    block: PDFBlock,
    target = "我们将译文放在原来的位置，保留页面布局。",
  ): TranslationBlock {
    return {
      blockId: block.id,
      status: "complete",
      sourceHash: createHash("sha256")
        .update(`zh-CN:v1:${block.text.trim()}`)
        .digest("hex"),
      sentences: [{ source: block.text, target }],
    };
  }

  async function fixture() {
    const doc = await PDFDocument.create();
    const font = await doc.embedFont(StandardFonts.Helvetica);
    const page = doc.addPage([400, 500]);
    page.drawText("Original paragraph on the left.", {
      x: 30,
      y: 450,
      size: 10,
      font,
    });
    page.drawText("Second line of source text.", {
      x: 30,
      y: 436,
      size: 10,
      font,
    });
    page.drawText("Right column stays here.", {
      x: 220,
      y: 450,
      size: 10,
      font,
    });
    page.drawRectangle({
      x: 30,
      y: 250,
      width: 100,
      height: 80,
      color: rgb(0.1, 0.5, 0.8),
    });
    page.drawText("E = mc2", { x: 220, y: 300, font, size: 15 });
    const blocks: PDFBlock[] = [
      {
        id: "left",
        page: 1,
        label: "text",
        bounds: {
          x: 30 / 400,
          y: 40 / 500,
          width: 165 / 400,
          height: 30 / 500,
        },
        text: "Original paragraph on the left. Second line of source text.",
      },
      {
        id: "right",
        page: 1,
        label: "text",
        bounds: {
          x: 220 / 400,
          y: 40 / 500,
          width: 150 / 400,
          height: 30 / 500,
        },
        text: "Right column stays here.",
      },
      {
        id: "figure",
        page: 1,
        label: "image",
        bounds: {
          x: 30 / 400,
          y: 170 / 500,
          width: 100 / 400,
          height: 80 / 500,
        },
        text: "",
        image: "figure.png",
      },
      {
        id: "formula",
        page: 1,
        label: "display_formula",
        bounds: {
          x: 220 / 400,
          y: 185 / 500,
          width: 100 / 400,
          height: 25 / 500,
        },
        text: "E = mc2",
      },
    ];
    return {
      doc,
      page,
      blocks,
      translations: blocks.slice(0, 2).map((b) => translation(b)),
    };
  }

  async function run(
    f: Awaited<ReturnType<typeof fixture>>,
    signal?: AbortSignal,
  ) {
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    return translatedPDF(
      pdfjs.getDocument,
      { data: await f.doc.save(), useSystemFonts: true },
      f.blocks,
      f.translations,
      await fontBytes,
      { signal, createCanvas: canvas },
    );
  }

  it("writes embedded Chinese text while keeping page size and vector graphics", async () => {
    const f = await fixture();
    const original = await f.doc.save();
    const result = await run(f);
    expect(result.translated).toBe(2);
    expect(result.issues).toEqual([]);
    expect(await f.doc.save()).toEqual(original);
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    const task = pdfjs.getDocument({
      data: result.bytes.slice(),
      useSystemFonts: true,
    });
    try {
      const pdf = await task.promise;
      expect(pdf.numPages).toBe(1);
      const page = await pdf.getPage(1);
      expect(page.getViewport({ scale: 1 })).toMatchObject({
        width: 400,
        height: 500,
      });
      const content = await page.getTextContent();
      expect(
        content.items.map((i) => ("str" in i ? i.str : "")).join(""),
      ).toContain("我们将译文");
      const surface = canvas(400, 500);
      await page.render({
        canvas: surface.canvas,
        canvasContext: surface.context,
        viewport: page.getViewport({ scale: 1 }),
      }).promise;
      expect(
        Array.from(surface.context.getImageData(60, 200, 1, 1).data).slice(
          0,
          3,
        ),
      ).toEqual([26, 128, 204]);
      // Unicode mappings can survive even when embedded composite glyphs are
      // broken. Check ink in each of the first six Chinese glyph cells as well.
      for (let glyph = 0; glyph < 6; glyph++) {
        const pixels = surface.context.getImageData(
          30 + glyph * 10,
          40,
          10,
          12,
        ).data;
        let ink = 0;
        for (let i = 0; i < pixels.length; i += 4) if (pixels[i] < 160) ink++;
        expect(ink, `missing glyph ${glyph}`).toBeGreaterThan(8);
      }
      surface.dispose();
    } finally {
      await task.destroy();
    }
  });

  it("does not mistake vertical margin text for a horizontal paragraph", async () => {
    const f = await fixture();
    f.page.drawText("Vertical margin text can be very long", {
      x: 12,
      y: 100,
      size: 10,
      rotate: degrees(90),
    });
    const result = await run(f);
    expect(result.translated).toBe(2);
    expect(result.issues).toEqual([]);
  });

  it("releases the rendering surface and loading task on cancellation", async () => {
    const f = await fixture();
    const controller = new AbortController();
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    let disposed = 0;
    await expect(
      translatedPDF(
        pdfjs.getDocument,
        { data: await f.doc.save(), useSystemFonts: true },
        f.blocks,
        f.translations,
        await fontBytes,
        {
          signal: controller.signal,
          createCanvas: (w, h) => {
            const result = canvas(w, h);
            controller.abort();
            return {
              ...result,
              dispose: () => {
                disposed++;
                result.dispose();
              },
            };
          },
        },
      ),
    ).rejects.toThrow();
    expect(disposed).toBe(1);
  });

  it.each([
    "untranslated",
    "stale",
    "overlap",
    "structure",
    "overflow",
    "bounds",
    "font",
    "background",
  ] as const)("preserves blocks when %s", async (reason) => {
    const f = await fixture();
    if (reason === "untranslated") f.translations[1].status = "running";
    if (reason === "stale") f.translations[1].sourceHash = "outdated";
    if (reason === "overlap")
      f.blocks.push({
        ...f.blocks[1],
        id: "protected",
        label: "image",
        image: "figure.png",
      });
    if (reason === "structure")
      f.translations[1].sentences[0].target = "独立公式 $$x^2$$ 保留原文";
    if (reason === "overflow")
      f.translations[1].sentences[0].target = "这是很长的段落。".repeat(150);
    if (reason === "bounds") f.blocks[1].bounds.x = -1;
    if (reason === "font") f.translations[1].sentences[0].target = "缺少字形🦄";
    if (reason === "background")
      f.page.drawRectangle({
        x: 220,
        y: 430,
        width: 150,
        height: 30,
        color: rgb(0.8, 0.9, 1),
      });
    const result = await run(f);
    expect(result.translated).toBe(1);
    expect(result.issues).toContainEqual({ page: 1, blockId: "right", reason });
  });

  it("removes only link hotspots covered by translated text", async () => {
    const f = await fixture();
    const link = (rect: number[]) =>
      f.doc.context.register(
        f.doc.context.obj({
          Type: "Annot",
          Subtype: "Link",
          Rect: rect,
          Border: [0, 0, 1],
          A: { S: "URI", URI: "https://example.com" },
        }),
      );
    f.page.node.set(
      PDFName.of("Annots"),
      f.doc.context.obj([link([30, 440, 80, 455]), link([30, 200, 80, 210])]),
    );
    const result = await run(f);
    const doc = await PDFDocument.load(result.bytes);
    expect(doc.getPage(0).node.Annots()?.size()).toBe(1);
    expect(f.page.node.Annots()?.size()).toBe(2);
  });

  it("exports the bundled paper with its saved translations and retains every page", async () => {
    const base = new URL(
      "../../../server/internal/neoread/example-paper/",
      import.meta.url,
    );
    const manifest = JSON.parse(
      await readFile(new URL("analysis/manifest.json", base), "utf8"),
    );
    const paper = JSON.parse(
      await readFile(new URL("paper.json", base), "utf8"),
    );
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    const result = await translatedPDF(
      pdfjs.getDocument,
      {
        data: new Uint8Array(await readFile(new URL("paper.pdf", base))),
        useSystemFonts: true,
      },
      manifest.blocks,
      paper.translations,
      await fontBytes,
      { createCanvas: canvas },
    );
    expect(result.issues.some((issue) => issue.blockId === "p5-b5")).toBe(
      false,
    );
    expect(
      result.issues.filter((issue) =>
        manifest.blocks.some(
          (b: PDFBlock) => b.caption && b.id === issue.blockId,
        ),
      ),
    ).toEqual([]);
    expect(result.pages).toBe(15);
    expect(result.translated).toBeGreaterThanOrEqual(105);
    expect(result.issues.some((issue) => issue.blockId === "p4-b8")).toBe(
      false,
    );
    // This exact paragraph was previously omitted solely because it contains h_t.
    expect(result.issues.some((issue) => issue.blockId === "p2-b3")).toBe(
      false,
    );
    const loading = pdfjs.getDocument({
      data: result.bytes.slice(),
      useSystemFonts: true,
    });
    try {
      const output = await loading.promise;
      const page = await output.getPage(2);
      const text = (await page.getTextContent()).items
        .map((item) => ("str" in item ? item.str : ""))
        .join("");
      for (const sentence of [
        "循环模型通常按照",
        "这些模型将各位置",
        "这种固有的顺序性",
        "近期研究通过分解技巧",
        "顺序计算这一根本限制依然存在",
      ])
        expect(text).toContain(sentence);
      expect(text).not.toMatch(/[\ue000-\uf8ff]/u);
      for (const block of manifest.blocks.filter((b: PDFBlock) => b.caption)) {
        const captionPage = await output.getPage(block.page);
        const captionText = (await captionPage.getTextContent()).items
          .map((item) => ("str" in item ? item.str : ""))
          .join("")
          .replace(/\s/g, "");
        const translated = paper.translations.find(
          (t: TranslationBlock) => t.blockId === block.id,
        );
        // Check actual emitted Chinese, not only the success counter. Formula-heavy
        // captions still have an ordinary opening sentence before their first math.
        const opening = translated.sentences[0].target
          .split(/[$\\]/)[0]
          .replace(/\s/g, "");
        expect(captionText).toContain(opening);
      }
      const matrixPage = await output.getPage(5);
      const matrixText = (await matrixPage.getTextContent()).items
        .map((item) => ("str" in item ? item.str : ""))
        .join("");
      expect(matrixText).toContain("其中，投影参数矩阵为");
      expect(matrixText).toContain("以及");
      const attentionPage = await output.getPage(4);
      const attentionText = (await attentionPage.getTextContent()).items
        .map((item) => ("str" in item ? item.str : ""))
        .join("");
      for (const sentence of [
        "最常用的两种注意力函数",
        "区别仅在于缩放因子",
        "加性注意力使用",
        "高度优化的矩阵乘法代码实现",
      ])
        expect(attentionText).toContain(sentence);
    } finally {
      await loading.destroy();
    }
    expect(result.issues.some((issue) => issue.blockId === "p1-b12")).toBe(
      false,
    );
  });

  it("handles crop boxes and page rotation in displayed coordinates", async () => {
    const f = await fixture();
    f.page.setCropBox(10, 20, 380, 460);
    // An extra page with a rotated coordinate system but horizontal displayed text.
    const rotated = f.doc.addPage([500, 400]);
    rotated.setRotation(degrees(90));
    rotated.drawText("A rotated page", {
      x: 50,
      y: 30,
      size: 10,
      rotate: degrees(90),
    });
    f.blocks = f.blocks.map((b) => ({
      ...b,
      bounds: {
        x: (b.bounds.x * 400 - 10) / 380,
        y: (b.bounds.y * 500 - 20) / 460,
        width: (b.bounds.width * 400) / 380,
        height: (b.bounds.height * 500) / 460,
      },
    }));
    const block: PDFBlock = {
      id: "rotated",
      page: 2,
      label: "text",
      text: "A rotated page",
      bounds: { x: 30 / 400, y: 40 / 500, width: 150 / 400, height: 30 / 500 },
    };
    f.blocks.push(block);
    f.translations.push(translation(block, "旋转后的页面仍然正确排版。"));
    const result = await run(f);
    expect(result.translated).toBe(3);
    const doc = await PDFDocument.load(result.bytes);
    expect(doc.getPage(0).getCropBox()).toEqual({
      x: 10,
      y: 20,
      width: 380,
      height: 460,
    });
    expect(doc.getPage(1).getRotation().angle).toBe(90);
  });

  it("does not produce an empty translation PDF and honours cancellation", async () => {
    const f = await fixture();
    f.translations = [];
    await expect(run(f)).rejects.toMatchObject({
      reason: "no_translated_blocks",
      message: expect.stringContaining("没有能安全排入"),
    });
    const controller = new AbortController();
    controller.abort();
    await expect(run(f, controller.signal)).rejects.toMatchObject({
      name: "AbortError",
    });
  });

  it("renders inline formula paths above the baseline with subscripts below it", async () => {
    const doc = await PDFDocument.create();
    const page = doc.addPage([200, 200]);
    const content = preparePDFText(
      "$h_{t-1}$",
      await embedExportFonts(doc, await fontBytes),
    )!;
    content.draw(page, content.text, 30, 100, 30);
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    const loading = pdfjs.getDocument({
      data: await doc.save(),
      useSystemFonts: true,
    });
    const surface = canvas(200, 200);
    try {
      const pdf = await loading.promise;
      const rendered = await pdf.getPage(1);
      await rendered.render({
        canvas: surface.canvas,
        canvasContext: surface.context,
        viewport: rendered.getViewport({ scale: 1 }),
      }).promise;
      const ink = (x: number, y: number, width: number, height: number) => {
        const pixels = surface.context.getImageData(x, y, width, height).data;
        let count = 0;
        for (let i = 0; i < pixels.length; i += 4) if (pixels[i] < 160) count++;
        return count;
      };
      expect(
        ink(30, 75, 18, 24),
        "missing or inverted base glyph",
      ).toBeGreaterThan(40);
      expect(
        ink(48, 101, 40, 8),
        "missing or misplaced subscript",
      ).toBeGreaterThan(10);
    } finally {
      surface.dispose();
      await loading.destroy();
    }
  });

  it("renders curved parentheses within their glyph bounds without spikes", async () => {
    const doc = await PDFDocument.create();
    const page = doc.addPage([200, 200]);
    const content = preparePDFText(
      "$( $",
      await embedExportFonts(doc, await fontBytes),
    )!;
    content.draw(page, content.text, 30, 100, 60);
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    const loading = pdfjs.getDocument({
      data: await doc.save(),
      useSystemFonts: true,
    });
    const surface = canvas(200, 200);
    try {
      const pdf = await loading.promise;
      const rendered = await pdf.getPage(1);
      await rendered.render({
        canvas: surface.canvas,
        canvasContext: surface.context,
        viewport: rendered.getViewport({ scale: 1 }),
      }).promise;
      const pixels = surface.context.getImageData(0, 0, 200, 200).data;
      let ink = 0,
        escaped = 0;
      for (let y = 0; y < 200; y++)
        for (let x = 0; x < 200; x++) {
          if (pixels[(y * 200 + x) * 4] >= 160) continue;
          ink++;
          if (x < 35 || x > 51 || y < 54 || y > 116) escaped++;
        }
      expect(ink).toBeGreaterThan(150);
      expect(
        escaped,
        "curve control points must not produce spikes beyond the glyph outline",
      ).toBe(0);
    } finally {
      surface.dispose();
      await loading.destroy();
    }
  });

  it("refuses to expand an erasure into neighboring protected content", async () => {
    const f = await fixture();
    // Helvetica's descender extends below this deliberately tight detector box.
    f.blocks[1].bounds.height = 10 / 500;
    f.blocks.push({
      id: "neighbor",
      page: 1,
      label: "image",
      image: "figure.png",
      text: "",
      bounds: { x: 220 / 400, y: 51 / 500, width: 150 / 400, height: 5 / 500 },
    });
    const result = await run(f);
    expect(result.issues).toContainEqual({
      page: 1,
      blockId: "right",
      reason: "overlap",
    });
  });

  it.each(["pdf_decode", "font_embed"] as const)(
    "classifies real %s failures at the engine boundary",
    async (reason) => {
      const f = await fixture();
      const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
      await expect(
        translatedPDF(
          pdfjs.getDocument,
          {
            data:
              reason === "pdf_decode"
                ? new Uint8Array([1, 2, 3])
                : await f.doc.save(),
            useSystemFonts: true,
          },
          f.blocks,
          f.translations,
          reason === "font_embed"
            ? { cjk: new Uint8Array([1]), latin: new Uint8Array([1]) }
            : await fontBytes,
          { createCanvas: canvas },
        ),
      ).rejects.toMatchObject({ reason });
    },
  );
});

describe("pdf-layout", () => {
  const measure = (text: string, size: number) => [...text].length * size;

  it("wraps Chinese punctuation and long Latin words without losing text", () => {
    for (const text of [
      "这是第一段，接着写下一段。",
      "supercalifragilistic",
      "带有（中文）标点的内容。",
      "中文 English 中文",
    ]) {
      const lines = wrapText(text, 60, 10, measure)!;
      expect(lines.join("").replaceAll(" ", "")).toBe(text.replaceAll(" ", ""));
      expect(lines.every((l) => measure(l, 10) <= 60)).toBe(true);
      expect(
        lines.every((l) => !/^[，。）（]/u.test(l) && !/[（]$/u.test(l)),
      ).toBe(true);
    }
  });

  it("shrinks only within the readability floor and refuses overflow", () => {
    const box = { x: 0, y: 0, width: 100, height: 40 };
    const layout = fitText(
      "这是需要排版的一段译文，需要完整显示。",
      box,
      12,
      measure,
    )!;
    expect(layout.size).toBeGreaterThanOrEqual(7.8);
    expect(layout.lines.length * layout.lineHeight).toBeLessThanOrEqual(40);
    expect(fitText("文字".repeat(1000), box, 12, measure)).toBeNull();
    expect(fitText("文字", { ...box, width: NaN }, 12, measure)).toBeNull();
  });

  it("keeps readable Markdown text and refuses math, tables, HTML and images", () => {
    expect(
      exportText("## 标题\n\n**译文**与[链接](https://example.com)\n第二句。"),
    ).toBe("标题\n\n译文与链接 第二句。");
    expect(exportText("- 第一项\n- 第二项")).toBe("• 第一项\n• 第二项");
    for (const source of [
      "公式 $x^2$",
      "![图片](image.png)",
      "<script>bad()</script>",
      "|A|B|\n|-|-|\n|1|2|",
    ])
      expect(exportText(source)).toBeNull();
  });

  it("extracts inline formulas before Markdown parses escapes and keeps literal dollars", () => {
    const formulas: string[] = [];
    const math = (tex: string) => {
      formulas.push(tex);
      return "FORMULA";
    };
    expect(
      exportText(
        String.raw`状态 $h_t$ 与 \(h_{t-1}\)，价格 \$5，代码 ` + "`$x$`。",
        math,
      ),
    ).toBe("状态 FORMULA 与 FORMULA，价格 $5，代码 $x$。");
    expect(formulas).toEqual(["h_t", "h_{t-1}"]);
    expect(exportText("**状态 $x^2$**", math)).toBe("状态 FORMULA");
    expect(exportText("独立 $$x^2$$", math)).toBeNull();
  });
});

describe("pdf-math", () => {
  it("preserves the geometry of subscripts, superscripts, fractions and Greek symbols", () => {
    const h = compileFormula("h");
    const subscript = compileFormula("h_{t-1}");
    const superscript = compileFormula("x^2");
    expect(subscript.width).toBeGreaterThan(h.width);
    expect(subscript.descent).toBeGreaterThan(h.descent);
    expect(subscript.shapes).toHaveLength(4);
    expect(superscript.ascent).toBeGreaterThan(compileFormula("x").ascent);
    const fraction = compileFormula(String.raw`\frac{\alpha}{\beta}`);
    expect(fraction.shapes.some((s) => s.rect)).toBe(true);
    expect(fraction.ascent + fraction.descent).toBeGreaterThan(1);
  });

  it("refuses malformed or external-content math instead of silently losing symbols", () => {
    for (const tex of [
      "x^{",
      String.raw`\unknowncommand{x}`,
      String.raw`\includegraphics{https://example.com/a.png}`,
      "x".repeat(4097),
    ]) {
      expect(() => compileFormula(tex)).toThrow();
    }
  });

  it("recovers a fraction command damaged by JSON form-feed decoding", () => {
    const expected = compileFormula(String.raw`\frac{1}{\sqrt{d_k}}`);
    expect(compileFormula("\u000crac{1}{" + String.raw`\sqrt{d_k}}`)).toEqual(
      expected,
    );
    expect(() => compileFormula("\u000crandom{x}")).toThrow();
  });
});

describe("pdf-svg-path", () => {
  it("preserves quadratic geometry and reflects each successive smooth control point", () => {
    expect(cubicGlyphPath("M0 0 Q3 6 6 0 T12 0 T18 0")).toBe(
      "M0 0 C2 4 4 4 6 0 C8 -4 10 -4 12 0 C14 4 16 4 18 0",
    );
    expect(cubicGlyphPath("m0 0 q3 6 6 0 t6 0 t6 0")).toBe(
      cubicGlyphPath("M0 0 Q3 6 6 0 T12 0 T18 0"),
    );
    expect(cubicGlyphPath("M0 0 Q3 6 6 0 L9 0 T15 0")).toBe(
      "M0 0 C2 4 4 4 6 0 L9 0 C9 0 11 0 15 0",
    );
  });

  it("keeps repeated coordinates and independent subpaths well defined", () => {
    expect(cubicGlyphPath("M0 0 3 3 h2 v-1 z m2 0 l3 1")).toBe(
      "M0 0 L3 3 L5 3 L5 2 Z M2 0 L5 1",
    );
    expect(cubicGlyphPath("M0 0 C1 2 2 2 3 0 S5 -2 6 0")).toBe(
      "M0 0 C1 2 2 2 3 0 C4 -2 5 -2 6 0",
    );
    for (const path of ["M0", "M0 0 Q1 2", "M0 0 A1 1 0 0 1 2 2", "M0 0#"])
      expect(() => cubicGlyphPath(path)).toThrow();
  });

  it("normalizes real parentheses and other mathematical glyphs before PDF writing", () => {
    for (const tex of [
      "(x_1,\\ldots,x_n)",
      "[z]",
      String.raw`\sqrt{x^2}`,
      String.raw`\frac{1}{1+x}`,
    ]) {
      const formula = compileFormula(tex);
      expect(formula.shapes.length).toBeGreaterThan(0);
      for (const shape of formula.shapes)
        if (shape.path) expect(shape.path).not.toMatch(/[QqTtSs]/);
    }
  });
});
