import { describe, expect, it, vi } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { AnnotationQuote, reflowPDFQuote } from "./AnnotationQuote";
import {
  type PDFLocation,
  type Annotation,
  type PDFBlock,
  type TOCItem,
  type Message,
  type DocumentLocation,
} from "@neoread/core";
import {
  activeAnnotation,
  applySavedAnnotation,
  colorLabel,
  highlightPalette,
} from "./annotations";
import {
  buildNavigation,
  emptyNavigation,
  flattenTOC,
  moveNavigation,
} from "./navigation-model";
import { annotationDigest, exportCounts, notesMarkdown } from "./notes-export";
import { paper } from "./papers/fixtures";
import { referenceCards, contextReferences } from "./references";
import { ReferenceNavigation } from "./reference-navigation";
import { pdfQuotePoint } from "./readers/pdf-reference-location";

describe("AnnotationQuote", () => {
  const part = {
    blockId: "p1-b1",
    sourceHash: "h",
    sentenceIndex: 0,
    source: "An original sentence.",
    target: "完整译文句子。",
  };

  it("shows source above translation with the exact selected side preserved after reload", () => {
    const location: PDFLocation = JSON.parse(
      JSON.stringify({
        type: "pdf",
        page: 1,
        sentenceLink: { origin: "translation", parts: [part] },
      }),
    );
    const html = renderToStaticMarkup(
      <AnnotationQuote quote="译文" location={location} />,
    );
    expect(html).toContain("An original sentence.");
    expect(html).toContain("<span>译文</span>");
    expect(html).not.toContain("完整译文句子。");
    expect(html.indexOf("原文")).toBeLessThan(html.indexOf("译文"));
    const original = renderToStaticMarkup(
      <AnnotationQuote
        quote="original"
        location={{
          ...location,
          sentenceLink: { origin: "source", parts: [part] },
        }}
      />,
    );
    expect(original).toContain("完整译文句子。");
    expect(original).not.toContain("An original sentence.");
  });

  it("keeps existing unlinked annotations unchanged", () => {
    expect(
      renderToStaticMarkup(
        <AnnotationQuote quote="旧批注" location={{ type: "pdf", page: 1 }} />,
      ),
    ).toBe("旧批注");
  });

  it.each([
    [
      "通过对 DAMAS 定位算法的研究，对\nDAMAS 算法进行改进。粗定\n位，提高定位的准确\n性。",
      "通过对 DAMAS 定位算法的研究，对DAMAS 算法进行改进。粗定位，提高定位的准确性。",
    ],
    [
      "Acoustic source\nlocalization improves accuracy.",
      "Acoustic source localization improves accuracy.",
    ],
    ["First line.\n\nSecond paragraph.", "First line.\n\nSecond paragraph."],
    ["中文\r\n换行", "中文换行"],
    ["  source  \n  text", "  source text"],
    ["locali\u00adzation", "locali\u00adzation"],
    ["locali\u00ad\nzation", "localization"],
    ["state-\nof-the-art", "state-of-the-art"],
  ])("reflows PDF layout lines: %j", (input, expected) => {
    expect(reflowPDFQuote(input)).toBe(expected);
  });

  it("reflows existing PDF quotes without changing stored anchors or EPUB quotes", () => {
    const quote = "粗定\n位，提高准确\n性。";
    const location: PDFLocation = { type: "pdf", page: 4, quote };
    expect(
      renderToStaticMarkup(
        <AnnotationQuote quote={quote} location={location} />,
      ),
    ).toBe("粗定位，提高准确性。");
    expect(location.quote).toBe(quote);
    expect(
      renderToStaticMarkup(
        <AnnotationQuote
          quote={quote}
          location={{ type: "epub", href: "chapter.xhtml" }}
        />,
      ),
    ).toBe(quote);
  });

  it("reflows a linked PDF source while preserving authored translation paragraphs", () => {
    const quote = "粗定\n位";
    const location: PDFLocation = {
      type: "pdf",
      page: 4,
      sentenceLink: {
        origin: "source",
        parts: [{ ...part, target: "第一段。\n第二段。" }],
      },
    };
    const html = renderToStaticMarkup(
      <AnnotationQuote quote={quote} location={location} />,
    );
    expect(html).toContain("<span>粗定位</span>");
    expect(html).toContain("第一段。\n第二段。");
  });

  it.each([
    ["https://host/\npath", "https://host/path"],
    ["See http://example.com/  \r\n  paper", "See http://example.com/paper"],
    ["https://host/a/\nb/\nc", "https://host/a/b/c"],
    ["www.example.com/\npaper", "www.example.com/paper"],
    ["DOI: 10.1234/\nabcdef", "DOI: 10.1234/abcdef"],
    [
      "(https://doi.org/\n10.1234/\nabcdef)",
      "(https://doi.org/10.1234/abcdef)",
    ],
    ["input /\noutput", "input / output"],
    [
      "See https://host/page and/or\nother text",
      "See https://host/page and/or other text",
    ],
    ["https://host/\n\nNext paragraph", "https://host/\n\nNext paragraph"],
  ])(
    "preserves URL slash wraps without joining ordinary prose: %j",
    (input, expected) => {
      expect(reflowPDFQuote(input)).toBe(expected);
    },
  );
});

