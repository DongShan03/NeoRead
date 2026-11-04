import { pdfAssetBounds } from "../../../packages/neoread-core/src/pdf-asset-bounds";
import { orderPageBlocks } from "./reading-order";
import { joinInlineText } from "./inline-math";
export const labels = [
  "abstract",
  "algorithm",
  "aside_text",
  "chart",
  "content",
  "display_formula",
  "doc_title",
  "figure_title",
  "footer",
  "footer_image",
  "footnote",
  "formula_number",
  "header",
  "header_image",
  "image",
  "inline_formula",
  "number",
  "paragraph_title",
  "reference",
  "reference_content",
  "seal",
  "table",
  "text",
  "vertical_text",
  "vision_footnote",
] as const;
export interface Box {
  x: number;
  y: number;
  width: number;
  height: number;
}
export interface Region {
  label: string;
  confidence: number;
  order: number;
  bounds: Box;
}
export interface TextSpan {
  text: string;
  bounds: Box;
  baseline?: number;
  fontSize?: number;
  math?: boolean;
}
export const isAsset = (label: string) =>
  [
    "algorithm",
    "chart",
    "image",
    "table",
    "display_formula",
    "inline_formula",
  ].includes(label);
const isFigure = (label: string) => label === "chart" || label === "image";
export function intersection(a: Box, b: Box) {
  return (
    Math.max(0, Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x)) *
    Math.max(0, Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y))
  );
}
// Keep moderate-confidence graphics as caption-group candidates. Unconfirmed
// candidates are removed by readingRegions; other classes retain the 0.5 cutoff.
export function decodeRegions(
  data: ArrayLike<number>,
  width: number,
  height: number,
): Region[] {
  const boxes: Region[] = [];
  for (let i = 0; i + 6 < data.length; i += 7) {
    const [cls, confidence, x1, y1, x2, y2, order] = Array.from(
      { length: 7 },
      (_, n) => Number(data[i + n]),
    );
    if (
      ![cls, confidence, x1, y1, x2, y2, order].every(Number.isFinite) ||
      confidence < (isFigure(labels[cls]) ? 0.3 : 0.5) ||
      !labels[cls]
    )
      continue;
    const x = Math.max(0, Math.min(1, x1 / width));
    const y = Math.max(0, Math.min(1, y1 / height));
    const bounds = {
      x,
      y,
      width: Math.max(0, Math.min(1, x2 / width) - x),
      height: Math.max(0, Math.min(1, y2 / height) - y),
    };
    if (bounds.width <= 0 || bounds.height <= 0) continue;
    if (labels[cls] === "image" && bounds.width * bounds.height > 0.9) continue;
    boxes.push({ label: labels[cls], confidence, order, bounds });
  }
  const kept: Region[] = [];
  for (const b of boxes.sort((a, b) => b.confidence - a.confidence)) {
    if (
      kept.some((a) => {
        const shared = intersection(a.bounds, b.bounds);
        const union =
          a.bounds.width * a.bounds.height +
          b.bounds.width * b.bounds.height -
          shared;
        return shared / union > (a.label === b.label ? 0.6 : 0.98);
      })
    )
      continue;
    kept.push(b);
  }
  return kept.sort((a, b) => a.order - b.order);
}
// Each span belongs to at most one region. Unmatched spans are retained separately.
export function assignText(regions: Region[], spans: TextSpan[]) {
  const texts: TextSpan[][] = regions.map(() => []);
  const unmatched: TextSpan[] = [];
  for (const span of spans) {
    let best = -1,
      score = 0.2;
    regions.forEach((region, i) => {
      const overlap =
        intersection(region.bounds, span.bounds) /
        (span.bounds.width * span.bounds.height || 1);
      if (overlap > score) {
        score = overlap;
        best = i;
      }
    });
    if (best < 0) unmatched.push(span);
    else texts[best].push(span);
  }
  return {
    texts: texts.map((t, i) =>
      preformatted.has(regions[i].label) ? joinLines(t) : joinSpans(t),
    ),
    unmatched: joinSpans(unmatched),
  };
}
// Algorithms keep their lines and indentation; everything else reads as prose.
const preformatted = new Set(["algorithm"]);
const sameLine = (a: TextSpan, b: TextSpan) =>
  Math.abs(a.bounds.y - b.bounds.y) <
  Math.min(a.bounds.height, b.bounds.height) * 0.45;
// PDF content streams can list columns out of order. Sort only inside one detected region.
const readingSort = (spans: TextSpan[]) =>
  [...spans].sort((a, b) =>
    sameLine(a, b) ? a.bounds.x - b.bounds.x : a.bounds.y - b.bounds.y,
  );
