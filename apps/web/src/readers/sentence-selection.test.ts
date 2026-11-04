// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import {
  selectSentence,
  sentenceBounds,
  sentenceRoot,
} from "./sentence-selection";
import { sentenceSegments } from "./sentence-segmentation";

afterEach(() => {
  document.body.innerHTML = "";
  window.getSelection()?.removeAllRanges();
});

/** Select `word` inside `root`, as a native double click would. */
function selectWord(root: Element, word: string) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const at = (node as Text).data.indexOf(word);
    if (at < 0) continue;
    const range = document.createRange();
    range.setStart(node, at);
    range.setEnd(node, at + word.length);
    const selection = window.getSelection()!;
    selection.removeAllRanges();
    selection.addRange(range);
    return selection;
  }
  throw new Error(word);
}

it("finds sentence bounds in English and Chinese", () => {
  const en = "Smith et al. found it. Then it broke! Done";
  const at = (text: string, part: string) => {
    const b = sentenceBounds(text, text.indexOf(part))!;
    return text.slice(b.start, b.end);
  };
  expect(at(en, "found")).toBe("Smith et al. found it.");
  expect(at(en, "broke")).toBe("Then it broke!");
  expect(at(en, "Done")).toBe("Done");
  const zh = "第一句话。第二句很长，对吧？第三句";
  expect(at(zh, "很长")).toBe("第二句很长，对吧？");
});

it.each([
  "The query includes “ money laundering service. ” Using an LLM, we continue.",
  'The query includes "money laundering service. " Using an LLM, we continue.',
  "The query includes ‘money laundering service.\n’ Using an LLM, we continue.",
  'The query includes “money laundering service. " Using an LLM, we continue.',
  'The query includes "money laundering service. ” Using an LLM, we continue.',
])("keeps spaced closing quotes with the selected sentence: %s", (text) => {
  const parts = sentenceSegments(text);
  expect(parts).toHaveLength(2);
  expect(parts[1].segment.trimStart()).toBe("Using an LLM, we continue.");
  expect(parts.map((part) => part.segment).join("")).toBe(text);
  const bounds = sentenceBounds(text, text.indexOf("money"))!;
  expect(text.slice(bounds.start, bounds.end)).toMatch(/[”"’]$/);
  expect(sentenceBounds(text, bounds.end - 1)).toEqual(bounds);
});

it("preserves opening quotes and apostrophes instead of moving them to the previous sentence", () => {
  const text = "Readers' examples end here. \"Don't move this opening quote.\"";
  const parts = sentenceSegments(text);
  expect(parts).toHaveLength(2);
  expect(parts[1].segment).toBe('"Don\'t move this opening quote."');
  expect(parts.map((part) => part.segment).join("")).toBe(text);
  const ending = "A “quoted sentence. ”";
  expect(sentenceSegments(ending).map((part) => part.segment)).toEqual([
    ending,
  ]);
});

it("grows a word to its sentence across PDF text layer lines", () => {
  document.body.innerHTML =
    '<div class="textLayer"><span>Intro text. Attention is</span><br><span>all you need. Next</span><br><span>one.</span></div>';
  const layer = document.querySelector(".textLayer")!;
  const selection = selectWord(layer, "need");
  expect(selectSentence(selection, layer)).toBe(true);
  expect(selection.toString()).toBe("Attention isall you need.");
  // A sentence continues onto the next line. (jsdom drops the <br> newline
  // from the selection text that browsers keep.)
  selectWord(layer, "Next");
  selectSentence(selection, layer);
  expect(selection.toString()).toBe("Nextone.");
});

it("keeps EPUB sentences within their paragraph and ignores outside text", () => {
  document.body.innerHTML =
    "<p id='a'>One <em>two</em> three. Four.</p><p id='b'>Five six.</p>";
  const selection = selectWord(document.querySelector("em")!, "two");
  const root = sentenceRoot(selection.anchorNode)!;
  expect(root.id).toBe("a");
  selectSentence(selection, root);
  expect(selection.toString()).toBe("One two three.");
  expect(selectSentence(selection, document.querySelector("#b")!)).toBe(false);
});
