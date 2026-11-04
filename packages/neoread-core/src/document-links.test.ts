import { describe, expect, it } from "vitest";
import { codexThreadURL } from "./codex-link";
import { parseNeoReadLink, neoreadLink } from "./neoread-link";
import { locationLabel, type DocumentLocation } from "./index";
import { documentTabCommand } from "./document-tab-shortcut";

describe("codex-link", () => {
  it("opens only an exact local thread id", () => {
    expect(codexThreadURL("01900000-0000-7000-8000-000000000002")).toBe(
      "codex://threads/01900000-0000-7000-8000-000000000002",
    );
    for (const id of [
      null,
      {},
      "",
      "new",
      "../new",
      "https://example.com",
      "01900000-0000-7000-8000-000000000002?prompt=run",
      "01900000-0000-7000-8000-000000000002\n",
    ]) {
      expect(() => codexThreadURL(id)).toThrow();
    }
  });
});

describe("neoread-link", () => {
  const id = "0123456789abcdef0123456789abcdef";

  it("round-trips document, annotation and page targets", () => {
    for (const target of [
      { id },
      { id, annotation: "fedcba9876543210fedcba9876543210" },
      { id, page: 12 },
    ])
      expect(parseNeoReadLink(neoreadLink(target))).toEqual(target);
  });

  it("rejects links exported with the old scheme", () => {
    expect(neoreadLink({ id })).toBe(`neoread://open?id=${id}`);
    expect(parseNeoReadLink(`reader://open?id=${id}&page=3`)).toBeNull();
  });

  it("rejects other schemes, actions and malformed parameters", () => {
    for (const link of [
      `https://open?id=${id}`,
      `neoread://delete?id=${id}`,
      "neoread://open?id=../../etc",
      "not a link",
    ])
      expect(parseNeoReadLink(link)).toBeNull();
    expect(
      parseNeoReadLink(`neoread://open?id=${id}&annotation=x&page=-1&extra=1`),
    ).toEqual({ id });
  });
});

describe("location", () => {
  describe("format-specific persistent locations", () => {
    it("retains EPUB locators without inventing page numbers", () => {
      const saved: DocumentLocation = {
        type: "epub",
        href: "chapter.xhtml",
        locator: JSON.stringify({
          href: "chapter.xhtml",
          locations: { progression: 0.32 },
          text: { highlight: "quoted passage" },
        }),
        progression: 0.32,
      };
      const restored: DocumentLocation = JSON.parse(JSON.stringify(saved));
      expect(restored).toEqual(saved);
      expect(locationLabel(restored)).toBe("章节进度 32%");
      expect(restored).not.toHaveProperty("page");
    });
    it("labels a PDF page and preserves selection rectangles", () => {
      const saved: DocumentLocation = {
        type: "pdf",
        page: 12,
        rects: [{ x: 0.1, y: 0.2, width: 0.4, height: 0.02 }],
      };
      expect(locationLabel(saved)).toBe("第 12 页");
      expect(JSON.parse(JSON.stringify(saved)).rects[0].width).toBe(0.4);
    });
  });
});

describe("document-tab-shortcut", () => {
  it("supports native close, tab cycling and numbered selection without consuming ordinary editing", () => {
    expect(
      documentTabCommand({ key: "t", metaKey: true, shiftKey: true }),
    ).toEqual({ type: "reopen" });
    expect(documentTabCommand({ key: "w", metaKey: true })).toEqual({
      type: "close",
    });
    expect(documentTabCommand({ key: "w", ctrlKey: true })).toEqual({
      type: "close",
    });
    expect(documentTabCommand({ key: "Tab", ctrlKey: true })).toEqual({
      type: "next",
    });
    expect(
      documentTabCommand({ key: "Tab", ctrlKey: true, shiftKey: true }),
    ).toEqual({ type: "previous" });
    expect(documentTabCommand({ key: "3", metaKey: true })).toEqual({
      type: "select",
      index: 2,
    });
    expect(documentTabCommand({ key: "9", metaKey: true })).toEqual({
      type: "select",
      index: -1,
    });
    for (const input of [
      { key: "w" },
      { key: "Tab" },
      { key: "w", metaKey: true, shiftKey: true },
      { key: "w", metaKey: true, isComposing: true },
      { key: "1", ctrlKey: true, altKey: true },
    ])
      expect(documentTabCommand(input)).toBeNull();
  });
});