describe("annotations", () => {
  const a: Annotation = {
    id: "a",
    documentId: "doc",
    kind: "underline",
    location: { type: "pdf", page: 1 },
    quote: "one two",
    note: "",
    color: "",
    createdAt: "",
  };

  it("replaces a merged underline in place and removes only superseded IDs", () => {
    const note = { ...a, id: "note", kind: "note" as const, note: "Keep this" };
    const result = applySavedAnnotation(
      [a, note, { ...a, id: "b", quote: "two three" }],
      { ...a, quote: "one two three", replacedIds: ["b"] },
    );
    expect(result.map((item) => item.id)).toEqual(["a", "note"]);
    expect(result[0].quote).toBe("one two three");
    expect(result[1]).toBe(note);
    expect(result[0]).not.toHaveProperty("replacedIds");
    expect(
      applySavedAnnotation(result, { ...a, quote: "one two three" }),
    ).toEqual(result);
  });

  it("edits an existing note when several annotation kinds cover a passage", () => {
    const note = { ...a, id: "note", kind: "note" as const, note: "Keep this" };
    expect(activeAnnotation([a, note], [a.id, note.id])).toBe(note);
    expect(activeAnnotation([a, note], [a.id])).toBe(a);
  });
});

describe("highlight-colors", () => {
  it("names colors from the user's palette and keeps retired names", () => {
    const palette = [{ value: "#112233", label: "重点" }];
    expect(colorLabel("#112233", palette)).toBe("重点");
    expect(colorLabel("#a985e0", palette)).toBe("紫色");
    expect(highlightPalette({ highlightColors: [] })).toHaveLength(4);
    expect(highlightPalette({ highlightColors: palette })).toBe(palette);
  });
});

