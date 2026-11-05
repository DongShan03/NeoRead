const pattern =
  /(?:\b(?:appendix|appendices|app\.)\s*|附[录錄]\s*)([A-ZＡ-Ｚ](?:\s*[.．]\s*[0-9０-９]+)*)(?![\wＡ-Ｚ０-９]|\s*[.．]\s*[0-9０-９])/gi;
const normalize = (label: string) =>
  label.normalize("NFKC").replace(/\s+/g, "").toUpperCase();

export function appendixReferences(text: string) {
  return Array.from(text.matchAll(pattern), (match) => ({
    index: match.index,
    text: match[0],
    label: normalize(match[1]),
  }));
}
