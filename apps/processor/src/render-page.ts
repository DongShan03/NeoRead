import { createCanvas } from "@napi-rs/canvas";
import type { PDFPageProxy } from "pdfjs-dist";
import type { Box } from "./layout";

/** Shared by layout analysis and visual reading; no layout model is required. */
export async function renderPage(page: PDFPageProxy, bounds?: Box) {
  const original = page.getViewport({ scale: 1 });
  const scale = Math.min(
    2,
    Math.sqrt(4_000_000 / (original.width * original.height)),
    16384 / Math.max(original.width, original.height),
  );
  const viewport = page.getViewport({ scale });
  if (
    bounds &&
    (!Object.values(bounds).every(Number.isFinite) ||
      bounds.x < 0 ||
      bounds.y < 0 ||
      bounds.width <= 0 ||
      bounds.height <= 0 ||
      bounds.x + bounds.width > 1.000001 ||
      bounds.y + bounds.height > 1.000001)
  )
    throw new Error("Invalid crop bounds");
  const x = Math.floor((bounds?.x ?? 0) * viewport.width);
  const y = Math.floor((bounds?.y ?? 0) * viewport.height);
  const right = Math.ceil(
    ((bounds?.x ?? 0) + (bounds?.width ?? 1)) * viewport.width,
  );
  const bottom = Math.ceil(
    ((bounds?.y ?? 0) + (bounds?.height ?? 1)) * viewport.height,
  );
  const canvas = createCanvas(right - x, bottom - y);
  await page.render({
    canvas: canvas as unknown as HTMLCanvasElement,
    canvasContext: canvas.getContext(
      "2d",
    ) as unknown as CanvasRenderingContext2D,
    viewport,
    ...(bounds ? { transform: [1, 0, 0, 1, -x, -y] } : {}),
    background: "white",
  }).promise;
  return { canvas, scale };
}
