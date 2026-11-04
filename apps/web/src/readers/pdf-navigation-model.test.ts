import { describe, expect, it, vi } from "vitest";
import {
  type PDFBlock,
  type DocumentLocation,
  type Annotation,
  type TOCItem,
} from "@neoread/core";
import { figureReferences, figureTarget } from "./figure-references";
import { type PDFDocumentProxy } from "pdfjs-dist";
import { numberedReference, resolvePDFCitation } from "./pdf-citations";
import { destinationPoint, parseDestinationHash } from "./pdf-link-preview";
import { ReferenceNavigation } from "../reference-navigation";
import { readFile } from "node:fs/promises";
import { PDFDocument, PDFName, PDFHexString } from "pdf-lib";
import { getDocument } from "pdfjs-dist/legacy/build/pdf.mjs";
import { readPDFOutline } from "./pdf-outline";
import { writePDFOutline } from "./pdf-outline-export";
import { annotatedPDF } from "./pdf-export";

describe("figure-references", () => {
  const block = (
    id: string,
    label: string,
    text: string,
    x = 0.1,
    y = 0.1,
  ): PDFBlock => ({
    id,
    label,
    text,
    page: 1,
    bounds: { x, y, width: 0.3, height: 0.2 },
  });

  it("recognizes English, bracketed, Chinese, supplementary and subfigure references", () => {
    expect(
      figureReferences(
        "如图三，Figure 3, figure[12], Fig. 2(a), 图十二、Figure S1、Figure 2.1。",
      ).map((r) => r.label),
    ).toEqual(["3", "3", "12", "2", "12", "s1", "2.1"]);
    expect(figureReferences("[3] configuration 4 Figure 12word")).toEqual([]);
  });

  it("resolves only caption identities and never selects a matching body paragraph", () => {
    const image = {
      ...block("image", "image", ""),
      caption: "Figure 3. Results",
    };
    const body = block("body", "text", "Figure 3 illustrates the result.");
    expect(figureTarget([body, image], "3")).toBe(image);
    expect(figureTarget([body, image], "1")).toBeUndefined();
    expect(
      figureTarget([body, image, { ...image, id: "duplicate" }], "3"),
    ).toBeUndefined();
  });

  it("associates a separate caption with the adjacent image in the same column", () => {
    const image = block("image", "chart", "");
    const other = block("other", "image", "", 0.6);
    const caption = block(
      "caption",
      "figure_title",
      "图三 实验结果",
      0.1,
      0.31,
    );
    expect(figureTarget([other, caption, image], "3")).toBe(image);
    expect(figureTarget([other, caption], "3")).toBeUndefined();
    expect(figureTarget([{ ...image, page: 2 }, caption], "3")).toBeUndefined();
  });

  it.each([
    "Supplementary Figure S1. Results",
    "supplementary Fig. S1. Results",
    "Supplemental Figure S1. Results",
    "  Supplementary\nFigure S1. Results  ",
  ])("resolves supplementary captions: %s", (caption) => {
    const image = { ...block("image", "image", ""), caption };
    expect(figureTarget([image], "s1")).toBe(image);
    expect(figureTarget([image], "1")).toBeUndefined();
    const separate = block("caption", "figure_title", caption, 0.1, 0.31);
    const uncaptioned = block("image", "image", "");
    expect(figureTarget([uncaptioned, separate], "s1")).toBe(uncaptioned);
  });

  it("rejects incidental supplementary mentions and ambiguous supplementary targets", () => {
    const image = {
      ...block("image", "image", ""),
      caption: "Results shown in Supplementary Figure S1.",
    };
    expect(figureTarget([image], "s1")).toBeUndefined();
    const target = { ...image, caption: "Supplementary Figure S1. Results" };
    expect(
      figureTarget([target, { ...target, id: "duplicate" }], "s1"),
    ).toBeUndefined();
  });
});

