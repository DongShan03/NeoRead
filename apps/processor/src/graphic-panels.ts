import type { PDFPageProxy } from "pdfjs-dist";
import type { Box } from "./layout";

/** Native filled panels provide figure boundaries even when OCR calls their text prose. */
export async function graphicPanels(
  page: PDFPageProxy,
  scale = 1,
): Promise<Box[]> {
  const { OPS, Util } = await import("pdfjs-dist/legacy/build/pdf.mjs");
  const viewport = page.getViewport({ scale });
  const operations = await page.getOperatorList();
  type State = { matrix: number[]; fill: string };
  let state: State = { matrix: [1, 0, 0, 1, 0, 0], fill: "#000000" };
  const stack: State[] = [],
    boxes: Box[] = [];
  const fills = new Set([
    OPS.fill,
    OPS.eoFill,
    OPS.fillStroke,
    OPS.eoFillStroke,
    OPS.closeFillStroke,
    OPS.closeEOFillStroke,
  ]);
  for (let i = 0; i < operations.fnArray.length; i++) {
    const fn = operations.fnArray[i],
      args = operations.argsArray[i];
    if (fn === OPS.save || fn === OPS.paintFormXObjectBegin) {
      stack.push({ matrix: [...state.matrix], fill: state.fill });
      if (fn === OPS.paintFormXObjectBegin && args?.[0])
        state.matrix = Util.transform(state.matrix, args[0]);
    } else if (fn === OPS.restore || fn === OPS.paintFormXObjectEnd) {
      state = stack.pop() ?? { matrix: [1, 0, 0, 1, 0, 0], fill: "#000000" };
    } else if (fn === OPS.transform)
      state.matrix = Util.transform(state.matrix, args);
    else if (fn === OPS.setFillRGBColor)
      state.fill = typeof args[0] === "string" ? args[0] : "#000000";
    else if (
      fn === OPS.constructPath &&
      fills.has(args?.[0]) &&
      args?.[2] &&
      !/^#f[ef]f[ef]f[ef]$/i.test(state.fill)
    ) {
      const path = args[1]?.[0];
      if (!path || path[path.length - 1] !== 4) continue;
      const b = args[2],
        transform = Util.transform(viewport.transform, state.matrix);
      const points = [
        [b[0], b[1]],
        [b[2], b[1]],
        [b[0], b[3]],
        [b[2], b[3]],
      ].map((p) => {
        Util.applyTransform(p, transform);
        return p;
      });
      const x = Math.min(...points.map((p) => p[0])) / viewport.width,
        y = Math.min(...points.map((p) => p[1])) / viewport.height;
      const width = Math.max(...points.map((p) => p[0])) / viewport.width - x,
        height = Math.max(...points.map((p) => p[1])) / viewport.height - y;
      if (
        width >= 0.1 &&
        height >= 0.018 &&
        width * height >= 0.003 &&
        width * height < 0.85 &&
        x >= -0.01 &&
        y >= -0.01 &&
        x + width <= 1.01 &&
        y + height <= 1.01
      )
        boxes.push({ x, y, width, height });
    }
  }
  return boxes;
}
