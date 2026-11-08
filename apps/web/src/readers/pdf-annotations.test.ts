// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act } from "react";
import { PDFBlockOverlay } from "./pdf-blocks";
import type { Annotation, PDFBlock } from "@neoread/core";
import { PDFAnnotationLayer } from "./pdf-annotations";
import * as pdfLineBoxes from "./pdf-line-boxes";
import { readFileSync } from "node:fs";

let host: HTMLDivElement;
let text: HTMLSpanElement;
let layer: PDFAnnotationLayer;
const activate = vi.fn();
const annotation = (
  id: string,
  kind: Annotation["kind"] = "highlight",
): Annotation => ({
  id,
  kind,
  documentId: "book",
  quote: "Marked text",
  note: "",
  color: "#e6b94c",
  createdAt: "",
  location: {
    type: "pdf",
    page: 1,
    rects: [
      { x: 0.1, y: 0.1, width: 0.3, height: 0.05 },
      { x: 0.1, y: 0.2, width: 0.2, height: 0.05 },
    ],
  },
});
function pointer(
  type: string,
  extra: MouseEventInit = {},
  target: EventTarget = text,
) {
  const event = new MouseEvent(type, {
    bubbles: true,
    cancelable: true,
    clientX: 100,
    clientY: 130,
    ...extra,
  });
  target.dispatchEvent(event);
  return event;
}
beforeEach(() => {
  activate.mockClear();
  host = document.createElement("div");
  host.innerHTML =
    '<div class="page" data-page-number="1"><span>Marked text</span><a href="#">Link</a></div>';
  document.body.append(host);
  text = host.querySelector("span")!;
  vi.spyOn(host.firstElementChild!, "getBoundingClientRect").mockReturnValue(
    new DOMRect(20, 40, 600, 800),
  );
  layer = new PDFAnnotationLayer(host, activate);
  layer.setAnnotations([
    annotation("highlight"),
    annotation("note", "note"),
    annotation("underline", "underline"),
  ]);
});
afterEach(() => {
  layer.destroy();
  host.remove();
  window.getSelection()?.removeAllRanges();
});
it("highlights all lines of overlapping annotations, but not gaps between lines", () => {
  pointer("pointermove");
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(6);
  pointer("pointermove", { clientY: 185 });
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(0);
  pointer("pointermove");
  host.dispatchEvent(new Event("pointerleave"));
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(0);
});
it("opens all overlapping records at the clicked line and removes only deleted markers", () => {
  pointer("pointerdown");
  expect(pointer("click").defaultPrevented).toBe(true);
  expect(activate).toHaveBeenCalledExactlyOnceWith({
    ids: ["highlight", "note", "underline"],
    anchor: { x: 100, top: 120, bottom: 160 },
  });
  const retained = host.querySelector('[data-annotation-id="note"]');
  const retainedRect = retained!.firstElementChild;
  layer.setAnnotations([annotation("note", "note")]);
  expect(host.querySelector('[data-annotation-id="note"]')).toBe(retained);
  expect(retained!.firstElementChild).toBe(retainedRect);
  expect(host.querySelectorAll(".neoread-highlight")).toHaveLength(2);
  expect(host.querySelector('[data-annotation-id="highlight"]')).toBeNull();
  // A PDF.js page rerender must not resurrect a deleted annotation.
  layer.paint();
  expect(host.querySelectorAll(".neoread-highlight")).toHaveLength(2);
});
it.each(["highlight", "underline"] as const)(
  "paints CJK %s as continuous lines and keeps hover active between glyphs",
  (kind) => {
    const mark = annotation("cjk", kind);
    if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
    mark.location.rects = [
      { x: 0.1, y: 0.1, width: 0.02, height: 0.02 },
      { x: 0.124, y: 0.102, width: 0.02, height: 0.018 },
      { x: 0.148, y: 0.099, width: 0.02, height: 0.022 },
      { x: 0.1, y: 0.15, width: 0.02, height: 0.02 },
    ];
    const saved = JSON.stringify(mark.location);
    layer.setAnnotations([mark]);
    const rects = [...host.querySelectorAll<HTMLElement>(".neoread-highlight")];
    expect(rects).toHaveLength(2);
    const first = rects[0];
    expect(Number.parseFloat(first.style.top)).toBeCloseTo(9.9);
    expect(Number.parseFloat(first.style.height)).toBeCloseTo(2.2);
    expect(Number.parseFloat(first.style.width)).toBeCloseTo(6.8);
    // This point sits in the original 0.120–0.124 gap between two glyphs.
    const clientX = 20 + 0.122 * 600;
    pointer("pointermove", { clientX });
    expect(host.querySelectorAll("[data-hovered]")).toHaveLength(2);
    const observer = new MutationObserver(() => {});
    observer.observe(host, {
      subtree: true,
      attributes: true,
      childList: true,
    });
    try {
      for (const x of [0.119, 0.122, 0.125])
        pointer("pointermove", { clientX: 20 + x * 600 });
      layer.paint();
      expect(observer.takeRecords()).toHaveLength(0);
    } finally {
      observer.disconnect();
    }
    pointer("pointerdown", { clientX });
    expect(pointer("click", { clientX }).defaultPrevented).toBe(true);
    const target = activate.mock.calls[0][0];
    expect(target.ids).toEqual(["cjk"]);
    expect(target.anchor.top).toBeCloseTo(40 + 0.099 * 800);
    expect(target.anchor.bottom).toBeCloseTo(40 + 0.121 * 800);
    expect(JSON.stringify(mark.location)).toBe(saved);
  },
);
it("places one continuous underline at the bottom of each merged line", () => {
  const style = document.createElement("style");
  // jsdom needs a literal colour to compute border shorthand properties.
  style.textContent = readFileSync("apps/web/src/style.css", "utf8")
    .match(/\.neoread-(?:annotation|highlight)[^{]*\{[^}]*\}/g)!
    .join("\n")
    .replaceAll("var(--annotation-color)", "#e6b94c");
  document.head.append(style);
  const mark = annotation("underline", "underline");
  if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
  mark.location.rects = [
    { x: 0.1, y: 0.1, width: 0.02, height: 0.02 },
    { x: 0.122, y: 0.102, width: 0.02, height: 0.022 },
  ];
  try {
    layer.setAnnotations([mark]);
    const rects = host.querySelectorAll<HTMLElement>(".neoread-highlight");
    expect(rects).toHaveLength(1);
    const line = rects[0];
    expect(
      Number.parseFloat(line.style.top) + Number.parseFloat(line.style.height),
    ).toBeCloseTo(12.4);
    expect(getComputedStyle(line).borderBottomWidth).toBe("2px");
    expect(getComputedStyle(line).borderBottomStyle).toBe("solid");
  } finally {
    style.remove();
  }
});
it("keeps same-height columns separate for drawing and hit testing", () => {
  const mark = annotation("columns");
  if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
  mark.location.rects = [
    { x: 0.1, y: 0.1, width: 0.3, height: 0.02 },
    { x: 0.54, y: 0.1, width: 0.3, height: 0.02 },
  ];
  layer.setAnnotations([mark]);
  expect(host.querySelectorAll(".neoread-highlight")).toHaveLength(2);
  pointer("pointermove", { clientX: 20 + 0.47 * 600 });
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(0);
  pointer("pointermove", { clientX: 20 + 0.6 * 600 });
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(2);
});
it("reuses line boxes for repaint, pointer moves, equivalent records and zoom", () => {
  const merge = vi.spyOn(pdfLineBoxes, "lineBoxes");
  const mark = annotation("cached");
  if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
  mark.location.rects = [
    { x: 0.1, y: 0.1, width: 0.02, height: 0.02 },
    { x: 0.135, y: 0.101, width: 0.02, height: 0.02 },
  ];
  try {
    layer.setAnnotations([mark]);
    expect(merge).toHaveBeenCalledTimes(1);
    expect(host.querySelectorAll(".neoread-highlight")).toHaveLength(1);
    for (let i = 0; i < 10; i++) {
      layer.paint(1);
      pointer("pointermove");
    }
    layer.setAnnotations([JSON.parse(JSON.stringify(mark))]);
    vi.spyOn(host.firstElementChild!, "getBoundingClientRect").mockReturnValue(
      new DOMRect(20, 40, 1200, 1600),
    );
    layer.paint();
    expect(merge).toHaveBeenCalledTimes(1);
    // Changing the displayed aspect ratio must reconsider the physical gap.
    vi.spyOn(host.firstElementChild!, "getBoundingClientRect").mockReturnValue(
      new DOMRect(20, 40, 1600, 800),
    );
    layer.paint();
    expect(merge).toHaveBeenCalledTimes(2);
    expect(host.querySelectorAll(".neoread-highlight")).toHaveLength(2);
    layer.setAnnotations([mark]);
    mark.location.rects[0].x = 0.2;
    layer.paint();
    expect(merge).toHaveBeenCalledTimes(3);
    // Removing the annotation also releases its geometry cache.
    layer.setAnnotations([]);
    layer.setAnnotations([mark]);
    expect(merge).toHaveBeenCalledTimes(4);
  } finally {
    merge.mockRestore();
  }
});
it("composites overlapping rectangles once per annotation, including on hover and repaint", () => {
  // Use the production rules: per-rectangle opacity would darken this overlap.
  const style = document.createElement("style");
  style.textContent = readFileSync("apps/web/src/style.css", "utf8")
    .match(/\.neoread-(?:annotation|highlight)[^{]*\{[^}]*\}/g)!
    .join("\n");
  document.head.append(style);
  const mark = annotation("overlap");
  if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
  mark.location.rects = [
    { x: 0.1, y: 0.1, width: 0.3, height: 0.05 },
    { x: 0.2, y: 0.1, width: 0.3, height: 0.05 },
    { x: 0.1, y: 0.1, width: 0.3, height: 0.05 },
  ];
  try {
    layer.setAnnotations([mark]);
    const assertSingleComposite = (opacity: string) => {
      const rects = [
        ...host.querySelectorAll<HTMLElement>(".neoread-highlight"),
      ];
      expect(rects).toHaveLength(1);
      const group = rects[0].parentElement!;
      expect(group).not.toBe(host.firstElementChild);
      expect(rects.every((rect) => rect.parentElement === group)).toBe(true);
      expect(getComputedStyle(group).opacity).toBe(opacity);
      expect(getComputedStyle(group).mixBlendMode).toBe("multiply");
      for (const rect of rects) {
        expect(["", "1"]).toContain(getComputedStyle(rect).opacity);
        expect(["", "normal"]).toContain(getComputedStyle(rect).mixBlendMode);
      }
    };
    assertSingleComposite("0.32");
    pointer("pointermove");
    assertSingleComposite("0.48");
    layer.paint();
    assertSingleComposite("0.48");
    expect(host.querySelectorAll(".neoread-annotation")).toHaveLength(1);
    layer.setAnnotations([]);
    expect(host.querySelector(".neoread-annotation")).toBeNull();
  } finally {
    style.remove();
  }
});
it("leaves dragging, text selection and native links untouched", () => {
  pointer("pointerdown");
  expect(pointer("click", { clientX: 115 }).defaultPrevented).toBe(false);
  pointer("pointerdown");
  const range = document.createRange();
  range.selectNodeContents(text);
  window.getSelection()!.addRange(range);
  pointer("pointermove");
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(0);
  expect(pointer("click").defaultPrevented).toBe(false);
  window.getSelection()!.removeAllRanges();
  pointer("pointerdown", {}, host.querySelector("a")!);
  expect(pointer("click", {}, host.querySelector("a")!).defaultPrevented).toBe(
    false,
  );
  expect(activate).not.toHaveBeenCalled();
});
it("discards gestures on scroll and detaches listeners on disposal", () => {
  pointer("pointerdown");
  host.dispatchEvent(new Event("scroll"));
  pointer("click");
  layer.destroy();
  pointer("pointerdown");
  pointer("click");
  expect(activate).not.toHaveBeenCalled();
  expect(host.querySelector(".neoread-highlight")).toBeNull();
});

