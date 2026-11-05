import { latexGreekText } from "../../../../packages/neoread-core/src/math-symbols";
// Inputs use the same whitespace-free, lower-case text as PDF selection offsets.
// OCR sometimes moves mathematical subscripts to the end of a line. Match the
// complete prose in that case, retaining the original PDF offsets for painting.
const mathLetter = /[\u{1d400}-\u{1d7cb}]/u;

export function sentenceMathOffsets(text: string, glyphs: Set<string>) {
  const offsets = new Set<number>();
  for (const match of text.matchAll(
    /(?<![\p{L}\p{N}])[\p{L}\p{N}](?![\p{L}\p{N}])/gu,
  )) {
    if (!glyphs.has(match[0].normalize("NFKD").toLowerCase())) continue;
    const start = text.slice(0, match.index).replace(/[\s\u00ad]/g, "").length;
    for (let i = 0; i < match[0].length; i++) offsets.add(start + i);
  }
  // Legacy line sorting can leave an operator plus a lower limit (Fk=1)
  // between prose words. Require its letters to exist in native math fonts.
  for (const match of text.matchAll(
    /(?<![\p{L}\p{N}])([a-z]{1,3})=\d+(?![\p{L}\p{N}])/giu,
  )) {
    if (![...match[1]].every((c) => glyphs.has(c.toLowerCase()))) continue;
    const start = text.slice(0, match.index).replace(/[\s\u00ad]/g, "").length;
    for (let i = 0; i < match[0].length; i++) offsets.add(start + i);
  }
  return offsets;
}

type SentenceMath = {
  text: Set<number>;
  block: Set<number>;
  sources: Set<number>[];
  inlineText?: Set<number>;
  inlineBlock?: Set<number>;
  operatorGlyphs?: Set<string>;
};

function prose(text: string, math?: Set<number>) {
  let value = "",
    offset = 0;
  const starts: number[] = [],
    ends: number[] = [];
  for (const char of text) {
    if (!mathLetter.test(char) && !math?.has(offset)) {
      value += char;
      for (let i = 0; i < char.length; i++) {
        starts.push(offset);
        ends.push(offset + char.length);
      }
    }
    offset += char.length;
  }
  return { value, starts, ends };
}
function uniqueIndex(text: string, needle: string) {
  if (!needle) return -1;
  const start = text.indexOf(needle);
  return start >= 0 && text.indexOf(needle, start + 1) < 0 ? start : -1;
}
function symbols(text: string, math?: Set<number>, base = 0) {
  let offset = base;
  return Array.from(text)
    .filter((c) => {
      const include = mathLetter.test(c) || !!math?.has(offset);
      offset += c.length;
      return include;
    })
    .map((c) => c.normalize("NFKD").toLowerCase())
    .sort()
    .join("");
}

// Version-4 extraction retains inline LaTeX structure. Match the surrounding
// prose exactly and verify every formula's variable/number multiset against
// native PDF glyphs before projecting sentence offsets onto that paragraph.
function inlineMathProjection(text: string, block: string) {
  if (!block.includes("$")) return;
  const parts: { text: string; math: boolean }[] = [];
  let offset = 0;
  for (const match of block.matchAll(/(?<!\\)\$(?:\\.|[^$])+\$/g)) {
    parts.push(
      { text: block.slice(offset, match.index), math: false },
      { text: match[0], math: true },
    );
    offset = match.index! + match[0].length;
  }
  if (!offset) return;
  parts.push({ text: block.slice(offset), math: false });
  if (
    parts
      .filter((p) => !p.math)
      .map((p) => p.text)
      .join("")
      .replace(/[^\p{L}]/gu, "").length < 32
  )
    return;
  const escaped = (value: string) =>
    value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const pattern = new RegExp(
    parts
      .map((p) =>
        p.math
          ? `(.{0,${Math.max(8, p.text.length * 2)}}?)`
          : `(${escaped(p.text)})`,
      )
      .join(""),
    "g",
  );
  const found = pattern.exec(text);
  if (!found) return;
  pattern.lastIndex = found.index + 1;
  if (pattern.exec(text)) return;
  const variables = (value: string) =>
    Array.from(
      latexGreekText(value)
        .replace(/[˜ˆ¯˙¨]/g, "")
        .replace(/\\[a-z]+/gi, "")
        .normalize("NFKD")
        .toLowerCase(),
    )
      .filter((c) => /[\p{L}\p{N}]/u.test(c))
      .sort()
      .join("");
  const starts: number[] = [],
    ends: number[] = [];
  let at = found.index;
  for (let i = 0; i < parts.length; i++) {
    const part = parts[i],
      actual = found[i + 1];
    if (part.math && variables(part.text) !== variables(actual)) return;
    for (let j = 0; j < part.text.length; j++) {
      starts.push(part.math ? at : at + j);
      ends.push(part.math ? at + actual.length : at + j + 1);
    }
    at += actual.length;
  }
  return { starts, ends };
}

