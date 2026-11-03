// @vitest-environment jsdom
import { expect, it } from "vitest";
import type { PDFBlock, TranslationBlock } from "@neoread/core";
import { translatedSelection } from "./selection";
import { prepareSentenceAnnotation } from "./sentence-annotation";

it("shares an explicitly complete translated sentence but keeps a partial selection local", () => {
  const host = document.createElement("div");
  host.innerHTML =
    '<section data-translation-block="p1-b1"><span data-sentence="0">整句译文。</span></section>';
  const block: PDFBlock = {
    id: "p1-b1",
    page: 1,
    label: "text",
    text: "A complete sentence.",
    bounds: { x: 0, y: 0, width: 1, height: 1 },
  };
  const translation: TranslationBlock = {
    blockId: block.id,
    sourceHash: "hash",
    status: "complete",
    sentences: [{ source: block.text, target: "整句译文。" }],
  };
  const range = document.createRange();
  range.selectNodeContents(host.querySelector("span")!);
  range.getBoundingClientRect = () => new DOMRect(0, 0, 100, 20);
  const full = translatedSelection(
    host,
    range,
    [block],
    [translation],
  )!.selection;
  expect(
    prepareSentenceAnnotation(full).location.translation?.ranges?.[0].bilingual,
  ).toBe(true);
  expect(full.location.translation?.ranges?.[0].bilingual).toBeUndefined();
  range.setEnd(host.querySelector("span")!.firstChild!, 2);
  const partial = translatedSelection(
    host,
    range,
    [block],
    [translation],
  )!.selection;
  expect(
    prepareSentenceAnnotation(partial).location.translation?.ranges?.[0]
      .bilingual,
  ).toBeUndefined();
});