it("keeps block and annotation hover active together, with independent hit regions", async () => {
  const block: PDFBlock = {
    id: "paragraph",
    page: 1,
    label: "paragraph",
    text: "Marked text and surrounding paragraph",
    bounds: { x: 0.05, y: 0.05, width: 0.8, height: 0.4 },
  };
  const hover = vi.fn();
  const action = vi.fn();
  const blocks = new PDFBlockOverlay(host, action, hover);
  blocks.setBlocks([block]);
  try {
    await act(async () => {
      pointer("pointermove");
    });
    expect(
      host.querySelectorAll(".neoread-highlight[data-hovered]"),
    ).toHaveLength(6);
    expect(
      host.querySelector(".neoread-block-hover")?.getAttribute("data-block-id"),
    ).toBe(block.id);
    expect(hover).toHaveBeenLastCalledWith(block);

    // Move into the unmarked space in the same paragraph.
    await act(async () => {
      pointer("pointermove", { clientY: 185 });
    });
    expect(
      host.querySelectorAll(".neoread-highlight[data-hovered]"),
    ).toHaveLength(0);
    expect(host.querySelector(".neoread-block-hover")).not.toBeNull();
    expect(hover).toHaveBeenCalledTimes(1);

    await act(async () => {
      pointer("pointermove");
    });
    pointer("pointerdown");
    pointer("click");
    expect(activate).toHaveBeenCalledTimes(1);
    expect(action).not.toHaveBeenCalled();

    host.dispatchEvent(new Event("pointerleave"));
    expect(
      host.querySelectorAll(".neoread-highlight[data-hovered]"),
    ).toHaveLength(0);
    expect(host.querySelector(".neoread-block-hover")).toBeNull();
    expect(hover).toHaveBeenLastCalledWith(null);
  } finally {
    await act(async () => blocks.destroy());
  }
});

