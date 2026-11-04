import { expect, it } from "vitest";
import {
  sentenceTextRanges,
  sentenceMathOffsets,
} from "./pdf-sentence-matching";
import midas from "../../../processor/src/fixtures/midas-sentence-math.json";

function midasNative(spans = midas.spans) {
  let text = "";
  const offsets = new Set<number>(),
    glyphs = new Set<string>(),
    operatorGlyphs = new Set<string>();
  for (const span of spans)
    for (const original of span.text) {
      if (/\s/.test(original)) continue;
      let char = original.toLowerCase();
      if (span.math) {
        offsets.add(text.length);
        glyphs.add(char);
      }
      if (span.text === "F" && span.math) {
        char = "⨆";
        operatorGlyphs.add("f");
      }
      text += char;
    }
  return {
    text,
    math: {
      text: offsets,
      block: sentenceMathOffsets(midas.legacy, glyphs),
      sources: [sentenceMathOffsets(midas.sentence, glyphs)],
      operatorGlyphs,
    },
  };
}

it("maps the reported MIDAS sentence despite a legacy lower limit moved into prose", () => {
  const native = midasNative();
  const source = normalized(midas.legacy),
    sentence = normalized(midas.sentence);
  const start = native.text.indexOf("formally,let"),
    end = native.text.indexOf(".weconstruct", start) + 1;
  expect(start).toBeGreaterThan(0);
  expect(
    sentenceTextRanges(native.text, source, [sentence], 0, native.math),
  ).toEqual([{ start, end }]);
  const following = "weconstruct";
  expect(
    sentenceTextRanges(native.text, source, [following], 0, {
      ...native.math,
      sources: [new Set()],
    }),
  ).toEqual([{ start: end, end: native.text.length }]);
});

it("does not use MIDAS's paragraph projection when a variable or the surrounding prose differs", () => {
  const changed = midas.spans.map((span, i) =>
    i === midas.spans.length - 2 ? { ...span, text: "l" } : span,
  );
  for (const spans of [
    changed,
    midas.spans.map((s) => ({
      ...s,
      text: s.text.replace("Formally, let", "Unrelated clause"),
    })),
  ]) {
    const native = midasNative(spans);
    expect(
      sentenceTextRanges(
        native.text,
        normalized(midas.legacy),
        [normalized(midas.sentence)],
        0,
        native.math,
      ),
    ).toEqual([]);
  }
});

const normalized = (s: string) => s.toLowerCase().replace(/\s/g, "");
const source = normalized(
  "Each time 𝑡 merges a 𝑖 record-local graph, it checks all followers.",
);
const pdf = normalized(
  "Each time 𝑡𝑖 merges a record-local graph, it checks all followers.",
);

it("maps ordinary inline formulas to PDF glyphs while keeping each variable", () => {
  const source = normalized(
    "The attention mechanism uses $d_k=64$ dimensions and reduces the cost for sequences of length $n$.",
  );
  const native = normalized(
    "The attention mechanism uses dk=64 dimensions and reduces the cost for sequences of length n.",
  );
  expect(sentenceTextRanges(native, source, [source])).toEqual([
    { start: 0, end: native.length },
  ]);
  const mathematical = native.replace("dk=64", "𝐷ₖ=64");
  expect(sentenceTextRanges(mathematical, source, [source])).toEqual([
    { start: 0, end: mathematical.length },
  ]);
  expect(
    sentenceTextRanges(native.replace("dk=64", "dl=64"), source, [source]),
  ).toEqual([]);
  expect(sentenceTextRanges(native + native, source, [source])).toEqual([]);
});

it("maps structured inline scripts to native glyph order without accepting different variables", () => {
  const first = normalized("where CS denotes the cosine similarity.");
  const second = normalized(
    "By maximizing $\\mathcal{M}_{\\mathcal{T}_{s},\\mathcal{T}_{t}}$, each $\\tilde{\\mathbf{x}}_{i}^{s}$ effectively captures certain semantic $\\hat{\\mathbf{x}}_{i}^{t}$ from the target image.",
  );
  const native = normalized(
    "By maximizing MTs,Tt, each ˜xsi effectively captures certain semantic ˆxti from the target image.",
  );
  expect(
    sentenceTextRanges(first + native, first + second, [first, second]),
  ).toEqual([
    { start: 0, end: first.length },
    { start: first.length, end: first.length + native.length },
  ]);
  expect(
    sentenceTextRanges(first + native.replace("xti", "xji"), first + second, [
      second,
    ]),
  ).toEqual([]);
  expect(
    sentenceTextRanges(first + native + first + native, first + second, [
      second,
    ]),
  ).toEqual([]);
});

it("keeps reordered subscripts in the full PDF sentence range", () => {
  const text = "preceding." + pdf + "following.";
  expect(sentenceTextRanges(text, source, [source])).toEqual([
    { start: 10, end: 10 + pdf.length },
  ]);
});

it("does not accept different prose or different mathematical variables", () => {
  expect(
    sentenceTextRanges(pdf.replace("allfollowers", "nofollowers"), source, [
      source,
    ]),
  ).toEqual([]);
  expect(sentenceTextRanges(pdf.replace("𝑖", "𝑗"), source, [source])).toEqual(
    [],
  );
  expect(
    sentenceTextRanges("𝑡𝑖=1.", "Unrelated paragraph.", ["𝑡=1𝑖."]),
  ).toEqual([]);
});

it("uses the full paragraph when OCR moves a subscript across a sentence boundary", () => {
  const first = normalized(
    "Each time 𝑡 merges a record-local graph, it checks all followers.",
  );
  const second = normalized(
    "𝑖 The transaction checks every record before committing.",
  );
  const actualSecond = normalized(
    "The transaction checks every record before committing.",
  );
  const text = pdf + actualSecond;
  expect(sentenceTextRanges(text, first + second, [first, second])).toEqual([
    { start: 0, end: pdf.length },
    { start: pdf.length, end: text.length },
  ]);
});

it("rejects ambiguous prose without paragraph context and resolves it with source order", () => {
  const paragraph = source + source;
  const text = pdf + pdf;
  expect(sentenceTextRanges(text, "Unrelated paragraph.", [source])).toEqual(
    [],
  );
  expect(sentenceTextRanges(text, paragraph, [source], source.length)).toEqual([
    { start: pdf.length, end: pdf.length * 2 },
  ]);
});
