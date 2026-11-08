import { sentenceTextRanges } from "./pdf-sentence-matching";
import { lineBoxes, type PDFRect } from "./pdf-line-boxes";
import type { Annotation, PDFLocation, TOCItem } from "@neoread/core";
import type {
  DocumentInitParameters,
  PDFDocumentLoadingTask,
  PDFPageProxy,
  PDFDocumentProxy,
} from "pdfjs-dist/types/src/display/api";

// Writes NeoRead's PDF annotations into a copy of the original as standard PDF
// annotations through PDF.js's incremental save, followed by an optional
// full rewrite for navigation. The source file is untouched. Highlights, notes and questions
// become Highlight annotations; notes, question text and AI answers become
// their comments. Underlines become a thin strip under the text. Bookmarks are exported separately as navigation entries when requested.

/** A quadrilateral in PDF user space: top-left, top-right, bottom-left, bottom-right. */
type Quad = [number, number, number, number, number, number, number, number];

export interface AnnotatedPDF {
  bytes: Uint8Array;
  exported: number;
  navigationExported?: number;
  /** Annotations that could not be placed, including translation-only marks. */
  skipped: number;
}

const editorPrefix = "pdfjs_internal_editor_";
const highlightType = 9;
/** Points; the reader draws a 2 px underline at 100 % zoom. */
const underlineWidth = 1.5;
const defaultColor = "#facc15";