// PDFReadingView filters the source annotations on every render. Hover/selection
// updates can therefore repaint the layer between pointerdown and click.
it.each(["highlight", "underline"] as const)(
  "opens a source %s even when React refreshes annotations during the click",
  (kind) => {
    const marks = [annotation("source", kind)];
    layer.setAnnotations(marks);
    pointer("pointerdown");
    layer.setAnnotations([...marks]);
    expect(pointer("click").defaultPrevented).toBe(true);
    expect(activate).toHaveBeenCalledExactlyOnceWith({
      ids: ["source"],
      anchor: { x: 100, top: 120, bottom: 160 },
    });
  },
);

it("does not open an annotation deleted during a click", () => {
  pointer("pointerdown");
  layer.setAnnotations([]);
  expect(pointer("click").defaultPrevented).toBe(false);
  expect(activate).not.toHaveBeenCalled();
});

it("gives annotation clicks priority over block focus, including after repaint", async () => {
  const block: PDFBlock = {
    id: "paragraph",
    page: 1,
    label: "paragraph",
    text: "Marked text and surrounding paragraph",
    bounds: { x: 0.05, y: 0.05, width: 0.8, height: 0.4 },
  };
  const focus = vi.fn(() => true);
  // Match PDFReaderAdapter: both capture listeners share the same host, with
  // the annotation layer registered first. stopPropagation alone is not enough.
  const blocks = new PDFBlockOverlay(host, vi.fn(), vi.fn(), focus);
  blocks.setBlocks([block]);
  try {
    pointer("pointerdown");
    layer.paint();
    pointer("click");
    expect(activate).toHaveBeenCalledTimes(1);
    expect(focus).not.toHaveBeenCalled();

    // Unmarked space in the same block must still focus normally.
    pointer("pointerdown", { clientY: 185 });
    pointer("click", { clientY: 185 });
    expect(focus).toHaveBeenCalledExactlyOnceWith(block);
    expect(activate).toHaveBeenCalledTimes(1);
  } finally {
    await act(async () => blocks.destroy());
  }
});

