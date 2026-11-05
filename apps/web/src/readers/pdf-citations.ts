import { figureReferences } from "./figure-references";
import { appendixReferences } from "./appendix-references";
import type { PDFDocumentProxy } from "pdfjs-dist";
import type { PDFBlock, PDFLocation } from "@neoread/core";
import { destinationPoint } from "./pdf-link-preview";

// TextContent can merge differently colored links into an entire line. Use the
// PDF's glyph advances, distributing the line's remaining width over its spaces.
function characterWidths(
  item: { str: string; width: number; transform: number[] },
  font?: Map<string, number>,
) {
  const scale = Math.hypot(item.transform[0], item.transform[1]) / 1000;
  const ligatures = [...(font?.keys() ?? [])]
    .filter((key) => key.length > 1)
    .sort((a, b) => b.length - a.length);
  const chars: { text: string; width: number }[] = [];
  for (let i = 0; i < item.str.length;) {
    const text =
      ligatures.find((key) => item.str.startsWith(key, i)) ??
      String.fromCodePoint(item.str.codePointAt(i)!);
    chars.push({
      text,
      width: /\s/.test(text) ? 0 : (font?.get(text) ?? 500) * scale,
    });
    i += text.length;
  }
  const spaces = chars.filter((char) => /\s/.test(char.text)).length;
  const sum = chars.reduce((total, char) => total + char.width, 0);
  for (const char of chars) {
    if (/\s/.test(char.text))
      char.width = Math.max(0, (item.width - sum) / (spaces || 1));
    else if (!spaces || !font) char.width *= item.width / (sum || 1);
  }
  return chars;
}

export interface PDFSourceLink {
  bounds: PDFBlock["bounds"];
  labels: string[];
  target: PDFLocation;
}

/** Read the actual PDF annotations; text only identifies the linked label. */
export async function readPDFSourceLinks(
  pdf: PDFDocumentProxy,
  pageNumber: number,
  operators?: Record<string, number>,
): Promise<PDFSourceLink[]> {
  const page = await pdf.getPage(pageNumber);
  const view = page.getViewport({ scale: 1 });
  const [annotations, content] = await Promise.all([
    page.getAnnotations(),
    page.getTextContent(),
  ]);
  if (!annotations.some((a) => a.subtype === "Link" && a.dest && a.rect))
    return [];
  const fonts = new Map<string, Map<string, number>>();
  if (operators) {
    const list = await page.getOperatorList();
    let font = "";
    const stack: string[] = [];
    for (let i = 0; i < list.fnArray.length; i++) {
      const op = list.fnArray[i],
        args = list.argsArray[i];
      if (op === operators.save) stack.push(font);
      else if (op === operators.restore) font = stack.pop() ?? font;
      else if (op === operators.setFont) font = args[0];
      else if (op === operators.showText) {
        let widths = fonts.get(font);
        if (!widths) fonts.set(font, (widths = new Map()));
        for (const glyph of args[0] as (
          number | { unicode: string; width: number }
        )[])
          if (typeof glyph !== "number" && glyph.unicode)
            widths.set(glyph.unicode, glyph.width);
      }
    }
  }
  let text = "";
  const glyphs: {
    start: number;
    end: number;
    text: string;
    center: number;
    y: number;
  }[] = [];
  for (const item of content.items) {
    if (!("str" in item)) continue;
    const [x, y] = view.convertToViewportPoint(
      item.transform[4],
      item.transform[5],
    );
    let cursor = x,
      offset = text.length;
    for (const char of characterWidths(item, fonts.get(item.fontName))) {
      glyphs.push({
        start: offset,
        end: offset + char.text.length,
        text: char.text,
        center: cursor + char.width / 2,
        y,
      });
      offset += char.text.length;
      cursor += char.width;
    }
    text += item.str + " ";
  }
  const references = [
    ...figureReferences(text).map((r) => ({
      ...r,
      label: "figure:" + r.label,
    })),
    ...appendixReferences(text).map((r) => ({
      ...r,
      label: "appendix:" + r.label,
    })),
  ];
  const result: PDFSourceLink[] = [];
  for (const annotation of annotations) {
    if (annotation.subtype !== "Link" || !annotation.dest || !annotation.rect)
      continue;
    const r = [
      ...view.convertToViewportPoint(annotation.rect[0], annotation.rect[1]),
      ...view.convertToViewportPoint(annotation.rect[2], annotation.rect[3]),
    ];
    const left = Math.min(r[0], r[2]),
      right = Math.max(r[0], r[2]),
      top = Math.min(r[1], r[3]),
      bottom = Math.max(r[1], r[3]);
    const linked = glyphs.filter(
      (g) =>
        g.y <= bottom + 1 &&
        g.y >= top - 1 &&
        g.center >= left - 1 &&
        g.center <= right + 1,
    );
    if (!linked.length) continue;
    const labels = new Set(
      references
        .filter((ref) =>
          linked.some(
            (g) => g.start < ref.index + ref.text.length && g.end > ref.index,
          ),
        )
        .map((ref) => ref.label),
    );
    if (!labels.size) {
      const visible = linked
        .map((g) => g.text)
        .join("")
        .replace(/[\s\[\]［］]/g, "");
      if (/^\d+(?:[,，–-]\d+)*$/.test(visible))
        for (const number of visible.match(/\d+/g) ?? []) labels.add(number);
    }
    if (!labels.size) continue;
    const dest =
      typeof annotation.dest === "string"
        ? await pdf.getDestination(annotation.dest)
        : annotation.dest;
    if (!Array.isArray(dest) || !dest.length) continue;
    const index =
      typeof dest[0] === "number" ? dest[0] : await pdf.getPageIndex(dest[0]);
    const targetView = (await pdf.getPage(index + 1)).getViewport({ scale: 1 });
    const point = destinationPoint(dest);
    const [x, y] = targetView.convertToViewportPoint(
      point.left ?? 0,
      point.top ?? targetView.viewBox[3],
    );
    result.push({
      bounds: {
        x: left / view.width,
        y: top / view.height,
        width: (right - left) / view.width,
        height: (bottom - top) / view.height,
      },
      labels: [...labels],
      target: {
        type: "pdf",
        page: index + 1,
        x: Math.max(0, x / targetView.width),
        y: Math.max(0, y / targetView.height),
      },
    });
  }
  return result;
}

export function blockPDFSourceLinks(block: PDFBlock, links: PDFSourceLink[]) {
  const result: Record<string, PDFLocation> = {};
  const ambiguous = new Set<string>();
  const b = block.bounds;
  for (const link of links) {
    const r = link.bounds;
    if (
      r.x + r.width <= b.x ||
      r.x >= b.x + b.width ||
      r.y + r.height <= b.y ||
      r.y >= b.y + b.height
    )
      continue;
    for (const label of link.labels) {
      if (ambiguous.has(label)) continue;
      const previous = result[label];
      if (
        previous &&
        (previous.page !== link.target.page ||
          previous.x !== link.target.x ||
          previous.y !== link.target.y)
      ) {
        delete result[label];
        ambiguous.add(label);
      } else result[label] = link.target;
    }
  }
  return result;
}

/** Resolve only a destination embedded in the original PDF, never a guessed caption. */
export async function resolvePDFCitation(
  pdf: PDFDocumentProxy,
  block: PDFBlock,
  label: string,
  operators?: Record<string, number>,
) {
  return blockPDFSourceLinks(
    block,
    await readPDFSourceLinks(pdf, block.page, operators),
  )[label];
}