export function hexColor(value: string): [number, number, number] {
  const match =
    /^#?([0-9a-f]{6})$/i.exec(value.trim()) ??
    /^#?([0-9a-f]{6})$/i.exec(defaultColor)!;
  const n = parseInt(match[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

/** The comment written into the PDF, or "" for none. */
export function commentText(annotation: Annotation, answer?: string) {
  const note = annotation.note.trim();
  if (annotation.kind !== "question") return note;
  const parts = [`问题：${note}`];
  if (answer?.trim()) parts.push(`回答：${answer.trim()}`);
  return parts.join("\n\n");
}

const compact = (text: string) => text.replace(/\s+/g, "").toLowerCase();

// NeoRead stores rects relative to the displayed page, which includes /Rotate
// and the crop box; the viewport maps them back to PDF user space.
function displayQuads(page: PDFPageProxy, rects: PDFRect[]): Quad[] {
  const viewport = page.getViewport({ scale: 1 });
  const point = (x: number, y: number) =>
    viewport.convertToPdfPoint(x * viewport.width, y * viewport.height) as [
      number,
      number,
    ];
  return lineBoxes(rects, viewport.width / viewport.height).map((r) => [
    ...point(r.x, r.y),
    ...point(r.x + r.width, r.y),
    ...point(r.x, r.y + r.height),
    ...point(r.x + r.width, r.y + r.height),
  ]) as Quad[];
}

function bounds(quads: Quad[]) {
  const xs = quads.flatMap((q) => [q[0], q[2], q[4], q[6]]);
  const ys = quads.flatMap((q) => [q[1], q[3], q[5], q[7]]);
  return [Math.min(...xs), Math.min(...ys), Math.max(...xs), Math.max(...ys)];
}

/** The bottom `thickness` points of a quad, along its own up direction. */
export function bottomStrip(q: Quad, thickness = underlineWidth): Quad {
  const height = Math.hypot(q[0] - q[4], q[1] - q[5]);
  const t = height > 0 ? Math.min(1, thickness / height) : 1;
  const up = (bx: number, by: number, tx: number, ty: number) => [
    bx + (tx - bx) * t,
    by + (ty - by) * t,
  ];
  return [
    ...up(q[4], q[5], q[0], q[1]),
    ...up(q[6], q[7], q[2], q[3]),
    q[4],
    q[5],
    q[6],
    q[7],
  ] as Quad;
}

function editorValue(
  annotation: Annotation,
  pageIndex: number,
  quads: Quad[],
  comment: string,
) {
  // An underline is an opaque strip along the bottom of each line, matching
  // the reader's 2 px underline. It is a highlight rather than an Ink line
  // because macOS Preview draws Ink annotations in white.
  const underline = annotation.kind === "underline";
  const marks = underline ? quads.map((q) => bottomStrip(q)) : quads;
  const rect = bounds(marks);
  return {
    annotationType: highlightType,
    pageIndex,
    rect,
    rotation: 0,
    color: hexColor(annotation.color || defaultColor),
    opacity: underline ? 1 : 0.5,
    quadPoints: marks.flat(),
    // Outline polygons go around each quad: TL, TR, BR, BL.
    outlines: marks.map((q) => [
      q[0],
      q[1],
      q[2],
      q[3],
      q[6],
      q[7],
      q[4],
      q[5],
    ]),
    user: "NeoRead",
    date: new Date(annotation.createdAt || Date.now()),
    popup: comment
      ? {
          contents: comment,
          deleted: false,
          rect: [rect[2], rect[3] - 100, rect[2] + 180, rect[3]],
        }
      : undefined,
  };
}

async function sentenceQuads(
  page: PDFPageProxy,
  block: PDFRect,
  sources: string[],
): Promise<Quad[]> {
  const viewport = page.getViewport({ scale: 1 });
  const content = await page.getTextContent();
  type Glyphs = {
    start: number;
    text: string;
    transform: number[];
    width: number;
    height: number;
  };
  const items: Glyphs[] = [];
  let text = "";
  for (const item of content.items) {
    if (!("str" in item) || !item.str.trim()) continue;
    const [a, b, , , e, f] = item.transform;
    // Only horizontal text is placed; rotated runs keep their block untouched.
    if (Math.abs(b) > 1e-3 || a <= 0) continue;
    const [cx, cy] = viewport.convertToViewportPoint(
      e + item.width / 2,
      f + item.height / 2,
    );
    const x = cx / viewport.width,
      y = cy / viewport.height;
    if (
      x < block.x ||
      x > block.x + block.width ||
      y < block.y ||
      y > block.y + block.height
    )
      continue;
    const value = compact(item.str);
    items.push({
      start: text.length,
      text: value,
      transform: item.transform,
      width: item.width,
      height: item.height,
    });
    text += value;
  }
  const needles = sources.map(compact).filter(Boolean);
  const ranges = sentenceTextRanges(text, text, needles);
  if (!needles.length || ranges.length < needles.length) return [];
  const quads: Quad[] = [];
  for (const range of ranges)
    for (const item of items) {
      const from = Math.max(range.start, item.start) - item.start;
      const to =
        Math.min(range.end, item.start + item.text.length) - item.start;
      if (to <= from) continue;
      const [, , , d, e, f] = item.transform;
      const per = item.width / item.text.length;
      const left = e + from * per,
        right = e + to * per;
      const height = item.height || Math.abs(d);
      const bottom = f - height * 0.2,
        top = f + height * 0.85;
      quads.push([left, top, right, top, left, bottom, right, bottom]);
    }
  return quads;
}

export interface ExportContext {
  /** Chat message text by ID, for answered questions. */
  answers?: Record<string, string>;
  navigation?: (pdf: PDFDocumentProxy) => Promise<TOCItem[]>;
}

export async function annotatedPDF(
  getDocument: (params: DocumentInitParameters) => PDFDocumentLoadingTask,
  source: DocumentInitParameters,
  annotations: Annotation[],
  { answers = {}, navigation }: ExportContext = {},
): Promise<AnnotatedPDF> {
  const task = getDocument(source);
  try {
    const pdf = await task.promise;
    const inRange = (page: number) => page >= 1 && page <= pdf.numPages;
    let exported = 0,
      skipped = 0;
    for (const annotation of annotations) {
      if (annotation.kind === "bookmark" || annotation.location.type !== "pdf")
        continue;
      const location = annotation.location as PDFLocation;
      // Quads per page. A mark is written whole or not at all, so a partly
      // placed selection is never exported silently.
      const pages = new Map<number, Quad[]>();
      // A translated mark has no exact range in the original PDF.
      if (location.translation) {
        skipped++;
        continue;
      }
      if (location.sourceAnchors?.length) {
        for (const anchor of location.sourceAnchors) {
          if (!inRange(anchor.page)) {
            pages.clear();
            break;
          }
          const page = await pdf.getPage(anchor.page);
          const quads = await sentenceQuads(page, anchor.bounds, [
            anchor.source,
          ]);
          if (!quads.length) {
            pages.clear();
            break;
          }
          pages.set(anchor.page, [...(pages.get(anchor.page) ?? []), ...quads]);
        }
      } else if (inRange(location.page) && location.rects?.length) {
        const page = await pdf.getPage(location.page);
        pages.set(location.page, displayQuads(page, location.rects));
      }
      if (!pages.size) {
        skipped++;
        continue;
      }
      // The comment goes on the first page the mark touches.
      let comment = commentText(annotation, answers[annotation.answerId ?? ""]);
      for (const [page, quads] of [...pages].sort((a, b) => a[0] - b[0])) {
        pdf.annotationStorage.setValue(
          `${editorPrefix}${annotation.id}-${page}`,
          editorValue(annotation, page - 1, quads, comment),
        );
        comment = "";
      }
      exported++;
    }
    let bytes = exported ? await pdf.saveDocument() : await pdf.getData();
    if (navigation) {
      const items = await navigation(pdf);
      const { writePDFOutline } = await import("./pdf-outline-export");
      bytes = await writePDFOutline(bytes, items, pdf);
      const count = (nodes: TOCItem[]): number =>
        nodes.reduce((n, item) => n + 1 + count(item.children), 0);
      return { bytes, exported, skipped, navigationExported: count(items) };
    }
    return { bytes, exported, skipped };
  } finally {
    await task.destroy();
  }
}