it("preserves group, rectangle identities and hover without DOM writes for equivalent data", () => {
  pointer("pointermove");
  const groups = [...host.querySelectorAll(".neoread-annotation")];
  const rects = [...host.querySelectorAll(".neoread-highlight")];
  const observer = new MutationObserver(() => {});
  observer.observe(host, { subtree: true, childList: true, attributes: true });
  try {
    layer.paint();
    layer.setAnnotations([
      annotation("highlight"),
      annotation("note", "note"),
      annotation("underline", "underline"),
    ]);
    expect(observer.takeRecords()).toHaveLength(0);
    groups.forEach((group, i) =>
      expect(host.querySelectorAll(".neoread-annotation")[i]).toBe(group),
    );
    rects.forEach((rect, i) =>
      expect(host.querySelectorAll(".neoread-highlight")[i]).toBe(rect),
    );
    expect(host.querySelectorAll("[data-hovered]")).toHaveLength(6);
  } finally {
    observer.disconnect();
  }
});

it("updates in-place color, kind and geometry while leaving another page untouched", () => {
  const page2 = document.createElement("div");
  page2.className = "page";
  page2.dataset.pageNumber = "2";
  host.append(page2);
  const first = annotation("first");
  const second = annotation("second");
  if (first.location.type !== "pdf" || second.location.type !== "pdf")
    throw new Error("Expected PDF locations");
  second.location.page = 2;
  layer.setAnnotations([first, second]);
  const group = host.querySelector<HTMLElement>(".neoread-annotation")!;
  const rect = group.firstElementChild as HTMLElement;
  const other = page2.firstElementChild!;
  const otherRect = other.firstElementChild!;
  const observer = new MutationObserver(() => {});
  observer.observe(page2, { subtree: true, childList: true, attributes: true });
  try {
    first.color = "#123456";
    first.kind = "underline";
    first.location.rects![0].x = 0.25;
    first.location.rects![0].y = 0.3;
    first.location.rects![0].width = 0.4;
    first.location.rects![0].height = 0.1;
    first.location.rects!.pop();
    layer.setAnnotations([first, second]);
    expect(host.querySelector(".neoread-annotation")).toBe(group);
    expect(group.firstElementChild).toBe(rect);
    expect(group.children).toHaveLength(1);
    expect(group.style.getPropertyValue("--annotation-color")).toBe("#123456");
    expect(rect.dataset).toMatchObject({
      annotationId: "first",
      kind: "underline",
    });
    expect([
      rect.style.left,
      rect.style.top,
      rect.style.width,
      rect.style.height,
    ]).toEqual(["25%", "30%", "40%", "10%"]);
    first.color = "";
    first.location.rects!.push({ x: 0, y: 0, width: 1, height: 1 });
    layer.paint();
    expect(group.style.getPropertyValue("--annotation-color")).toBe("#facc15");
    // The full-page rectangle contains the prior line and normalizes to one box.
    expect(group.children).toHaveLength(1);
    expect(group.firstElementChild).toBe(rect);
    layer.setAnnotations([second]);
    expect(group.isConnected).toBe(false);
    expect(page2.firstElementChild).toBe(other);
    expect(other.firstElementChild).toBe(otherRect);
    expect(observer.takeRecords()).toHaveLength(0);
    layer.paint();
    expect(host.querySelector('[data-annotation-id="first"]')).toBeNull();
  } finally {
    observer.disconnect();
  }
});

