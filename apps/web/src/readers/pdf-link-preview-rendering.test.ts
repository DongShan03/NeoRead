// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import type { PDFDocumentProxy } from "pdfjs-dist";
import type { LinkPreview } from "@neoread/core";
import { PDFLinkPreview } from "./pdf-link-preview";

const previews: PDFLinkPreview[] = [];
afterEach(() => {
  previews.splice(0).forEach((preview) => preview.destroy());
  document.body.replaceChildren();
  vi.restoreAllMocks();
});

function fixture(width = 612, height = 792) {
  const rendered: {
    canvas: HTMLCanvasElement;
    width: number;
    height: number;
  }[] = [];
  const context = {} as CanvasRenderingContext2D;
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(context);
  vi.spyOn(HTMLCanvasElement.prototype, "toDataURL").mockReturnValue(
    "data:image/png;base64,fixture",
  );
  vi.spyOn(window, "devicePixelRatio", "get").mockReturnValue(2);
  const render = vi.fn(({ canvas }: { canvas: HTMLCanvasElement }) => {
    rendered.push({ canvas, width: canvas.width, height: canvas.height });
    return { promise: Promise.resolve(), cancel: vi.fn() };
  });
  const page = {
    getViewport: ({ scale }: { scale: number }) => ({
      width: width * scale,
      height: height * scale,
      convertToPdfPoint: (x: number, y: number) => [x, y],
      convertToViewportPoint: (x: number, y: number) => [x, y],
    }),
    render,
  };
  const pdf = { getPage: vi.fn().mockResolvedValue(page) };
  const container = document.createElement("div");
  const link = document.createElement("a");
  container.append(link);
  document.body.append(container);
  const show = vi.fn<(value: LinkPreview | null) => void>();
  const preview = new PDFLinkPreview(
    container,
    () => pdf as unknown as PDFDocumentProxy,
    show,
    () => {},
  );
  previews.push(preview);
  return { preview, link, show, pdf, page, render, rendered };
}

it.each([
  [612, 792],
  [100, 10000],
])(
  "renders only the preview crop and releases its canvas (%i × %i page)",
  async (width, height) => {
    const { preview, link, show, rendered } = fixture(width, height);
    await preview.previewLocation(
      { type: "pdf", page: 1, x: 0.75, y: 0.25 },
      link,
    );
    expect(show).toHaveBeenCalledWith(
      expect.objectContaining({
        page: 1,
        image: "data:image/png;base64,fixture",
      }),
    );
    expect(rendered).toHaveLength(1);
    expect(rendered[0].width * rendered[0].height).toBeLessThanOrEqual(
      8_000_000,
    );
    expect(Math.max(rendered[0].width, rendered[0].height)).toBeLessThanOrEqual(
      8192,
    );
    expect(rendered[0].canvas.width * rendered[0].canvas.height).toBe(0);
    preview.hide();
    expect(show).toHaveBeenLastCalledWith(null);
  },
);

it("cancels an unfinished hover render and releases its pixels when hidden", async () => {
  const { preview, link, render, rendered, show } = fixture();
  let reject!: (error: Error) => void;
  const cancel = vi.fn(() => reject(new Error("render cancelled")));
  render.mockImplementation(({ canvas }) => {
    rendered.push({ canvas, width: canvas.width, height: canvas.height });
    return { promise: new Promise<void>((_, no) => (reject = no)), cancel };
  });
  const pending = preview.previewLocation({ type: "pdf", page: 1 }, link);
  await vi.waitFor(() => expect(render).toHaveBeenCalledTimes(1));
  preview.hide();
  await pending;
  expect(cancel).toHaveBeenCalledTimes(1);
  expect(rendered[0].canvas.width * rendered[0].canvas.height).toBe(0);
  expect(show.mock.calls.every(([value]) => value === null)).toBe(true);
});

it("keeps a pinned preview's independent render usable after hover is hidden", async () => {
  const { preview, link, show, render } = fixture();
  await preview.previewLocation({ type: "pdf", page: 1 }, link);
  const pinned = show.mock.calls.at(-1)![0]!;
  preview.hide();
  await expect(
    pinned.renderImage!(1200, new AbortController().signal),
  ).resolves.toBe("data:image/png;base64,fixture");
  expect(render).toHaveBeenCalledTimes(2);
});
