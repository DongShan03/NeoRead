// @vitest-environment jsdom
import { act, useRef } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type {
  Annotation,
  PDFBlock,
  EPUBReadingBlock,
  ReaderAdapter,
  SearchResult,
  TranslationBlock,
} from "@neoread/core";
// Geometry/lifecycle assertions are independent of the composited hover renderer.
vi.mock("./sentence-hover", () => ({
  SentenceHover: class {
    invalidate() {}
    show(_key: string, ranges: Range[]) {
      CSS.highlights.set("test-hover", new Highlight(...ranges));
    }
    clear() {
      CSS.highlights.delete("test-hover");
    }
    destroy() {
      this.clear();
    }
  },
}));
import { useSentenceMarks } from "./useSentenceMarks";
import { sentenceLink } from "./sentence-links";
const t: TranslationBlock = {
  blockId: "p1-b1",
  sourceHash: "hash",
  status: "complete",
  sentences: [{ source: "Original sentence.", target: "翻译句子。" }],
};
const blocks = [
  {
    id: t.blockId,
    text: "Original sentence.",
    page: 1,
    bounds: { x: 0, y: 0, width: 1, height: 1 },
  },
] as PDFBlock[];
const epubBlocks: EPUBReadingBlock[] = [
  {
    id: t.blockId,
    text: "Original sentence.",
    label: "text",
    location: { type: "epub", href: "chapter.xhtml", locator: "{}" },
  },
];
const events = {
  location: vi.fn(),
  selection: vi.fn(),
  annotation: vi.fn(),
  annotationHover: vi.fn(),
  sentenceHover: vi.fn(),
};
let host: HTMLDivElement, root: Root;
const registry = new Map<string, Set<Range>>();
const resizes = new Map<Element, () => void>();
const box = (x: number) => new DOMRect(x, 0, 100, 20);
const engine = {
  sentenceRanges: () => {
    const range = document.createRange();
    range.selectNodeContents(host.querySelector("[data-source]")!.firstChild!);
    return [range];
  },
} as unknown as ReaderAdapter;
function App({
  linked = true,
  epub = false,
  annotations = [],
  translations = [t],
  searchQuery = "",
  searchResults,
  sourceBlocks = blocks,
}: {
  linked?: boolean;
  epub?: boolean;
  annotations?: Annotation[];
  translations?: TranslationBlock[];
  searchQuery?: string;
  searchResults?: SearchResult[];
  sourceBlocks?: PDFBlock[];
}) {
  const ref = useRef<HTMLDivElement>(null);
  useSentenceMarks(
    ref,
    engine,
    epub ? epubBlocks : sourceBlocks,
    translations,
    annotations,
    linked,
    "parallel",
    events,
    searchQuery,
    searchResults,
  );
  return (
    <div ref={ref}>
      {epub ? (
        <div data-epub-block="p1-b1">
          <span data-epub-run="p1-b1">Original </span>
          <em>
            <span data-epub-run="p1-b1">sentence.</span>
          </em>
        </div>
      ) : (
        <div className="page" data-page-number="1">
          <div className="textLayer">
            <span data-source>Original sentence.</span>
          </div>
        </div>
      )}
      <section data-translation-block="p1-b1">
        {(!epub || translations.length > 0) && (
          <div data-sentence="0">翻译句子。</div>
        )}
      </section>
    </div>
  );
}
beforeEach(() => {
  resizes.clear();
  vi.useFakeTimers();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal(
    "Highlight",
    class extends Set<Range> {
      priority = 0;
      constructor(...ranges: Range[]) {
        super(ranges);
      }
    },
  );
  vi.stubGlobal("CSS", { highlights: registry });
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(private callback: () => void) {}
      observe(node: Element) {
        resizes.set(node, this.callback);
      }
      disconnect() {}
    },
  );
  vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(
    function (this: Element) {
      return box(this.closest("[data-translation-block]") ? 200 : 0);
    },
  );
  Object.defineProperty(Range.prototype, "getClientRects", {
    configurable: true,
    value: function (this: Range) {
      const el =
        this.startContainer.nodeType === 3
          ? this.startContainer.parentElement
          : (this.startContainer as Element);
      return [box(el?.closest("[data-translation-block]") ? 200 : 0)];
    },
  });
  Object.defineProperty(Range.prototype, "getBoundingClientRect", {
    configurable: true,
    value: function (this: Range) {
      return this.getClientRects()[0];
    },
  });
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  registry.clear();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  events.annotation.mockClear();
  events.annotationHover.mockClear();
  events.sentenceHover.mockClear();
  window.getSelection()?.removeAllRanges();
});
const hover = () =>
  [...registry.entries()].find(([name]) => name.endsWith("-hover"))?.[1];