it.each(["reset", "replace", "detach"] as const)(
  "restores current annotations after a page %s",
  (operation) => {
    const current = annotation("current");
    layer.setAnnotations([current, annotation("removed")]);
    const oldPage = host.firstElementChild!;
    const oldGroup = oldPage.querySelector(".neoread-annotation")!;
    let page = oldPage;
    if (operation === "reset") {
      oldPage.replaceChildren();
    } else {
      page = document.createElement("div");
      page.className = "page";
      (page as HTMLElement).dataset.pageNumber = "1";
      if (operation === "replace") oldPage.replaceWith(page);
      else {
        oldPage.remove();
        layer.paint();
      }
    }
    current.color = "#112233";
    layer.setAnnotations([current]);
    if (operation === "detach") host.append(page);
    layer.paint();
    const group = page.querySelector<HTMLElement>(".neoread-annotation")!;
    expect(group).not.toBeNull();
    expect(group).not.toBe(oldGroup);
    expect(group.style.getPropertyValue("--annotation-color")).toBe("#112233");
    expect(page.querySelectorAll(".neoread-highlight")).toHaveLength(2);
    expect(page.querySelector('[data-annotation-id="removed"]')).toBeNull();
    layer.paint();
    expect(page.querySelector(".neoread-annotation")).toBe(group);
  },
);

