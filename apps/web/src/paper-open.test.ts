import { beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import type { Processing } from "@neoread/core";
import {
  openLibraryDocument,
  openLibraryDocuments,
  openLinkTarget,
  openWhenParsed,
  useReaderStore,
} from "./store";
import { paper } from "./papers/fixtures";

vi.mock("@neoread/api", () => ({
  api: {
    processing: vi.fn(),
    update: vi.fn(async () => ({})),
    saveLibraryPreferences: vi.fn(async (value) => value),
  },
}));
const doc = paper();
const job = (patch: Partial<Processing> = {}): Processing => ({
  documentId: doc.id,
  phase: "learning",
  status: "running",
  pagesDone: 0,
  pagesTotal: 2,
  translationsDone: 0,
  translationsTotal: 0,
  detail: "",
  updatedAt: "",
  ...patch,
});
beforeEach(() => {
  vi.resetAllMocks();
  useReaderStore.getState().open(null);
  useReaderStore.setState({
    documents: [doc],
    linkTarget: null,
    libraryPreferences: { mode: "books" },
  });
});
it.each(["queued", "running", "paused", "waiting", "failed"] as const)(
  "blocks a paper whose parsing is %s",
  async (status) => {
    vi.mocked(api.processing).mockResolvedValue([job({ status })]);
    await expect(openLibraryDocument(doc)).rejects.toThrow(
      ["queued", "running"].includes(status)
        ? "论文飞速解析中，请稍等" : "论文解析没有完成，无法打开",
    );
    expect(useReaderStore.getState().active).toBeNull();
  },
);
it("blocks unknown and incomplete parsing", async () => {
  for (const [jobs, reason] of [
    [[], "论文解析任务已不存在，请重新解析文档"],
    [[job({ phase: "translating", incomplete: true })], "论文正文不完整，无法打开"],
  ] as const) {
    vi.mocked(api.processing).mockResolvedValue([...jobs]);
    await expect(openLibraryDocument(doc)).rejects.toThrow(
      reason,
    );
    expect(useReaderStore.getState().active).toBeNull();
  }
});
it.each(["running", "paused", "failed", "complete"] as const)(
  "opens parsed papers even when translation is %s",
  async (status) => {
    vi.mocked(api.processing).mockResolvedValue([
      job({ phase: "translating", status }),
    ]);
    expect(await openLibraryDocument(doc)).toBe(true);
    expect(useReaderStore.getState().active).toEqual(doc);
  },
);
it("allows retry after parsing completes", async () => {
  vi.mocked(api.processing)
    .mockResolvedValueOnce([job()])
    .mockResolvedValueOnce([job({ phase: "ready", status: "complete" })]);
  await expect(openLibraryDocument(doc)).rejects.toThrow();
  expect(await openLibraryDocument(doc)).toBe(true);
});
it("guards links without switching library or saving the blocked target", async () => {
  vi.mocked(api.processing).mockResolvedValue([job()]);
  await expect(openLinkTarget({ id: doc.id, page: 1 })).rejects.toThrow(
    "论文飞速解析中，请稍等",
  );
  expect(useReaderStore.getState().active).toBeNull();
  expect(useReaderStore.getState().linkTarget).toBeNull();
  expect(useReaderStore.getState().libraryPreferences.mode).toBe("books");
});
it("keeps books available without a processing request", async () => {
  const book = paper({ library: "books" });
  expect(await openLibraryDocument(book)).toBe(true);
  expect(api.processing).not.toHaveBeenCalled();
});
it("does not open when the processing request fails", async () => {
  vi.mocked(api.processing).mockRejectedValue(new Error("连接失败"));
  await expect(openLibraryDocument(doc)).rejects.toThrow("连接失败");
  expect(useReaderStore.getState().active).toBeNull();
});
it("does not replace a newer navigation with a late response", async () => {
  let resolve!: (jobs: Processing[]) => void;
  vi.mocked(api.processing).mockReturnValue(
    new Promise((done) => {
      resolve = done;
    }),
  );
  const pending = openLibraryDocument(doc);
  const book = paper({ id: "book", library: "books" });
  await openLibraryDocument(book);
  resolve([job({ phase: "ready" })]);
  expect(await pending).toBe(false);
  expect(useReaderStore.getState().active).toEqual(book);
});
it("opens a paper from Open With once parsing finishes", async () => {
  vi.useFakeTimers();
  try {
    vi.mocked(api.processing)
      .mockResolvedValueOnce([job()])
      .mockResolvedValueOnce([job({ pagesDone: 1 })])
      .mockResolvedValue([job({ phase: "ready", status: "complete" })]);
    const onWaiting = vi.fn();
    const opened = openWhenParsed(doc.id, onWaiting);
    await vi.advanceTimersByTimeAsync(0);
    expect(onWaiting).toHaveBeenCalledOnce();
    expect(useReaderStore.getState().active).toBeNull();
    expect(useReaderStore.getState().libraryPreferences.mode).toBe("papers");
    await vi.advanceTimersByTimeAsync(2400);
    expect(await opened).toBe(true);
    expect(onWaiting).toHaveBeenCalledOnce();
    expect(useReaderStore.getState().active).toEqual(doc);
  } finally {
    vi.useRealTimers();
  }
});
it.each([
  [job({ status: "failed", detail: "解析失败" }), "解析失败"],
  [job({ status: "waiting", detail: "需要配置" }), "需要配置"],
  [
    job({ phase: "ready", status: "complete", incomplete: true }),
    "论文正文不完整，无法打开",
  ],
])("reports a paper from Open With that cannot open", async (state, error) => {
  vi.mocked(api.processing).mockResolvedValue([state]);
  await expect(openWhenParsed(doc.id, vi.fn())).rejects.toThrow(error);
  expect(useReaderStore.getState().active).toBeNull();
});
it("replaces an earlier Open With wait", async () => {
  vi.useFakeTimers();
  try {
    vi.mocked(api.processing).mockResolvedValue([job()]);
    const first = openWhenParsed(doc.id, vi.fn());
    await vi.advanceTimersByTimeAsync(0);
    const book = paper({ id: "book", library: "books" });
    useReaderStore.setState({ documents: [doc, book] });
    expect(await openWhenParsed(book.id, vi.fn())).toBe(true);
    vi.mocked(api.processing).mockResolvedValue([
      job({ phase: "ready", status: "complete" }),
    ]);
    await vi.advanceTimersByTimeAsync(1200);
    expect(await first).toBe(true);
    expect(useReaderStore.getState().active).toEqual(book);
  } finally {
    vi.useRealTimers();
  }
});

it("cancels automatic opening after the reader chooses another document", async () => {
  vi.useFakeTimers();
  try {
    vi.mocked(api.processing).mockResolvedValue([job()]);
    const pending = openWhenParsed(doc.id, vi.fn());
    await vi.advanceTimersByTimeAsync(0);
    const book = paper({ id: "chosen-book", library: "books" });
    useReaderStore.setState({ documents: [doc, book] });
    await openLibraryDocument(book);
    vi.mocked(api.processing).mockResolvedValue([job({ phase: "ready" })]);
    await vi.advanceTimersByTimeAsync(1200);
    expect(await pending).toBe(true);
    expect(useReaderStore.getState().active?.id).toBe(book.id);
  } finally {
    vi.useRealTimers();
  }
});

it("stops waiting when the document is removed from the library", async () => {
  vi.useFakeTimers();
  try {
    vi.mocked(api.processing).mockResolvedValue([job()]);
    const pending = openWhenParsed(doc.id, vi.fn());
    await vi.advanceTimersByTimeAsync(0);
    useReaderStore.getState().setDocuments([]);
    await vi.advanceTimersByTimeAsync(1200);
    expect(await pending).toBe(false);
    const calls = vi.mocked(api.processing).mock.calls.length;
    await vi.advanceTimersByTimeAsync(5000);
    expect(api.processing).toHaveBeenCalledTimes(calls);
  } finally {
    vi.useRealTimers();
  }
});

it("reports a missing parsing task instead of polling indefinitely", async () => {
  vi.mocked(api.processing).mockResolvedValue([]);
  await expect(openWhenParsed(doc.id, vi.fn())).rejects.toThrow(
    "解析任务已不存在",
  );
  expect(api.processing).toHaveBeenCalledOnce();
});


it("opens only allowed documents with one processing snapshot and records every visit", async () => {
  const book = paper({ id: "book", library: "books" });
  const ready = paper({ id: "ready" });
  vi.mocked(api.processing).mockResolvedValue([job(), job({ documentId: ready.id, phase: "ready" })]);
  const result = await openLibraryDocuments([doc, book, ready]);
  expect(api.processing).toHaveBeenCalledOnce();
  expect(result.opened).toEqual([book, ready]);
  expect(result.skipped).toEqual([{ document: doc, reason: "论文飞速解析中，请稍等" }]);
  expect(api.update).toHaveBeenCalledWith(book.id, {});
  expect(api.update).toHaveBeenCalledWith(ready.id, {});
  expect(useReaderStore.getState().active).toEqual(book);
});
it("keeps the current view when every requested document is blocked", async () => {
  const book = paper({ id: "book", library: "books" });
  useReaderStore.getState().open(book);
  vi.mocked(api.processing).mockResolvedValue([job({ status: "failed", detail: "解析失败" })]);
  expect((await openLibraryDocuments([doc])).skipped[0].reason).toBe("解析失败");
  expect(useReaderStore.getState().active).toEqual(book);
  expect(api.update).not.toHaveBeenCalled();
});
it("does not replace a newer navigation with a late batch response", async () => {
  let resolve!: (jobs: Processing[]) => void;
  vi.mocked(api.processing).mockReturnValue(new Promise((done) => { resolve = done; }));
  const pending = openLibraryDocuments([doc]);
  const book = paper({ id: "book", library: "books" });
  useReaderStore.getState().open(book);
  resolve([job({ phase: "ready" })]);
  expect((await pending).opened).toEqual([]);
  expect(useReaderStore.getState().active).toEqual(book);
});
it("does not cancel a pending open for an empty tab group", async () => {
  let resolve!: (jobs: Processing[]) => void;
  vi.mocked(api.processing).mockReturnValue(new Promise((done) => { resolve = done; }));
  const pending = openLibraryDocument(doc);
  useReaderStore.getState().openMany([]);
  await openLibraryDocuments([]);
  resolve([job({ phase: "ready" })]);
  expect(await pending).toBe(true);
});
it("preserves receiving papers' existing opening permission", async () => {
  vi.mocked(api.processing).mockResolvedValue([job({ receiving: true })]);
  expect(await openLibraryDocument(doc)).toBe(true);
});

it("keeps Open With waiting for the learning phase before reporting incomplete text", async () => {
  vi.useFakeTimers();
  try {
    vi.mocked(api.processing).mockResolvedValueOnce([job({ incomplete: true })])
      .mockResolvedValue([job({ incomplete: true, phase: "ready" })]);
    const onWaiting = vi.fn();
    const pending = openWhenParsed(doc.id, onWaiting);
    const rejection = expect(pending).rejects.toThrow("论文正文不完整，无法打开");
    await vi.advanceTimersByTimeAsync(0);
    expect(onWaiting).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1200);
    await rejection;
  } finally { vi.useRealTimers(); }
});

it("returns opened and skipped documents even when a recent-visit write fails", async () => {
  const book = paper({ id: "book", library: "books" });
  const other = paper({ id: "other", library: "books" });
  vi.mocked(api.processing).mockResolvedValue([job()]);
  vi.mocked(api.update).mockRejectedValueOnce(new Error("写入失败"));
  const result = await openLibraryDocuments([book, doc, other]);
  expect(result.opened).toEqual([book, other]);
  expect(result.skipped).toEqual([{ document: doc, reason: "论文飞速解析中，请稍等" }]);
  expect(result).toMatchObject({ recentErrors: ["写入失败"] });
  expect(api.update).toHaveBeenCalledTimes(2);
  expect(useReaderStore.getState().active).toEqual(book);
});
