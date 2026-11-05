import type { PDFBlock } from "@neoread/core";

const pattern =
  /(?:\b(?:appendix|appendices|app\.)\s*|附[录錄]\s*)([A-ZＡ-Ｚ](?:\s*[.．]\s*[0-9０-９]+)*)(?![\wＡ-Ｚ０-９]|\s*[.．]\s*[0-9０-９])/gi;
const headingNumber =
  /^([A-ZＡ-Ｚ](?:\s*[.．]\s*[0-9０-９]+)*)(?!\s*[.．]\s*[0-9０-９])(?=\s|[.．:：、]|$|[\u3400-\u9fff])/i;
const normalize = (label: string) =>
  label.normalize("NFKC").replace(/\s+/g, "").toUpperCase();

export function appendixReferences(text: string) {
  return Array.from(text.matchAll(pattern), (match) => ({
    index: match.index,
    text: match[0],
    label: normalize(match[1]),
  }));
}

/** A unique heading establishes the destination, never a mention in body text. */
export function appendixTarget(blocks: PDFBlock[], label: string) {
  const key = normalize(label);
  const matches = blocks.filter((block) => {
    if (!["paragraph_title", "doc_title"].includes(block.label)) return false;
    const title = block.text.trim().replace(/^#{1,6}\s+/, "");
    const explicit = appendixReferences(title)[0];
    const number =
      explicit?.index === 0 ? explicit.label : headingNumber.exec(title)?.[1];
    return number !== undefined && normalize(number) === key;
  });
  return matches.length === 1 ? matches[0] : undefined;
}
