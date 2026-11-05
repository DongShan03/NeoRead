import { appendixReferences } from "./readers/appendix-references";
import { figureReferences } from "./readers/figure-references";
import { createContext } from "react";
import { toast } from "sonner";
import type { Link, PhrasingContent, Root, Text } from "mdast";
import { SKIP, visit } from "unist-util-visit";
import type { PDFLocation } from "@neoread/core";

export const ReadingLinkNavigation = createContext<
  ((blockId: string, label: string) => void) | undefined
>(undefined);
export const ReadingLinkResolver = createContext<
  ((blockId: string) => Promise<Record<string, PDFLocation>>) | undefined
>(undefined);

export function openExternalLink(url: string) {
  if (!/^https?:\/\//i.test(url)) return;
  if (window.neoreadDesktop?.openExternal)
    void window.neoreadDesktop
      .openExternal(url)
      .catch((e) => toast.error(e.message));
  else window.open(url, "_blank", "noopener,noreferrer");
}

export function citationHref(blockId: string, label: string) {
  return `#neoread-citation?${new URLSearchParams({ block: blockId, label })}`;
}

const citationPattern = /\[(\d+(?:\s*[,，–-]\s*\d+)*)\]/g;
// Longer spans are more likely years or misreads than citation runs.
const maxExpandedRange = 20;

// Work on parsed Markdown text nodes so code, links and math stay untouched.
export function remarkReadingCitations({
  blockId,
  labels,
}: {
  blockId?: string;
  labels?: readonly string[];
}) {
  const allowed = new Set(labels);
  return (tree: Root) => {
    if (!blockId || !allowed.size) return;
    visit(tree, (node, index, parent) => {
      if (node.type === "link" || node.type === "linkReference") return SKIP;
      if (node.type !== "text" || !parent || index === undefined) return;
      const parts = readingParts(node.value, blockId, allowed);
      if (!parts) return;
      // Text only lives in phrasing parents.
      (parent.children as PhrasingContent[]).splice(index, 1, ...parts);
      return [SKIP, index + parts.length];
    });
  };
}
function readingParts(
  value: string,
  blockId: string,
  allowed: ReadonlySet<string>,
) {
  const references = [
    ...figureReferences(value).map((ref) => ({ ...ref, kind: "figure" })),
    ...appendixReferences(value).map((ref) => ({ ...ref, kind: "appendix" })),
  ].sort((a, b) => a.index - b.index);
  if (!references.length) return citationParts(value, blockId, allowed);
  const parts: PhrasingContent[] = [];
  let start = 0;
  for (const reference of references) {
    const before = value.slice(start, reference.index);
    const label = `${reference.kind}:${reference.label}`;
    parts.push(
      ...(citationParts(before, blockId, allowed) ?? [text(before)]),
      allowed.has(label)
        ? {
            type: "link",
            url: citationHref(blockId, label),
            children: [text(reference.text)],
          }
        : text(reference.text),
    );
    start = reference.index + reference.text.length;
  }
  const after = value.slice(start);
  parts.push(...(citationParts(after, blockId, allowed) ?? [text(after)]));
  return parts;
}
function citationParts(
  value: string,
  blockId: string,
  allowed: ReadonlySet<string>,
) {
  const parts: PhrasingContent[] = [];
  let start = 0;
  for (const match of value.matchAll(citationPattern)) {
    parts.push(
      text(value.slice(start, match.index) + "["),
      ...citationList(match[1], blockId, allowed),
      text("]"),
    );
    start = match.index + match[0].length;
  }
  if (!start) return;
  parts.push(text(value.slice(start)));
  return parts.filter((part) => part.type !== "text" || part.value);
}
/** "3–5, 8" links 3, 4, 5 and 8; separators between items stay as written. */
function citationList(
  list: string,
  blockId: string,
  allowed: ReadonlySet<string>,
): PhrasingContent[] {
  const link = (label: string): Link | Text =>
    allowed.has(label)
      ? {
          type: "link",
          url: citationHref(blockId, label),
          children: [text(label)],
        }
      : text(label);
  const comma = list.includes("，") ? "，" : ", ";
  return list.split(/(\s*[,，]\s*)/).flatMap((item, i) => {
    if (i % 2) return [text(item)];
    const range = /^(\d+)\s*[–-]\s*(\d+)$/.exec(item);
    const from = Number(range?.[1]),
      to = Number(range?.[2]);
    if (range && to > from && to - from <= maxExpandedRange)
      return Array.from({ length: to - from + 1 }, (_, k) =>
        String(from + k),
      ).flatMap((label, k) => (k ? [text(comma), link(label)] : [link(label)]));
    return item
      .split(/(\d+)/)
      .filter(Boolean)
      .map((token) => (/^\d+$/.test(token) ? link(token) : text(token)));
  });
}
const text = (value: string): Text => ({ type: "text", value });