describe("navigation-model", () => {
  const heading = (id: string, page: number, y = 0): TOCItem => ({
    id,
    label: id,
    location: { type: "pdf", page, x: 0.1, y },
    children: [],
  });

  const mark = (page: number, y: number): Annotation => ({
    id: "b",
    documentId: "d",
    kind: "bookmark",
    location: { type: "pdf", page, x: 0.1, y },
    note: "My bookmark",
    quote: "",
    color: "",
    createdAt: "",
  });

  it("uses parsed headings in canonical double-column order instead of page fallback", () => {
    const blocks: PDFBlock[] = [
      {
        id: "left",
        page: 1,
        label: "paragraph_title",
        text: "Left lower",
        bounds: { x: 0.1, y: 0.8, width: 0.35, height: 0.03 },
      },
      {
        id: "right",
        page: 1,
        label: "paragraph_title",
        text: "Right upper",
        bounds: { x: 0.55, y: 0.1, width: 0.35, height: 0.03 },
      },
    ];
    const tree = buildNavigation(
      [{ ...heading("page-1", 1), label: "第 1 页" }],
      blocks,
      [],
      emptyNavigation(),
    );
    expect(tree.map((n) => n.label)).toEqual(["Left lower", "Right upper"]);
    expect(tree[1].location).toMatchObject({ x: 0.55, y: 0.1 });
  });

  it("rejects cycles, backwards parents and crossing subtree intervals", () => {
    const a = heading("a", 1);
    a.children = [heading("b", 2)];
    const tree = buildNavigation(
      [a, heading("c", 3), heading("d", 4)],
      [],
      [],
      emptyNavigation(),
    );
    expect(moveNavigation(tree, "a", "b")).toBeTruthy();
    expect(moveNavigation(tree, "b", "d")).toBeTruthy();
    expect(moveNavigation(tree, "d", "a")).toBeTruthy(); // c would be after d in preorder
    expect(moveNavigation(tree, "c", "a")).toBeNull();
    expect(moveNavigation(tree, "b", null)).toBeNull();
  });

  it("inserts bookmarks beside the preceding section and keeps their kind", () => {
    const a = heading("a", 1);
    a.children = [heading("b", 2, 0.2), heading("c", 2, 0.8)];
    const state = emptyNavigation();
    state.entries["bookmark:b"] = { label: "Renamed", collapsed: true };
    const tree = buildNavigation([a], [], [mark(2, 0.5)], state);
    expect(tree[0].children.map((n) => n.id)).toEqual(["b", "bookmark:b", "c"]);
    expect(tree[0].children[1]).toMatchObject({
      kind: "bookmark",
      label: "Renamed",
      originalLabel: "My bookmark",
    });
  });

  it("keeps a bookmark between a parent title and its first child in source order", () => {
    const a = heading("a", 1, 0.1);
    a.children = [heading("b", 1, 0.8)];
    const tree = buildNavigation([a], [], [mark(1, 0.5)], emptyNavigation());
    expect(flattenTOC(tree).map((n) => n.id)).toEqual(["a", "bookmark:b", "b"]);
    expect(tree[0].children[0].kind).toBe("bookmark");
  });

  it("keeps explicit edits when new headings arrive without overwriting state", () => {
    const state = emptyNavigation();
    state.entries.c = { label: "Custom", parentId: "a", kind: "bookmark" };
    const a = heading("a", 1);
    a.children = [heading("b", 2)];
    const tree = buildNavigation([a, heading("c", 3)], [], [], state);
    expect(tree[0].children.map((n) => n.id)).toEqual(["b", "c"]);
    expect(tree[0].children[1]).toMatchObject({
      label: "Custom",
      kind: "bookmark",
    });
    expect(state.entries.c.parentId).toBe("a");
  });

  it("supports moving a heading under an existing personal bookmark", () => {
    const state = emptyNavigation();
    state.entries.c = { parentId: "bookmark:b" };
    const tree = buildNavigation(
      [heading("a", 1), heading("c", 3)],
      [],
      [mark(2, 0.5)],
      state,
    );
    expect(tree[1].id).toBe("bookmark:b");
    expect(tree[1].children.map((n) => n.id)).toEqual(["c"]);
  });

  it("keeps renamed headings attached when reparsing renumbers paragraph blocks", () => {
    const block: PDFBlock = {
      id: "p1-b2",
      page: 1,
      label: "paragraph_title",
      text: "Methods",
      bounds: { x: 0.1, y: 0.2, width: 0.4, height: 0.03 },
    };
    const first = buildNavigation([], [block], [], emptyNavigation());
    const state = emptyNavigation();
    state.entries[first[0].id] = { label: "我的方法" };
    const next = buildNavigation([], [{ ...block, id: "p1-b5" }], [], state);
    expect(next[0].label).toBe("我的方法");
  });

  it("preserves original order for headings with the exact same destination", () => {
    const a = heading("a", 1, 0.2),
      b = heading("b", 1, 0.2),
      c = heading("c", 1, 0.2);
    const blocks: PDFBlock[] = [
      {
        id: "p1-b1",
        page: 1,
        label: "paragraph_title",
        text: "Title",
        bounds: { x: 0.1, y: 0.2, width: 0.4, height: 0.1 },
      },
    ];
    const tree = buildNavigation([a, b, c], blocks, [], emptyNavigation());
    expect(moveNavigation(tree, "a", "c")).toBeTruthy();
    expect(moveNavigation(tree, "c", "a")).toBeTruthy();
    expect(moveNavigation(tree, "b", "a")).toBeNull();
  });

  it("deletes only selected entries and promotes surviving descendants in source order", () => {
    const a = heading("a", 1),
      b = heading("b", 2),
      c = heading("c", 3);
    a.children = [b];
    b.children = [c];
    const state = { revision: 1, entries: { b: { deleted: true } } };
    const tree = buildNavigation([a, heading("d", 4)], [], [], state);
    expect(flattenTOC(tree).map((n) => n.id)).toEqual(["a", "c", "d"]);
    expect(tree[0].children[0].parentId).toBe("a");
    expect(
      buildNavigation([a], [], [], {
        revision: 2,
        entries: {
          a: { deleted: true },
          b: { deleted: true },
          c: { deleted: true },
        },
      }),
    ).toEqual([]);
    expect(
      buildNavigation([], [], [mark(1, 0.5)], {
        revision: 1,
        entries: { "bookmark:b": { deleted: true } },
      }),
    ).toEqual([]);
  });
});