// Match the complete prose and variable/number multiset before assigning any
// sentence range. This handles script order changes and legacy math fragments
// moved into prose, while preserving native PDF offsets and sentence boundaries.
function reorderedMathProjection(
  text: string,
  block: string,
  math?: SentenceMath,
) {
  if (!math || !block.includes("$")) return;
  const nativeMathOffsets = math.inlineText ?? math.text;
  const sourceMathOffsets = math.inlineBlock ?? math.block;
  if (!nativeMathOffsets.size) return;
  const flags = Array.from({ length: block.length }, (_, i) =>
    sourceMathOffsets.has(i),
  );
  for (const match of block.matchAll(/(?<!\\)\$(?:\\.|[^$])+\$/g)) {
    for (let i = match.index!; i < match.index! + match[0].length; i++)
      flags[i] = true;
  }
  let sourceProse = "",
    sourceMath = "";
  for (let i = 0; i < block.length; i++) {
    if (flags[i]) sourceMath += block[i];
    else {
      sourceProse += block[i];
    }
  }
  if (sourceProse.length < 32) return;
  const native = prose(text, nativeMathOffsets),
    at = uniqueIndex(native.value, sourceProse);
  if (at < 0) return;
  let first = native.starts[at],
    last = native.ends[at + sourceProse.length - 1];
  if (flags[0]) while (first > 0 && nativeMathOffsets.has(first - 1)) first--;
  if (flags.at(-1))
    while (last < text.length && nativeMathOffsets.has(last)) last++;
  const key = (value: string, legacy = false) => {
    if (legacy && math.operatorGlyphs?.has("f"))
      value = value.replace(/f(?=(?:[a-z]=\d+|[_^]))/gi, "⨆");
    return Array.from(
      latexGreekText(value)
        .replace(/\\(?:bigsqcup|sqcup)(?![a-z])/gi, "⨆")
        .replace(/\\sum(?![a-z])/gi, "∑")
        .replace(/\\[a-z]+/gi, "")
        .replace(/[˜ˆ¯˙¨]/g, "")
        .normalize("NFKD")
        .toLowerCase(),
    )
      .filter((c) => /[\p{L}\p{N}⨆∑]/u.test(c))
      .sort()
      .join("");
  };
  const nativeMath = text
    .slice(first, last)
    .split("")
    .filter((_c, i) => nativeMathOffsets.has(first + i))
    .join("");
  if (key(sourceMath, true) !== key(nativeMath)) return;
  const starts: number[] = [],
    ends: number[] = [];
  let previous = first,
    plain = 0;
  for (let i = 0; i < block.length; i++) {
    if (!flags[i]) {
      starts[i] = native.starts[at + plain];
      ends[i] = native.ends[at + plain];
      previous = ends[i];
      plain++;
    } else {
      starts[i] = previous;
      ends[i] = plain < sourceProse.length ? native.starts[at + plain] : last;
    }
  }
  return { starts, ends };
}

export function sentenceTextRanges(
  text: string,
  block: string,
  sources: string[],
  sourceOffset = 0,
  math?: SentenceMath,
): Array<{ start: number; end: number }> {
  const inline =
    inlineMathProjection(text, block) ??
    reorderedMathProjection(text, block, math);
  const paragraphStart = uniqueIndex(text, block);
  const pdfProse = prose(text, math?.text),
    blockProse = prose(block, math?.block);
  const proseStart = uniqueIndex(pdfProse.value, blockProse.value);
  const wholeParagraph =
    proseStart >= 0 &&
    symbols(block, math?.block) ===
      symbols(
        text.slice(
          pdfProse.starts[proseStart],
          pdfProse.ends[proseStart + blockProse.value.length - 1],
        ),
        math?.text,
        pdfProse.starts[proseStart],
      );
  const matches: Array<{ start: number; end: number }> = [];
  let cursor =
    paragraphStart >= 0 ? paragraphStart + sourceOffset : sourceOffset;
  let sourceCursor = sourceOffset;
  for (const [index, source] of sources.entries()) {
    if (!source) continue;
    const sourceStart = block.indexOf(source, sourceCursor);
    if (sourceStart >= 0) sourceCursor = sourceStart + source.length;
    if (inline && sourceStart >= 0) {
      const start = inline.starts[sourceStart],
        end = inline.ends[sourceStart + source.length - 1];
      matches.push({ start, end });
      cursor = end;
      continue;
    }
    const contextual =
      wholeParagraph && sourceStart >= 0
        ? proseStart +
          prose(block.slice(0, sourceStart), math?.block).value.length
        : -1;
    let start =
      paragraphStart >= 0 && sourceStart >= 0
        ? paragraphStart + sourceStart
        : contextual >= 0
          ? pdfProse.starts[contextual]
          : text.indexOf(source, cursor);
    if (start >= 0 && text.slice(start, start + source.length) === source) {
      cursor = start + source.length;
      matches.push({ start, end: cursor });
      continue;
    }
    const needle = prose(source, math?.sources[index]).value;
    // Without a complete paragraph match, require a unique substantial prose
    // sentence with the same variables. Paragraph context can also account for
    // subscripts that OCR has moved across sentence boundaries.
    if (
      !wholeParagraph &&
      (needle.length < 32 || needle.length < source.length * 0.8)
    )
      continue;
    const projected =
      contextual >= 0 ? contextual : uniqueIndex(pdfProse.value, needle);
    if (
      !needle ||
      projected < 0 ||
      pdfProse.value.slice(projected, projected + needle.length) !== needle
    )
      continue;
    start = pdfProse.starts[projected];
    const end = pdfProse.ends[projected + needle.length - 1];
    if (
      !wholeParagraph &&
      symbols(source, math?.sources[index]) !==
        symbols(text.slice(start, end), math?.text, start)
    )
      continue;
    // Paragraph context resolves repeated sentences; otherwise require a
    // unique full-prose match. This never expands an unmatched sentence.
    cursor = end;
    matches.push({ start, end });
  }
  return matches;
}