const texts = (ranges: Set<Range> | undefined) =>
  [...(ranges ?? [])].map((r) => r.toString());
const search = () =>
  [...registry.entries()].find(([name]) => name.endsWith("-search"))?.[1];
it("rebuilds sentence marks once after a burst of window-resize notifications", async () => {
  const results: SearchResult[] = [
    {
      id: "p1-b1:translation:0",
      blockId: "p1-b1",
      side: "translation",
      excerpt: "翻译句子。",
      location: { type: "pdf", page: 1 },
    },
  ];
  await act(async () =>
    root.render(<App searchQuery="翻译" searchResults={results} />),
  );
  const paint = vi.spyOn(registry, "set");
  const notify = resizes.get(host.firstElementChild!)!;
  for (let i = 0; i < 5; i++) {
    await act(async () => {
      notify();
      await vi.advanceTimersByTimeAsync(32);
    });
    expect(paint).not.toHaveBeenCalled();
  }
  await act(async () => {
    await vi.advanceTimersByTimeAsync(160);
  });
  expect(paint).toHaveBeenCalledTimes(1);
  expect(texts(search())).toEqual(["翻译"]);
});
it("does not rebuild sentence highlights on unrelated renders without search results", async () => {
  const translations = [t];
  const annotations: Annotation[] = [];
  await act(async () =>
    root.render(<App translations={translations} annotations={annotations} />),
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(32);
  });
  const remove = vi.spyOn(registry, "delete");
  await act(async () =>
    root.render(<App translations={translations} annotations={annotations} />),
  );
  expect(remove).not.toHaveBeenCalled();
});
it("highlights only matching words in translation and clears when search changes", async () => {
  const result: SearchResult = {
    id: "p1-b1:translation:0",
    blockId: "p1-b1",
    side: "translation",
    excerpt: "翻译句子。",
    location: { type: "pdf", page: 1 },
  };
  await act(async () =>
    root.render(<App searchQuery="翻译" searchResults={[result]} />),
  );
  expect(texts(search())).toEqual(["翻译"]);
  await act(async () => root.render(<App />));
  expect(search()).toBeUndefined();
});
it("maps original search words through PDF sentence ranges", async () => {
  const result: SearchResult = {
    id: "p1-b1:source:0",
    blockId: "p1-b1",
    side: "source",
    excerpt: "Original sentence.",
    location: { type: "pdf", page: 1 },
  };
  await act(async () =>
    root.render(<App searchQuery="Original" searchResults={[result]} />),
  );
  expect(texts(search())).toEqual(["Original"]);
});
async function pointer(selector: string, type: string, x: number, buttons = 0) {
  await act(async () => {
    host.querySelector(selector)!.dispatchEvent(
      new MouseEvent(type, {
        bubbles: true,
        clientX: x,
        clientY: 5,
        buttons,
        button: 0,
      }),
    );
    await vi.advanceTimersByTimeAsync(425);
  });
}
it("highlights both sentence texts in either direction, clears on drag, and keeps native selection untouched", async () => {
  await act(async () => root.render(<App />));
  await pointer("[data-source]", "pointermove", 10);
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
  await pointer("[data-sentence]", "pointermove", 210);
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
  expect(window.getSelection()?.toString()).toBe("");
  await pointer("[data-sentence]", "pointerdown", 210, 1);
  expect(hover()).toBeUndefined();
});
it("keeps legacy translated marks single-sided regardless of hover association", async () => {
  const annotation: Annotation = {
    id: "a",
    documentId: "doc",
    kind: "underline",
    quote: "翻译",
    note: "note",
    color: "#5b9fe8",
    createdAt: "",
    location: {
      type: "pdf",
      page: 1,
      translation: {
        blockId: t.blockId,
        sourceHash: t.sourceHash,
        sentenceIndexes: [0],
        start: 0,
        end: 2,
      },
      sentenceLink: sentenceLink(
        [{ blockId: t.blockId, sentenceIndexes: [0] }],
        [t],
        "translation",
      ),
    },
  };
  await act(async () =>
    root.render(<App linked={false} annotations={[annotation]} />),
  );
  await pointer("[data-sentence]", "pointermove", 210);
  expect(hover()).toBeUndefined();
  expect(events.annotationHover).toHaveBeenLastCalledWith(
    expect.objectContaining({ ids: ["a"] }),
  );
  let mark = [...registry.entries()].find(([name]) =>
    name.includes("-underline-"),
  )!;
  expect(texts(mark[1])).toEqual(["翻译"]);
  await act(async () => root.render(<App annotations={[annotation]} />));
  mark = [...registry.entries()].find(([name]) =>
    name.includes("-underline-"),
  )!;
  expect(texts(mark[1])).toEqual(["翻译"]);
  expect(document.head.textContent).toContain("underline 2px #5b9fe8");
  await pointer("[data-source]", "pointermove", 10);
  expect(events.annotationHover).toHaveBeenLastCalledWith(null);
  await pointer("[data-source]", "pointerdown", 10, 1);
  await pointer("[data-source]", "click", 10);
  expect(events.annotation).not.toHaveBeenCalled();
  await pointer("[data-sentence]", "pointerdown", 210, 1);
  await pointer("[data-sentence]", "click", 210);
  expect(events.annotation).toHaveBeenCalledWith(
    expect.objectContaining({ ids: ["a"] }),
  );
  await act(async () => root.render(<App annotations={[]} />));
  expect(
    [...registry.keys()].some((name) => name.includes("-underline-")),
  ).toBe(false);
});
it("does not project legacy source notes onto translated sentences", async () => {
  const a: Annotation = {
    id: "a",
    documentId: "doc",
    kind: "note",
    quote: "Original",
    note: "note",
    color: "#6cc58c",
    createdAt: "",
    location: {
      type: "pdf",
      page: 1,
      sentenceLink: sentenceLink(
        [{ blockId: t.blockId, sentenceIndexes: [0] }],
        [t],
        "source",
      ),
    },
  };
  await act(async () => root.render(<App annotations={[a]} />));
  expect(registry.size).toBe(0);
  await act(async () =>
    root.render(
      <App
        annotations={[a]}
        translations={[{ ...t, sourceHash: "changed" }]}
      />,
    ),
  );
  expect(registry.size).toBe(0);
});

