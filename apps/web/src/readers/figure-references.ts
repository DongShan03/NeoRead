const number = String.raw`(?:[Ss]?\d+(?:\.\d+)*|[零〇一二三四五六七八九十百两]+)`;
const pattern = new RegExp(
  String.raw`(?:\bfig(?:ure)?\.?\s*|(?<![意企试])图\s*)(?:\[\s*(${number})\s*\]|［\s*(${number})\s*］|(${number}))(?:\s*\([a-z]\)|[a-z])?(?![\w.]*\d|[a-z])`,
  "gi",
);
function normalize(value: string) {
  if (/^[s\d]/i.test(value))
    return value.toLowerCase().replace(/^0+(?=\d)/, "");
  const digits = "零一二三四五六七八九";
  let total = 0,
    digit = 0;
  for (const char of value.replaceAll("〇", "零").replaceAll("两", "二")) {
    if (char === "十" || char === "百") {
      total += (digit || 1) * (char === "十" ? 10 : 100);
      digit = 0;
    } else digit = digits.indexOf(char);
  }
  return String(total + digit);
}
export function figureReferences(text: string) {
  return Array.from(text.matchAll(pattern), (match) => ({
    index: match.index,
    text: match[0],
    label: normalize(match[1] ?? match[2] ?? match[3]),
  }));
}