function joinSpans(spans: TextSpan[]) {
  if (
    spans.length &&
    spans.every((s) => s.baseline !== undefined && s.fontSize !== undefined)
  )
    return joinInlineText(spans);
  return readingSort(spans)
    .map((s) => s.text)
    .join(" ")
    .replace(/\s+/g, " ")
    .trim();
}
function joinLines(spans: TextSpan[]) {
  if (!spans.length) return "";
  const lines: TextSpan[][] = [];
  for (const span of readingSort(spans)) {
    const line = lines.at(-1);
    if (line && sameLine(line[0], span)) line.push(span);
    else lines.push([span]);
  }
  // Horizontal gaps become spaces at the region's median character width.
  const widths = spans
    .map((s) => s.bounds.width / Math.max(1, s.text.trim().length))
    .sort((a, b) => a - b);
  const char = widths[Math.floor(widths.length / 2)] || 1;
  const spaces = (gap: number, least: number) =>
    " ".repeat(
      gap <= char * 0.15
        ? 0
        : Math.min(40, Math.max(least, Math.round(gap / char))),
    );
  const left = Math.min(...spans.map((s) => s.bounds.x));
  return lines
    .map((line) => {
      let text = spaces(line[0].bounds.x - left, 0),
        end = line[0].bounds.x;
      line.forEach((span, i) => {
        if (i) text += spaces(span.bounds.x - end, 1);
        text += span.text.replace(/\s+/g, " ").trim();
        end = Math.max(end, span.bounds.x + span.bounds.width);
      });
      return text.trimEnd();
    })
    .join("\n");
}