it("paints a rebuilt page without querying or modifying other pages", () => {
  const page1 = host.firstElementChild!;
  const page2 = page1.cloneNode(false) as HTMLElement;
  page2.dataset.pageNumber = "2";
  host.append(page2);
  const second = annotation("second");
  if (second.location.type !== "pdf") throw new Error("Expected PDF location");
  second.location.page = 2;
  layer.setAnnotations([annotation("first"), second]);
  const other = page2.firstElementChild;
  page1.replaceChildren();
  const query = vi.spyOn(page2, "querySelector");
  layer.paint(1);
  expect(page1.querySelector('[data-annotation-id="first"]')).not.toBeNull();
  expect(page2.firstElementChild).toBe(other);
  expect(query).not.toHaveBeenCalled();
  layer.setAnnotations([]);
  expect(host.querySelector(".neoread-annotation")).toBeNull();
});

it("removes marks with empty geometry or bookmark kind and restores later geometry", () => {
  const mark = annotation("changing");
  if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
  layer.setAnnotations([mark]);
  mark.location.rects = [];
  layer.paint();
  expect(host.querySelector(".neoread-annotation")).toBeNull();
  mark.location.rects = [{ x: 0, y: 0, width: 1, height: 1 }];
  layer.paint();
  expect(host.querySelectorAll(".neoread-highlight")).toHaveLength(1);
  mark.kind = "bookmark";
  layer.paint();
  expect(host.querySelector(".neoread-annotation")).toBeNull();
});

it("reconciles a reattached page against changes made while it was detached", () => {
  const page = host.firstElementChild!;
  const retained = page.querySelector('[data-annotation-id="note"]');
  page.remove();
  layer.setAnnotations([annotation("note", "note")]);
  host.append(page);
  layer.paint();
  expect(page.querySelectorAll(".neoread-annotation")).toHaveLength(1);
  expect(page.querySelector('[data-annotation-id="note"]')).toBe(retained);
  expect(page.querySelector('[data-annotation-id="highlight"]')).toBeNull();
  expect(page.querySelector('[data-annotation-id="underline"]')).toBeNull();
});

it("reorders overlapping colors without replacing groups or moving an unchanged order", () => {
  const first = annotation("first");
  const second = annotation("second");
  second.color = "#123456";
  layer.setAnnotations([first, second]);
  const page = host.firstElementChild!;
  const [firstGroup, secondGroup] = [
    ...page.querySelectorAll(".neoread-annotation"),
  ];
  const firstRect = firstGroup.firstElementChild;
  const secondRect = secondGroup.firstElementChild;
  pointer("pointermove");
  layer.setAnnotations([second, first]);
  const reordered = [...page.querySelectorAll(".neoread-annotation")];
  expect(reordered[0]).toBe(secondGroup);
  expect(reordered[1]).toBe(firstGroup);
  expect(firstGroup.firstElementChild).toBe(firstRect);
  expect(secondGroup.firstElementChild).toBe(secondRect);
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(4);
  const observer = new MutationObserver(() => {});
  observer.observe(page, { childList: true, subtree: true, attributes: true });
  try {
    layer.paint();
    layer.setAnnotations([second, first]);
    expect(observer.takeRecords()).toHaveLength(0);
  } finally {
    observer.disconnect();
  }
});