it("keeps a stationary hover after a child mutation and a parent render", async () => {
  await act(async () => root.render(<App />));
  await pointer("[data-sentence]", "pointermove", 210);
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
  await act(async () => {
    host.querySelector(".page")!.append(document.createElement("i"));
    await vi.advanceTimersByTimeAsync(50);
  });
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
  await act(async () => root.render(<App translations={[{ ...t }]} />));
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
});

it("coalesces pointer bursts and ignores block-overlay mutations without rematching PDF text", async () => {
  const matching = vi.spyOn(engine, "sentenceRanges");
  const reads = vi.spyOn(Range.prototype, "getClientRects");
  await act(async () => root.render(<App />));
  await pointer("[data-sentence]", "pointermove", 210);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(40);
  });
  const before = matching.mock.calls.length;
  reads.mockClear();
  await act(async () => {
    for (let i = 0; i < 30; i++)
      host.querySelector("[data-sentence]")!.dispatchEvent(
        new MouseEvent("pointermove", {
          bubbles: true,
          clientX: 210 + i / 100,
          clientY: 5,
        }),
      );
    await vi.advanceTimersByTimeAsync(40);
  });
  expect(reads).not.toHaveBeenCalled();
  await act(async () => {
    const overlay = document.createElement("div");
    overlay.className = "neoread-block-hover";
    host.querySelector(".page")!.append(overlay);
    await vi.advanceTimersByTimeAsync(40);
  });
  expect(matching.mock.calls.length).toBe(before);
  await act(async () => {
    host.querySelector("[data-source]")!.textContent = "Original sentence.";
    await vi.advanceTimersByTimeAsync(40);
  });
  expect(matching.mock.calls.length).toBe(before + 1);
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
});

