// Some embedded PDF fonts report zero metrics. Zero is not a usable ascent.
export function pdfFontAscent(style?: { ascent?: number; descent?: number }) {
  const ascent = style?.ascent,
    descent = style?.descent;
  if (typeof ascent === "number" && Number.isFinite(ascent) && ascent > 0)
    return ascent;
  if (
    typeof descent === "number" &&
    Number.isFinite(descent) &&
    descent < 0 &&
    descent > -1
  )
    return 1 + descent;
  return 0.8;
}

// Font ascent/descent may span more than one em (e.g. DejaVu Sans axis labels).
export function pdfFontHeight(style?: { ascent?: number; descent?: number }) {
  const descent = style?.descent;
  return typeof descent === "number" &&
    Number.isFinite(descent) &&
    descent < 0 &&
    descent > -1
    ? Math.max(1, pdfFontAscent(style) - descent)
    : 1;
}

// OMX/CMEX slot 0x46 is the large square-union operator, not a Latin F.
export function pdfMathOperator(text: string, fontName: string) {
  return /^CMEX\d+$/i.test(fontName.replace(/^[A-Z]{6}\+/, "")) && text === "F"
    ? "⨆"
    : undefined;
}