describe("pdf-citations", () => {
  const block: PDFBlock = {
    id: "p1-b1",
    page: 1,
    label: "text",
    text: "See [6].",
    bounds: { x: 0, y: 0.3, width: 1, height: 0.3 },
  };

  it("resolves the original citation annotation to its exact PDF page and position", async () => {
    const view = {
      width: 600,
      height: 800,
      viewBox: [0, 0, 600, 800],
      convertToViewportPoint: (x: number, y: number) => [x, 800 - y],
    };
    const page = {
      getViewport: () => view,
      getAnnotations: async () => [
        { subtype: "Link", dest: "cite.six", rect: [100, 500, 120, 512] },
      ],
      getTextContent: async () => ({
        items: [
          {
            str: "[6]",
            width: 20,
            height: 12,
            transform: [12, 0, 0, 12, 100, 500],
          },
        ],
      }),
    };
    const pdf = {
      getPage: vi.fn(async () => page),
      getDestination: vi.fn(async () => [
        { num: 12, gen: 0 },
        { name: "XYZ" },
        72,
        700,
      ]),
      getPageIndex: vi.fn(async () => 8),
    };
    expect(
      await resolvePDFCitation(pdf as unknown as PDFDocumentProxy, block, "6"),
    ).toEqual({ type: "pdf", page: 9, x: 0.12, y: 0.125 });
    expect(
      await resolvePDFCitation(pdf as unknown as PDFDocumentProxy, block, "7"),
    ).toBeUndefined();
  });

  it("only falls back to numbered bibliography blocks, preserving references within a multi-entry block", () => {
    const refs = {
      ...block,
      id: "p9-b1",
      page: 9,
      label: "reference_content",
      text: "[5] Five\n[6] Six",
    };
    expect(numberedReference([block, refs], "6")).toEqual({
      type: "pdf",
      page: 9,
      x: 0,
      y: 0.44999999999999996,
    });
    expect(numberedReference([block, refs], "8")).toBeUndefined();
  });

  it("uses glyph widths for proportional-font lines and excludes the next line's baseline", async () => {
    const glyphs = [...("WWWW[6]" + "i".repeat(30))].map((unicode) => ({
      unicode,
      width:
        unicode === "W"
          ? 1000
          : unicode === "i"
            ? 100
            : unicode === "6"
              ? 500
              : 300,
    }));
    const page = {
      getViewport: () => ({
        width: 600,
        height: 800,
        viewBox: [0, 0, 600, 800],
        convertToViewportPoint: (x: number, y: number) => [x, 800 - y],
      }),
      getAnnotations: async () => [
        { subtype: "Link", dest: "six", rect: [56, 500, 61, 510] },
      ],
      getTextContent: async () => ({
        items: [
          {
            str: `WWWW [6] ${"i".repeat(30)}`,
            fontName: "F1",
            width: 87,
            height: 12,
            transform: [10, 0, 0, 10, 10, 500],
          },
          {
            str: "6",
            fontName: "F1",
            width: 5,
            height: 12,
            transform: [10, 0, 0, 10, 56, 489],
          },
        ],
      }),
      getOperatorList: async () => ({
        fnArray: [1, 2],
        argsArray: [["F1", 10], [glyphs]],
      }),
    };
    const pdf = {
      getPage: async () => page,
      getDestination: async () => [8, { name: "XYZ" }, 72, 700],
    };
    const target = await resolvePDFCitation(
      pdf as unknown as PDFDocumentProxy,
      block,
      "6",
      { setFont: 1, showText: 2, save: 3, restore: 4 },
    );
    expect(target).toEqual({ type: "pdf", page: 9, x: 0.12, y: 0.125 });
  });
});

describe("pdf-link-preview", () => {
  it("parses PDF.js internal link hashes", () => {
    const explicit = [{ num: 12, gen: 0 }, { name: "XYZ" }, 72, 700, null];
    expect(
      parseDestinationHash("#" + escape(JSON.stringify(explicit))),
    ).toEqual({
      dest: explicit,
    });
    expect(parseDestinationHash("#cite.vaswani2017")).toEqual({
      dest: "cite.vaswani2017",
    });
    expect(parseDestinationHash("#nameddest=fig%3A3")).toEqual({
      dest: "fig:3",
    });
    expect(parseDestinationHash("#page=4&zoom=auto")).toEqual({ page: 4 });
    expect(parseDestinationHash("#")).toEqual({});
  });

  it("reads the target point of each destination mode", () => {
    const ref = { num: 1, gen: 0 };
    expect(destinationPoint([ref, { name: "XYZ" }, 300, 500, 0])).toEqual({
      left: 300,
      top: 500,
    });
    expect(destinationPoint([ref, { name: "FitH" }, 640])).toEqual({
      left: null,
      top: 640,
    });
    expect(destinationPoint([ref, { name: "FitR" }, 10, 20, 30, 40])).toEqual({
      left: 10,
      top: 40,
    });
    expect(destinationPoint([ref, { name: "Fit" }])).toEqual({
      left: null,
      top: null,
    });
  });

  it("forgets return points that are back on screen", async () => {
    let near = false;
    const origin: DocumentLocation = { type: "pdf", page: 2, y: 0.3 };
    const reader = {
      getLocation: () => origin,
      goTo: vi.fn().mockResolvedValue(undefined),
      isNear: () => near,
    };
    const navigation = new ReferenceNavigation(reader);
    // A link click records the start; a far target keeps it.
    navigation.remember(origin);
    expect(navigation.settle()).toBe(false);
    expect(navigation.origin).toEqual(origin);
    // A second jump keeps the first start.
    navigation.remember({ type: "pdf", page: 9 });
    expect(navigation.origin).toEqual(origin);
    // Scrolling back near the start drops the return button.
    near = true;
    expect(navigation.settle()).toBe(true);
    expect(navigation.origin).toBeUndefined();
    // A visit to something already on screen leaves no return point.
    await navigation.visit({ type: "pdf", page: 2, y: 0.4 });
    expect(navigation.origin).toBeUndefined();
  });

  it("keeps the return point during location events before the link has moved the viewport", () => {
    let near = true;
    const origin: DocumentLocation = { type: "pdf", page: 2, y: 0.3 };
    const navigation = new ReferenceNavigation({
      getLocation: () => origin,
      goTo: vi.fn(),
      isNear: () => near,
    });
    navigation.remember(origin);
    expect(navigation.settle()).toBe(false);
    expect(navigation.origin).toEqual(origin);
    near = false;
    expect(navigation.settle()).toBe(false);
    near = true;
    expect(navigation.settle()).toBe(true);
  });
});