it("waits for dwell, tolerates line gaps, and cancels pending work on interruption", async () => {
  await act(async () => root.render(<App />));
  const matching = vi.spyOn(engine, "sentenceRanges");
  const send = (x: number, type = "pointermove") =>
    host
      .querySelector("[data-source]")!
      .dispatchEvent(
        new MouseEvent(type, { bubbles: true, clientX: x, clientY: 5 }),
      );
  const advance = async (ms: number) => {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });
  };
  send(10);
  await advance(399);
  send(10, "pointerleave");
  await advance(600);
  expect(matching).not.toHaveBeenCalled();
  expect(hover()).toBeUndefined();
  send(10);
  await advance(200);
  send(12);
  await act(async () => root.render(<App translations={[{ ...t }]} />));
  await advance(199);
  expect(matching).not.toHaveBeenCalled();
  expect(hover()).toBeUndefined();
  await advance(1);
  expect(hover()).toBeDefined();
  expect(matching).toHaveBeenCalledTimes(1);
  // Whitespace starts a single leave deadline; repeated moves do not extend it.
  send(150);
  await advance(300);
  send(160);
  await advance(199);
  expect(hover()).toBeDefined();
  send(12);
  await advance(600);
  expect(hover()).toBeDefined();
  send(150);
  await advance(499);
  expect(hover()).toBeDefined();
  await advance(1);
  expect(hover()).toBeUndefined();
  send(10);
  await advance(399);
  send(10, "pointerdown");
  await advance(600);
  expect(hover()).toBeUndefined();
  send(10);
  await advance(400);
  expect(hover()).toBeDefined();
  send(10, "scroll");
  expect(hover()).toBeUndefined();
  send(10);
  send(10, "pointerleave");
  await advance(600);
  expect(hover()).toBeUndefined();
});

it.each(["highlight", "underline", "note"] as const)(
  "suppresses the whole marked source sentence for %s, previews without selecting, and restores after deletion",
  async (kind) => {
    const annotation: Annotation = {
      id: "source-mark",
      documentId: "doc",
      kind,
      quote: "Original",
      note: "具体批注",
      color: "#e6b94c",
      createdAt: "",
      location: {
        type: "pdf",
        page: 1,
        rects: [{ x: 0, y: 0, width: 0.2, height: 1 }],
      },
    };
    await act(async () => root.render(<App annotations={[annotation]} />));
    // Hover the unmarked part of the same sentence.
    await pointer("[data-source]", "pointermove", 70);
    expect(hover()).toBeUndefined();
    expect(events.annotationHover).toHaveBeenLastCalledWith(
      expect.objectContaining({ ids: [annotation.id] }),
    );
    expect(events.annotation).not.toHaveBeenCalled();
    await pointer("[data-source]", "pointerleave", 70);
    expect(events.annotationHover).toHaveBeenLastCalledWith(null);
    await pointer("[data-source]", "pointermove", 70);
    expect(events.annotationHover).toHaveBeenLastCalledWith(
      expect.objectContaining({ ids: [annotation.id] }),
    );
    await act(async () => root.render(<App annotations={[]} />));
    expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
    expect(events.annotationHover).toHaveBeenLastCalledWith(null);
  },
);

it("draws split source anchors independently of translation and linking settings", async () => {
  const a: Annotation = {
    id: "split-source",
    documentId: "doc",
    kind: "highlight",
    quote: "Original sentence.",
    note: "",
    color: "#6cc58c",
    createdAt: "",
    location: {
      type: "pdf",
      page: 1,
      sourceAnchors: [
        {
          blockId: "old-layout-id",
          page: 1,
          source: "Original sentence.",
          bounds: { x: 0, y: 0, width: 1, height: 1 },
        },
      ],
    },
  };
  await act(async () =>
    root.render(<App linked={false} translations={[]} annotations={[a]} />),
  );
  const mark = [...registry.entries()].find(([name]) =>
    name.includes("-highlight-"),
  );
  expect(mark).toBeDefined();
  expect(texts(mark![1])).toEqual(["Original sentence."]);
  await pointer("[data-source]", "pointermove", 10);
  expect(events.annotationHover).toHaveBeenLastCalledWith(
    expect.objectContaining({ ids: ["split-source"] }),
  );
  await pointer("[data-sentence]", "pointermove", 210);
  expect(events.annotationHover).toHaveBeenLastCalledWith(null);
});

