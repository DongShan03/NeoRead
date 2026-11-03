import type {
  DocumentLocation,
  EPUBReadingBlock,
  PDFBlock,
  ReaderSelection,
  TranslationBlock,
} from "@neoread/core";
import { sentenceAnchor } from "@neoread/core";
import { epubSentenceLocation } from "./epub-links";

/** Keep partial selections local; only explicitly complete sentences share a mark. */
export function prepareSentenceAnnotation(
  value: ReaderSelection,
): ReaderSelection {
  const { sentenceLink: _, ...location } = value.location;
  const mark = location.translation;
  if (!mark) return { ...value, location };
  const parts = mark.ranges ?? [{ ...mark, quote: value.text }];
  const bilingual =
    parts.length > 0 &&
    parts.every((part) => part.wholeSentence && !!part.sourceText);
  return {
    ...value,
    location: {
      ...location,
      translation: {
        ...mark,
        bilingual: bilingual || undefined,
        ranges: parts.map((part) => ({
          ...part,
          bilingual: bilingual || undefined,
        })),
      },
    },
  };
}

/** Sentence anchors work even when the opposite pane is collapsed. */
export function hoveredSentenceSelection(
  host: HTMLElement,
  block: PDFBlock | EPUBReadingBlock,
  translation: TranslationBlock,
  index: number,
  origin: "source" | "translation",
  ranges: Range[],
): ReaderSelection | undefined {
  const sentence = translation.sentences[index];
  const rects = ranges
    .flatMap((range) => Array.from(range.getClientRects()))
    .filter((rect) => rect.width > 0 && rect.height > 0);
  if (!sentence || !rects.length) return;
  const text =
    origin === "source"
      ? sentence.source
      : ranges
          .map((range) => range.toString())
          .join("")
          .trim();
  let location: DocumentLocation;
  if ("location" in block) {
    location = epubSentenceLocation(block, translation.sentences, [index]);
  } else {
    const page = host.querySelector<HTMLElement>(
      `.page[data-page-number="${block.page}"]`,
    );
    const bounds = page?.getBoundingClientRect();
    location = {
      type: "pdf",
      page: block.page,
      quote: sentenceAnchor(sentence),
      rects:
        origin === "source" && bounds?.width && bounds.height
          ? rects.map((rect) => ({
              x: (rect.left - bounds.left) / bounds.width,
              y: (rect.top - bounds.top) / bounds.height,
              width: rect.width / bounds.width,
              height: rect.height / bounds.height,
            }))
          : [block.bounds],
    };
  }
  if (translation.sourceHash && sentence.target) {
    const part = {
      blockId: block.id,
      sourceHash: translation.sourceHash,
      sentenceIndexes: [index],
      start: 0,
      end: 0,
      quote: sentence.target,
      sourceText: sentenceAnchor(sentence),
      targetText: translation.sentences
        .map((sentence) => sentence.target)
        .join(""),
      recovered: true,
      wholeSentence: true,
      bilingual: true,
      ...("location" in block
        ? { location: location as typeof block.location }
        : {}),
    };
    location = { ...location, translation: { ...part, ranges: [part] } };
  }
  return {
    text,
    location,
    anchor: {
      x:
        (Math.min(...rects.map((r) => r.left)) +
          Math.max(...rects.map((r) => r.right))) /
        2,
      top: Math.min(...rects.map((r) => r.top)),
      bottom: Math.max(...rects.map((r) => r.bottom)),
    },
  };
}
