import type { ReadingAnchor } from "@neoread/core";

/** Document blocks are in vertical reading order. Avoid measuring every prior block per scroll. */
export function translationBlockAt(nodes: readonly HTMLElement[], y: number) {
  let low = 0,
    high = nodes.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    if (nodes[middle].getBoundingClientRect().bottom > y) high = middle;
    else low = middle + 1;
  }
  return nodes[low];
}
export function translationReadingAnchor(
  host: HTMLElement,
  nodes: readonly HTMLElement[],
): ReadingAnchor | undefined {
  const viewport = host.getBoundingClientRect();
  if (!viewport.height) return;
  const y = viewport.top + viewport.height * 0.5;
  const node = translationBlockAt(nodes, y) ?? nodes.at(-1);
  if (!node?.dataset.translationBlock) return;
  const rect = node.getBoundingClientRect();
  return {
    blockId: node.dataset.translationBlock,
    fraction: Math.max(0, Math.min(1, (y - rect.top) / (rect.height || 1))),
  };
}
