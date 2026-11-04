import {
  pdfFontAscent,
  pdfFontHeight,
  pdfMathOperator,
} from "../../../packages/neoread-core/src/pdf-text";
import type { PDFPageProxy } from "pdfjs-dist";
import type { TextSpan } from "./layout";

export async function extractTextSpans(
  page: PDFPageProxy,
  scale = 1,
): Promise<TextSpan[]> {
  const { Util } = await import("pdfjs-dist/legacy/build/pdf.mjs");
  const viewport = page.getViewport({ scale });
  const width = Math.ceil(viewport.width),
    height = Math.ceil(viewport.height);
  const content = await page.getTextContent();
  // Loading the operator list makes the embedded font names available. Generic
  // text styles alone cannot distinguish CMSY calligraphic letters from plain T.
  await page.getOperatorList();
  const fonts = new Map<string, string>(
    Object.keys(content.styles).map((key) => [
      key,
      page.commonObjs.get(key).name,
    ]),
  );
  return content.items.flatMap((item) => {
    if (!("str" in item) || !item.str.trim()) return [];
    const transform = Util.transform(viewport.transform, item.transform);
    const vertical = Math.abs(transform[1]) > Math.abs(transform[0]) * 2;
    if (
      vertical &&
      (transform[4] / width < 0.1 || transform[4] / width > 0.9) &&
      /^arxiv\s*:\s*\d{4}\.\d+/i.test(item.str.trim())
    )
      return [];
    const h = Math.hypot(transform[2], transform[3]);
    const ascent = pdfFontAscent(content.styles[item.fontName]);
    const font = (fonts.get(item.fontName) ?? "").replace(/^[A-Z]{6}\+/, "");
    const operator = pdfMathOperator(item.str, font);
    return [
      {
        text: mathFontText(item.str, fonts.get(item.fontName) ?? ""),
        // CMEX large-operator glyph origins sit 0.75 em above the prose baseline.
        baseline: (transform[5] + (operator ? h * 0.75 : 0)) / height,
        fontSize: h / height,
        math:
          /^(?:CMMI|CMSY|MSBM|CMEX)\d+$/i.test(font) ||
          (/^CMBX\d+$/i.test(font) && item.str.length === 1) ||
          (/^CMR\d+$/i.test(font) && /^[\d˜ˆ¯˙¨=+()\[\],.\-]+$/.test(item.str)),
        bounds: {
          x: transform[4] / width,
          y: (transform[5] - h * ascent) / height,
          width: Math.abs(item.width * scale) / width,
          height: (h * pdfFontHeight(content.styles[item.fontName])) / height,
        },
      },
    ];
  });
}

const script = Array.from("𝒜ℬ𝒞𝒟ℰℱ𝒢ℋℐ𝒥𝒦ℒℳ𝒩𝒪𝒫𝒬ℛ𝒮𝒯𝒰𝒱𝒲𝒳𝒴𝒵");
const doubleStruck = Array.from("𝔸𝔹ℂ𝔻𝔼𝔽𝔾ℍ𝕀𝕁𝕂𝕃𝕄ℕ𝕆ℙℚℝ𝕊𝕋𝕌𝕍𝕎𝕏𝕐ℤ");

// These TeX symbol fonts encode mathematical alphabets as ASCII capitals.
// Only restore the known font encodings; ordinary prose and math italic stay intact.
export function mathFontText(text: string, fontName: string) {
  const operator = pdfMathOperator(text, fontName);
  if (operator) return operator;
  const name = fontName.replace(/^[A-Z]{6}\+/, "");
  if (/^CMEX\d+$/i.test(name) && /^[b-g]$/.test(text))
    return text <= "d" ? "ˆ" : "˜";
  if (/^CMBX\d+$/i.test(name) && /^[A-Za-z]$/.test(text))
    return String.fromCodePoint(
      text.charCodeAt(0) + (text <= "Z" ? 0x1d400 - 65 : 0x1d41a - 97),
    );
  const alphabet = /^CMSY\d+$/i.test(name)
    ? script
    : /^MSBM\d+$/i.test(name)
      ? doubleStruck
      : undefined;
  return alphabet
    ? text.replace(/[A-Z]/g, (letter) => alphabet[letter.charCodeAt(0) - 65])
    : text;
}