it("does not rewrite percentages rounded by CSS serialization on equivalent paints", () => {
  const mark = annotation("fractional");
  if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
  mark.location.rects = [
    {
      x: 0.1 + 0.2,
      y: 1 / 3,
      width: 0.1 + 0.2,
      height: 1 / 7,
    },
  ];
  layer.setAnnotations([mark]);
  const rect = host.querySelector<HTMLElement>(".neoread-highlight")!;
  const originalStyle = rect.style.cssText;
  const observer = new MutationObserver(() => {});
  observer.observe(host, { subtree: true, attributes: true, childList: true });
  try {
    layer.paint();
    layer.setAnnotations([JSON.parse(JSON.stringify(mark))]);
    expect(rect.style.cssText).toBe(originalStyle);
    expect(observer.takeRecords()).toHaveLength(0);
    mark.location.rects[0].x = 0.4;
    layer.setAnnotations([mark]);
    expect(rect.style.left).toBe("40%");
    observer.takeRecords();
    layer.paint();
    expect(observer.takeRecords()).toHaveLength(0);
  } finally {
    observer.disconnect();
  }
});

it("skips setters when the browser serializes a percentage with fewer digits", () => {
  const descriptor = Object.getOwnPropertyDescriptor(
    CSSStyleDeclaration.prototype,
    "left",
  )!;
  const setter = vi
    .spyOn(CSSStyleDeclaration.prototype, "left", "set")
    .mockImplementation(function (this: CSSStyleDeclaration, value: string) {
      descriptor.set!.call(
        this,
        value.endsWith("%")
          ? Number.parseFloat(value).toPrecision(6) + "%"
          : value,
      );
    });
  try {
    const mark = annotation("rounded");
    if (mark.location.type !== "pdf") throw new Error("Expected PDF location");
    mark.location.rects![0].x = 0.1 + 0.2;
    layer.setAnnotations([mark]);
    const rect = host.querySelector<HTMLElement>(".neoread-highlight")!;
    expect(Number.parseFloat(rect.style.left)).toBe(30);
    setter.mockClear();
    layer.paint();
    layer.setAnnotations([JSON.parse(JSON.stringify(mark))]);
    expect(setter).not.toHaveBeenCalled();
    mark.location.rects![0].x = 0.4;
    layer.setAnnotations([mark]);
    expect(setter).toHaveBeenCalledTimes(1);
    expect(Number.parseFloat(rect.style.left)).toBe(40);
    setter.mockClear();
    layer.paint();
    expect(setter).not.toHaveBeenCalled();
  } finally {
    setter.mockRestore();
  }
});

it("does not rescan annotation DOM or serialize selected text on repeated drag moves", () => {
  pointer("pointermove");
  pointer("pointerdown", { buttons: 1 });
  const range = document.createRange();
  range.selectNodeContents(text);
  const selection = window.getSelection()!;
  selection.addRange(range);
  const serialize = vi.spyOn(selection, "toString");
  const query = vi.spyOn(host, "querySelectorAll");
  for (let i = 0; i < 30; i++) pointer("pointermove", { buttons: 1 });
  pointer("pointermove");
  expect(query).not.toHaveBeenCalled();
  expect(serialize).not.toHaveBeenCalled();
  query.mockRestore();
  serialize.mockRestore();
  window.getSelection()!.removeAllRanges();
  pointer("pointermove");
  expect(host.querySelectorAll("[data-hovered]")).toHaveLength(6);
});