describe("notes-export", () => {
  const mark = (patch: Partial<Annotation>): Annotation => ({
    id: "x",
    documentId: "p",
    kind: "highlight",
    location: { type: "pdf", page: 1 },
    quote: "",
    note: "",
    color: "#e6b94c",
    createdAt: "2026-10-01",
    ...patch,
  });

  const doc = paper({
    title: "Attention",
    author: "Ada",
    metadata: { date: "2017", venue: "NeurIPS", doi: "10.1000/x" },
  });

  const data = {
    paperNote: "Main idea",
    messages: [
      { id: "m1", role: "user", content: "hi" },
      { id: "m2", role: "assistant", content: "Because **softmax**." },
    ] as Message[],
    annotations: [
      mark({ id: "b", kind: "bookmark", location: { type: "pdf", page: 3 } }),
      mark({
        id: "q",
        kind: "question",
        quote: "line two\nline three",
        note: "Why?",
        answerId: "m2",
        location: { type: "pdf", page: 2 },
      }),
      mark({ id: "h", quote: "key claim", location: { type: "pdf", page: 1 } }),
    ],
  };

  it("exports chosen parts in reading order with answers", () => {
    const md = notesMarkdown(
      doc,
      data,
      ["info", "paperNote", "highlights", "questions", "bookmarks"],
      (a) => `neoread://open?id=p&annotation=${a.id}`,
    );
    expect(md).toBe(
      [
        "# Attention",
        "> Ada · 2017 · NeurIPS\n> DOI: 10.1000/x\n> https://doi.org/10.1000/x",
        "## 论文笔记",
        "Main idea",
        "## 批注",
        "### 第 1 页 · 划线 [↗](neoread://open?id=p&annotation=h)\n\n> key claim",
        "### 第 2 页 · 问题 [↗](neoread://open?id=p&annotation=q)\n\n> line two\n> line three\n\n**问：** Why?\n\n**AI 答：**\n\nBecause **softmax**.",
        "## 书签",
        "- [第 3 页](neoread://open?id=p&annotation=b)",
      ].join("\n\n") + "\n",
    );
  });

  it("leaves out unchosen parts and counts what each part holds", () => {
    const md = notesMarkdown(doc, data, ["chat"]);
    expect(md).toContain("## AI 对话\n\n**我：**\n\nhi\n\n---\n\n**AI：**");
    expect(md).not.toContain("论文笔记");
    expect(md).not.toContain("批注");
    expect(exportCounts(data)).toEqual({
      info: 1,
      paperNote: 1,
      notes: 0,
      highlights: 1,
      questions: 1,
      bookmarks: 1,
      chat: 2,
    });
  });

  it("omits the duplicate document heading when combining notes with translation", () => {
    const md = notesMarkdown(doc, data, ["paperNote"], undefined, {
      includeTitle: false,
    });
    expect(md).toBe("## 论文笔记\n\nMain idea\n");
    expect(
      notesMarkdown(doc, data, [], undefined, { includeTitle: false }).trim(),
    ).toBe("");
  });

  it("quotes annotations into the paper note once, in reading order", () => {
    const link = (a: Annotation) => `neoread://open?id=p&annotation=${a.id}`;
    const marks = [
      mark({
        id: "q",
        kind: "question",
        note: "Why\nscale?",
        quote: "dk",
        location: { type: "pdf", page: 4 },
      }),
      mark({
        id: "h",
        quote: "multi\n head",
        note: "key idea\nsecond line",
        location: { type: "pdf", page: 2 },
      }),
      mark({ id: "b", kind: "bookmark" }),
      mark({ id: "e" }),
    ];
    const date = new Date("2026-10-08T12:00:00");
    const digest = annotationDigest(marks, "", link, date);
    expect(digest).toBe(
      [
        "### 批注摘录 · 2026/10/8",
        "- [第 2 页](neoread://open?id=p&annotation=h)：“multi head”",
        "  key idea",
        "  second line",
        "- [第 4 页](neoread://open?id=p&annotation=q) 问题：Why scale?（关于“dk”）",
      ].join("\n"),
    );
    expect(annotationDigest(marks, digest, link, date)).toBe("");
  });
});