export interface ReadingRegion extends Region {
  text: string;
  members?: number[];
  caption?: string;
}
const mainCaption = /^(?:figure|fig\.?|table)\s*\d+[\s:.]/i;
function union(boxes: Box[]): Box {
  const x = Math.min(...boxes.map((b) => b.x)),
    y = Math.min(...boxes.map((b) => b.y));
  return {
    x,
    y,
    width: Math.max(...boxes.map((b) => b.x + b.width)) - x,
    height: Math.max(...boxes.map((b) => b.y + b.height)) - y,
  };
}
// Inline symbols remain in their paragraph. Group panels only when a nearby
// numbered caption provides evidence that they belong to one figure.
export function readingRegions(
  raw: Region[],
  spans: TextSpan[],
  panels: Box[] = [],
): ReadingRegion[] {
  spans = readingTextSpans(raw, spans);
  const regions = raw.filter((r) => r.label !== "inline_formula");
  const assigned = assignText(regions, spans);
  const fitted = pdfAssetBounds(regions, spans);
  let entries: ReadingRegion[] = regions.map((r, i) => ({
    ...r,
    text: assigned.texts[i],
    bounds: fitted[i],
  }));
  const captions = entries.filter(
    (r) => r.label === "figure_title" && mainCaption.test(r.text),
  );
  // Expand from a caption through connected native filled panels. Require an
  // independently detected image inside the group, so ordinary boxed prose
  // cannot become a figure merely because its background is colored.
  for (const caption of captions.filter((c) => /^fig/i.test(c.text))) {
    const cb = caption.bounds;
    const horizontal = (a: Box, b: Box) =>
      Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x);
    const members = panels.filter(
      (b) =>
        cb.y - b.y - b.height >= -0.01 &&
        cb.y - b.y - b.height <= 0.045 &&
        horizontal(b, cb) >= Math.min(b.width, cb.width) * 0.8,
    );
    if (!members.length) continue;
    for (;;) {
      const before = members.length;
      for (const b of panels) {
        if (members.includes(b) || b.y + b.height > cb.y + 0.01) continue;
        if (
          members.some(
            (p) =>
              horizontal(b, p) >= Math.min(b.width, p.width) * 0.8 &&
              Math.max(0, p.y - b.y - b.height, b.y - p.y - p.height) <= 0.03 &&
              !entries.some(
                (r) =>
                  !mainCaption.test(r.text) &&
                  r.label === "text" &&
                  r.bounds.y > Math.min(b.y + b.height, p.y + p.height) &&
                  r.bounds.y + r.bounds.height < Math.max(b.y, p.y),
              ),
          )
        )
          members.push(b);
      }
      if (members.length === before) break;
    }
    const assets = entries.filter(
      (r) =>
        isFigure(r.label) &&
        members.some(
          (b) =>
            intersection(r.bounds, b) / (r.bounds.width * r.bounds.height) >
            0.5,
        ),
    );
    if (!assets.length) continue;
    const bounds = union(members);
    const consumed = entries.filter(
      (r) =>
        r !== caption &&
        !mainCaption.test(r.text) &&
        members.some(
          (b) =>
            intersection(r.bounds, b) / (r.bounds.width * r.bounds.height) >
            0.85,
        ),
    );
    const image: ReadingRegion = {
      ...assets[0],
      label: "image",
      bounds,
      text: assignText([{ ...assets[0], bounds }], spans).texts[0],
      order: Math.min(...consumed.map((r) => r.order)),
      members: consumed.map((r) => r.order),
    };
    entries = [...entries.filter((r) => !consumed.includes(r)), image];
  }
  const groups = new Map<ReadingRegion, ReadingRegion[]>();
  const remaining = new Set(
    entries.filter((r) => isFigure(r.label) || r.label === "table"),
  );
  // Start at the caption, then extend through adjacent rows of panels. A fixed
  // distance to the caption alone drops the upper rows of tall figures.
  while (remaining.size) {
    const before = remaining.size;
    for (const asset of remaining) {
      const b = asset.bounds;
      const eligible = captions
        .filter((c) => {
          if ((asset.label === "table") !== /^table/i.test(c.text))
            return false;
          const cb = c.bounds;
          const gap =
            asset.label === "table"
              ? Math.min(
                  Math.abs(cb.y - b.y - b.height),
                  Math.abs(b.y - cb.y - cb.height),
                )
              : cb.y - b.y - b.height;
          const center = b.x + b.width / 2;
          const connected =
            isFigure(asset.label) &&
            groups.get(c)?.some((panel) => {
              const p = panel.bounds;
              return (
                Math.max(0, p.y - b.y - b.height, b.y - p.y - p.height) <= 0.03
              );
            });
          if (
            gap < -0.01 ||
            (gap > 0.12 && !connected) ||
            center < cb.x - Math.min(0.12, cb.width / 4) ||
            center > cb.x + cb.width + Math.min(0.12, cb.width / 4)
          )
            return false;
          // Do not combine across intervening prose or another numbered caption.
          return !entries.some(
            (r) =>
              r !== asset &&
              r !== c &&
              (r.label === "text" ||
                r.label === "paragraph_title" ||
                mainCaption.test(r.text)) &&
              r.bounds.y > b.y + b.height &&
              r.bounds.y + r.bounds.height < cb.y &&
              center >= r.bounds.x &&
              center <= r.bounds.x + r.bounds.width,
          );
        })
        .sort(
          (a, z) =>
            Math.abs(a.bounds.y - b.y - b.height) -
            Math.abs(z.bounds.y - b.y - b.height),
        );
      const caption = eligible[0];
      if (caption) {
        groups.set(caption, [...(groups.get(caption) ?? []), asset]);
        remaining.delete(asset);
      }
    }
    if (remaining.size === before) break;
  }
  const consumed = new Set<ReadingRegion>();
  const merged: ReadingRegion[] = [];
  for (const [caption, assets] of groups) {
    let bounds = union([caption.bounds, ...assets.map((a) => a.bounds)]);
    const parts = entries.filter(
      (r) =>
        r.label === "figure_title" &&
        !mainCaption.test(r.text) &&
        intersection(r.bounds, bounds) / (r.bounds.width * r.bounds.height) >
          0.8,
    );
    const members = [...assets, ...parts];
    bounds = union(members.map((r) => r.bounds));
    members.forEach((r) => consumed.add(r));
    const text = assignText([{ ...assets[0], bounds }], spans).texts[0];
    merged.push({
      ...assets[0],
      bounds,
      text,
      order: Math.min(...members.map((r) => r.order)),
      confidence: Math.min(...assets.map((r) => r.confidence)),
      members: members.map((r) => r.order),
    });
  }
  return orderPageBlocks(
    [
      ...entries.filter((r) => r.confidence >= 0.5 && !consumed.has(r)),
      ...merged,
    ].sort((a, b) => a.order - b.order),
  );
}

export function readingTextSpans(
  regions: Region[],
  spans: TextSpan[],
): TextSpan[] {
  const candidates = spans.filter(
    (s) =>
      /^\d{1,4}$/.test(s.text.trim()) &&
      s.bounds.width < 0.035 &&
      s.bounds.y > 0.06 &&
      s.bounds.y + s.bounds.height < 0.95 &&
      (s.bounds.x < 0.09 || s.bounds.x > 0.9) &&
      !regions.some(
        (r) =>
          isAsset(r.label) &&
          intersection(r.bounds, s.bounds) /
            (s.bounds.width * s.bounds.height) >
            0.5,
      ),
  );
  const removed = new Set<TextSpan>();
  for (const span of candidates) {
    const column = candidates
      .filter((s) => Math.abs(s.bounds.x - span.bounds.x) < 0.012)
      .sort((a, b) => a.bounds.y - b.bounds.y);
    if (column.length < 4) continue;
    const step = Number(column[1].text) - Number(column[0].text);
    const sequential =
      [1, 5, 10].includes(step) &&
      column.every(
        (s, i) =>
          i === 0 || Number(s.text) - Number(column[i - 1].text) === step,
      );
    if (sequential) column.forEach((s) => removed.add(s));
  }
  return spans.filter((s) => !removed.has(s));
}