it("links EPUB sentences across inline runs in both directions without changing selection", async () => {
  await act(async () => root.render(<App epub />));
  await pointer("[data-epub-run]", "pointermove", 5);
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
  await pointer("[data-sentence]", "pointermove", 205);
  expect(texts(hover())).toEqual(["Original sentence.", "翻译句子。"]);
  expect(window.getSelection()?.toString()).toBe("");
  await pointer("[data-epub-block]", "scroll", 5);
  expect(hover()).toBeUndefined();
});
it("keeps EPUB hover on the current side when linking is disabled", async () => {
  await act(async () => root.render(<App epub linked={false} />));
  await pointer("[data-epub-run]", "pointermove", 5);
  expect(texts(hover())).toEqual(["Original sentence."]);
  await pointer("[data-sentence]", "pointermove", 205);
  expect(texts(hover())).toEqual(["翻译句子。"]);
});
it("segments untranslated EPUB source and rejects mismatched translation anchors", async () => {
  await act(async () => root.render(<App epub translations={[]} />));
  await pointer("[data-epub-run]", "pointermove", 5);
  expect(texts(hover())).toEqual(["Original sentence."]);
  await act(async () =>
    root.render(
      <App
        epub
        translations={[
          {
            ...t,
            sentences: [{ source: "Changed source.", target: "翻译句子。" }],
          },
        ]}
      />,
    ),
  );
  await pointer("[data-sentence]", "pointermove", 205);
  expect(texts(hover())).toEqual(["翻译句子。"]);
});

it.each([
  [false, "source"],
  [false, "translation"],
  [true, "source"],
  [true, "translation"],
] as const)(
  "offers a whole-sentence action and shares one saved note on both sides (EPUB: %s, origin: %s)",
  async (epub, origin) => {
    await act(async () => root.render(<App epub={epub} />));
    const source = epub ? "[data-epub-run]" : "[data-source]";
    await pointer(
      origin === "source" ? source : "[data-sentence]",
      "pointermove",
      origin === "source" ? 5 : 205,
    );
    const selected = events.sentenceHover.mock
      .lastCall?.[0] as import("@neoread/core").ReaderSelection;
    expect(selected?.text).toBe(
      origin === "source" ? "Original sentence." : "翻译句子。",
    );
    expect(selected.location.translation).toMatchObject({
      wholeSentence: true,
      bilingual: true,
    });
    expect(window.getSelection()?.toString()).toBe("");
    const annotation: Annotation = {
      id: "shared-note",
      documentId: "doc",
      kind: "note",
      color: "#e6b94c",
      quote: selected.text,
      note: "同一条批注",
      createdAt: "",
      location: selected.location,
    };
    await act(async () =>
      root.render(<App epub={epub} annotations={[annotation]} />),
    );
    const marks = [...registry.entries()].filter(([name]) =>
      name.includes("-highlight-"),
    );
    expect(marks.flatMap(([, ranges]) => texts(ranges)).sort()).toEqual(
      ["Original sentence.", "翻译句子。"].sort(),
    );
    for (const [selector, x] of [
      [source, 5],
      ["[data-sentence]", 205],
    ] as const) {
      await pointer(selector, "pointerdown", x);
      await pointer(selector, "click", x);
      expect(events.annotation).toHaveBeenLastCalledWith(
        expect.objectContaining({ ids: ["shared-note"] }),
      );
    }
  },
);

it("uses restored original anchors for annotations saved from the model's sentence text", async () => {
  const matching = vi.spyOn(engine, "sentenceRanges");
  try {
    await act(async () => root.render(<App />));
    await pointer("[data-sentence]", "pointermove", 205);
    const selected = events.sentenceHover.mock
      .lastCall?.[0] as import("@neoread/core").ReaderSelection;
    const annotation: Annotation = {
      id: "legacy-anchor",
      documentId: "doc",
      kind: "highlight",
      color: "#5b9fe8",
      quote: selected.text,
      note: "",
      createdAt: "",
      location: selected.location,
    };
    await act(async () => window.dispatchEvent(new Event("blur")));
    matching.mockClear();
    const anchor = "Original sen- tence.";
    await act(async () =>
      root.render(
        <App
          annotations={[annotation]}
          translations={[{ ...t, sentences: [{ ...t.sentences[0], anchor }] }]}
        />,
      ),
    );
    const marks = [...registry.entries()]
      .filter(([name]) => name.includes("-highlight-5b9fe8"))
      .flatMap(([, ranges]) => texts(ranges))
      .sort();
    expect(marks).toEqual(["Original sentence.", "翻译句子。"].sort());
    expect(matching).toHaveBeenCalledWith([
      expect.objectContaining({ sources: [anchor] }),
    ]);
    expect(annotation.location.translation?.sourceText).toBe(
      "Original sentence.",
    );
  } finally {
    matching.mockRestore();
  }
});