describe("references", () => {
  it("keeps quoted conversation text separate from original-document navigation", () => {
    const cards = referenceCards(
      { references: [{ kind: "chat", text: "quoted conversation" }] },
      [],
    );
    expect(cards).toEqual([
      expect.objectContaining({
        kind: "chat",
        label: "引用对话",
        text: "quoted conversation",
      }),
    ]);
    expect(cards[0].location).toBeUndefined();
    expect(cards[0].sourceId).toBeUndefined();
  });

  it("uses exact saved locations for selected passages and bounds long previews", () => {
    const location: DocumentLocation = {
      type: "epub",
      href: "ch2.xhtml",
      locator: '{"locations":{"domRange":{}}}',
    };
    const cards = referenceCards(
      { references: [{ text: "A long passage ".repeat(100), location }] },
      [{ id: "2", label: "第二章", location, children: [] }],
    );
    expect(cards).toHaveLength(1);
    expect(cards[0].label).toBe("第二章 · 重点句");
    expect(cards[0].location).toEqual(location);
    expect(cards[0].preview.length).toBeLessThanOrEqual(161);
  });

  it("splits legacy context into compact entries without inventing historical locations", () => {
    const cards = referenceCards(
      { context: "First statement. ".repeat(50) + "\n\nA second paragraph." },
      [],
    );
    expect(cards.length).toBeGreaterThan(2);
    expect(
      cards.every((card) => card.preview.length <= 161 && !card.location),
    ).toBe(true);
    expect(cards.at(-1)?.text).toBe("A second paragraph.");
  });

  it("keeps the page or chapter used for automatic context, independent of subsequent reading", () => {
    const position: DocumentLocation = { type: "pdf", page: 6, y: 0.7 };
    const references = contextReferences("Page text", position);
    position.page = 9;
    expect(references[0].location).toEqual({ type: "pdf", page: 6 });
    expect(references[0].kind).toBe("section");
    expect(
      contextReferences("Chapter text", {
        type: "epub",
        href: "ch2.xhtml#end",
        progression: 0.8,
      })[0].location,
    ).toEqual({ type: "epub", href: "ch2.xhtml", progression: 0 });
  });

  it("returns to the original position after visiting multiple citations", async () => {
    let current: DocumentLocation = { type: "pdf", page: 8, x: 0.1, y: 0.65 };
    const reader = {
      getLocation: () => current,
      goTo: vi.fn(async (target: DocumentLocation) => {
        current = target;
      }),
    };
    const navigation = new ReferenceNavigation(reader);
    await navigation.visit({ type: "pdf", page: 1 });
    await navigation.visit({ type: "pdf", page: 3 });
    await navigation.back();
    expect(current).toEqual({ type: "pdf", page: 8, x: 0.1, y: 0.65 });
    expect(navigation.origin).toBeUndefined();
  });

  it("uses block-focused navigation for a source reference and keeps its return point", async () => {
    const original: DocumentLocation = { type: "pdf", page: 8, y: 0.6 };
    const target: DocumentLocation = { type: "pdf", page: 2 };
    const reader = {
      getLocation: () => original,
      goTo: vi.fn(async () => {}),
    };
    const focus = vi.fn(async () => {});
    const navigation = new ReferenceNavigation(reader);
    await navigation.visit(target, focus);
    expect(focus).toHaveBeenCalledWith(target);
    expect(reader.goTo).not.toHaveBeenCalled();
    expect(navigation.origin).toEqual(original);
  });

  it("retains a return point on failed navigation and permits retrying", async () => {
    const reader = {
      getLocation: () => ({
        type: "epub" as const,
        href: "ch2.xhtml",
        locator: "exact-position",
      }),
      goTo: vi.fn().mockResolvedValue(undefined),
    };
    const navigation = new ReferenceNavigation(reader);
    await navigation.visit({ type: "epub", href: "ch1.xhtml" });
    reader.goTo.mockRejectedValueOnce(new Error("navigation failed"));
    await expect(navigation.back()).rejects.toThrow("navigation failed");
    expect(navigation.origin?.type).toBe("epub");
    expect(navigation.busy).toBe(false);
    await navigation.back();
    expect(reader.goTo).toHaveBeenLastCalledWith({
      type: "epub",
      href: "ch2.xhtml",
      locator: "exact-position",
    });
  });

  it("finds a PDF quote's text run instead of always jumping to the page top", () => {
    const point = pdfQuotePoint(
      [
        { str: "Heading", transform: [1, 0, 0, 1, 50, 750], height: 12 },
        {
          str: "The cited sentence.",
          transform: [1, 0, 0, 1, 80, 300],
          height: 10,
        },
      ],
      "cited sentence",
      {
        width: 600,
        height: 800,
        convertToViewportPoint: (x, y) => [x, 800 - y],
      },
    );
    expect(point).toEqual({ x: 80 / 600, y: 490 / 800 });
  });
});