describe("pdf-outline", () => {
  const fixture = () =>
    readFile(
      new URL("../../public/samples/reading-notes.pdf", import.meta.url),
    );

  it("round trips Unicode labels, kinds, hierarchy, same-page positions and standard annotations", async () => {
    const tree: TOCItem[] = [
      {
        id: "original-1",
        kind: "heading",
        label: "修改后的章节",
        originalLabel: "Original chapter",
        location: { type: "pdf", page: 1, x: 0.1, y: 0.2 },
        children: [
          {
            id: "bookmark:1",
            kind: "bookmark",
            label: "我的书签",
            originalLabel: "Bookmark",
            location: { type: "pdf", page: 1, x: 0.1, y: 0.5 },
            children: [],
          },
        ],
      },
    ];
    const annotation: Annotation = {
      id: "highlight",
      documentId: "d",
      kind: "highlight",
      location: {
        type: "pdf",
        page: 1,
        rects: [{ x: 0.1, y: 0.2, width: 0.2, height: 0.04 }],
      },
      quote: "test",
      note: "Kept comment",
      color: "#facc15",
      createdAt: "2026-01-01T00:00:00Z",
    };
    const saved = await annotatedPDF(
      getDocument,
      { data: new Uint8Array(await fixture()) },
      [annotation],
      { navigation: async () => tree },
    );
    expect(saved).toMatchObject({ exported: 1, navigationExported: 2 });
    const task = getDocument({ data: saved.bytes });
    try {
      const pdf = await task.promise;
      expect((await pdf.getOutline())?.[0].title).toBe("修改后的章节");
      const imported = await readPDFOutline(pdf);
      expect(imported[0]).toMatchObject({
        id: "original-1",
        kind: "heading",
        originalLabel: "Original chapter",
      });
      expect(imported[0].children[0]).toMatchObject({
        id: "bookmark:1",
        kind: "bookmark",
        label: "我的书签",
      });
      if (imported[0].children[0].location.type !== "pdf") throw Error();
      expect(imported[0].children[0].location.y).toBeCloseTo(0.5);
      expect(
        (await (await pdf.getPage(1)).getAnnotations()).some(
          (a) => a.subtype === "Highlight",
        ),
      ).toBe(true);
    } finally {
      await task.destroy();
    }
  });

  it("ignores forged/stale Reader markers instead of trusting their type or destination", async () => {
    const original = getDocument({ data: new Uint8Array(await fixture()) });
    let saved: Uint8Array;
    try {
      const pdf = await original.promise;
      saved = await writePDFOutline(
        await pdf.getData(),
        await readPDFOutline(pdf),
        pdf,
      );
    } finally {
      await original.destroy();
    }
    const doc = await PDFDocument.load(saved!);
    const info = doc.context.lookup(
      doc.context.trailerInfo.Info,
    ) as import("pdf-lib").PDFDict;
    info.set(
      PDFName.of("ReaderNavigation"),
      PDFHexString.fromText(
        JSON.stringify({
          version: 1,
          entries: [
            { id: "__proto__", kind: "bookmark", label: "fake", page: 999 },
          ],
        }),
      ),
    );
    const task = getDocument({ data: await doc.save() });
    try {
      const tree = await readPDFOutline(await task.promise);
      expect(tree[0].id).toBe("outline:0");
      expect(tree[0].kind).toBeUndefined();
      expect(tree[0].location).toMatchObject({ page: 1 });
    } finally {
      await task.destroy();
    }
  });

  it("preserves rotated/cropped page coordinates through export", async () => {
    const doc = await PDFDocument.create();
    const p = doc.addPage([600, 800]);
    p.setCropBox(50, 100, 400, 600);
    const { degrees } = await import("pdf-lib");
    p.setRotation(degrees(90));
    const task = getDocument({ data: await doc.save() });
    let saved: Uint8Array;
    try {
      const pdf = await task.promise;
      saved = await writePDFOutline(
        await pdf.getData(),
        [
          {
            id: "r",
            label: "Rotated",
            kind: "bookmark",
            location: { type: "pdf", page: 1, x: 0.2, y: 0.6 },
            children: [],
          },
        ],
        pdf,
      );
    } finally {
      await task.destroy();
    }
    const reopened = getDocument({ data: saved! });
    try {
      const tree = await readPDFOutline(await reopened.promise);
      expect(tree[0].kind).toBe("bookmark");
      const l = tree[0].location;
      if (l.type !== "pdf") throw Error();
      expect(l.x).toBeCloseTo(0.2);
      expect(l.y).toBeCloseTo(0.6);
    } finally {
      await reopened.destroy();
    }
  });
});