it("retains the same bilingual annotation when re-parsing changes its source block ID", async () => {
  const moved = { ...blocks[0], id: "p1-b2" };
  const sourceBlocks = [
    { ...blocks[0], text: "An unrelated paragraph." },
    moved,
  ];
  const match = vi
    .spyOn(engine, "sentenceRanges")
    .mockImplementation((passages) => {
      if (passages[0]?.blockId !== moved.id) return [];
      const range = document.createRange();
      range.selectNodeContents(
        host.querySelector("[data-source]")!.firstChild!,
      );
      return [range];
    });
  try {
    await act(async () => root.render(<App sourceBlocks={sourceBlocks} />));
    await pointer("[data-sentence]", "pointermove", 205);
    const selected = events.sentenceHover.mock
      .lastCall?.[0] as import("@neoread/core").ReaderSelection;
    const annotation: Annotation = {
      id: "relocated-note",
      documentId: "doc",
      kind: "highlight",
      color: "#5b9fe8",
      quote: selected.text,
      note: "",
      createdAt: "",
      location: selected.location,
    };
    await act(async () =>
      root.render(
        <App sourceBlocks={sourceBlocks} annotations={[annotation]} />,
      ),
    );
    const colored = () =>
      [...registry.entries()]
        .filter(([name]) => name.includes("-highlight-5b9fe8"))
        .flatMap(([, ranges]) => texts(ranges))
        .sort();
    expect(colored()).toEqual(["Original sentence.", "翻译句子。"].sort());
    expect(match).toHaveBeenCalledWith([
      expect.objectContaining({ blockId: moved.id, sourceOffset: 0 }),
    ]);
    await act(async () =>
      root.render(
        <App
          sourceBlocks={[...sourceBlocks, { ...moved, id: "p1-b3" }]}
          annotations={[annotation]}
        />,
      ),
    );
    expect(colored()).toEqual(["翻译句子。"]);
    await act(async () =>
      root.render(
        <App
          sourceBlocks={sourceBlocks}
          annotations={[annotation]}
          translations={[]}
        />,
      ),
    );
    expect(colored()).toEqual(["Original sentence."]);
    await pointer("[data-source]", "pointerdown", 5);
    await pointer("[data-source]", "click", 5);
    expect(events.annotation).toHaveBeenLastCalledWith(
      expect.objectContaining({ ids: ["relocated-note"] }),
    );
  } finally {
    match.mockRestore();
  }
});

it("keeps the dwell toolbar alive while entering it and clears it on outside actions", async () => {
  await act(async () => root.render(<App />));
  const toolbar = document.createElement("div");
  toolbar.className = "selection-bar";
  document.body.append(toolbar);
  try {
    await pointer("[data-sentence]", "pointermove", 205);
    expect(events.sentenceHover.mock.lastCall?.[0]).toBeTruthy();
    await pointer("[data-sentence]", "pointerleave", 205);
    await act(async () => {
      toolbar.dispatchEvent(new MouseEvent("pointermove", { bubbles: true }));
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(events.sentenceHover.mock.lastCall?.[0]).toBeTruthy();
    await act(async () =>
      document.body.dispatchEvent(
        new MouseEvent("pointerdown", { bubbles: true }),
      ),
    );
    expect(events.sentenceHover).toHaveBeenLastCalledWith(null);
    expect(hover()).toBeUndefined();
    await pointer("[data-sentence]", "pointermove", 205);
    await act(async () => window.dispatchEvent(new Event("blur")));
    expect(events.sentenceHover).toHaveBeenLastCalledWith(null);
  } finally {
    toolbar.remove();
  }
});
