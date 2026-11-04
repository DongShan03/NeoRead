const segmenter = new Intl.Segmenter(undefined, { granularity: "sentence" });
const word = (char: string) => /[\p{L}\p{N}]/u.test(char);

/** Match server sentence_quotes.go: whitespace must not strand a closing quote. */
export function sentenceSegments(
  text: string,
): { index: number; segment: string }[] {
  const closing = new Set<number>();
  let at = 0,
    previous = "",
    slashes = 0,
    double = false,
    single = false;
  for (const char of text) {
    if (char === "\\") {
      slashes++;
      previous = char;
      at += char.length;
      continue;
    }
    const escaped = slashes % 2 === 1;
    slashes = 0;
    const next = text.codePointAt(at + char.length);
    const apostrophe =
      (char === "'" || char === "’") &&
      word(previous) &&
      ((next !== undefined && word(String.fromCodePoint(next))) ||
        (char === "'" && !single));
    if (!escaped && !apostrophe) {
      if (char === "“") double = true;
      else if (char === "”") {
        closing.add(at);
        double = false;
      } else if (char === "‘") single = true;
      else if (char === "’") {
        closing.add(at);
        single = false;
      } else if (char === '"') {
        if (double) closing.add(at);
        double = !double;
      } else if (char === "'") {
        if (single) closing.add(at);
        single = !single;
      } else if ("»›」』".includes(char)) closing.add(at);
    }
    previous = char;
    at += char.length;
  }
  const result: { index: number; segment: string }[] = [];
  let start = 0;
  for (const part of segmenter.segment(text)) {
    let end = Math.max(start, part.index + part.segment.length);
    while (end < text.length) {
      let next = end;
      while (next < text.length && /\s/u.test(text[next])) next++;
      if (!closing.has(next)) break;
      end = next + String.fromCodePoint(text.codePointAt(next)!).length;
    }
    if (end > start)
      result.push({ index: start, segment: text.slice(start, end) });
    start = end;
  }
  return result;
}
