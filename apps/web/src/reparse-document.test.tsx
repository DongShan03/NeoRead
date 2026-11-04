import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Document } from "@neoread/core";
const mocks = vi.hoisted(() => ({
  start: vi.fn(),
  refresh: vi.fn(),
  close: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
}));
vi.mock("@neoread/api", () => ({ api: { setAssistance: mocks.start } }));
vi.mock("./store", () => ({
  refreshLibrary: mocks.refresh,
  useReaderStore: { getState: () => ({ closeTabs: mocks.close }) },
}));
vi.mock("sonner", () => ({
  toast: {
    loading: vi.fn(() => "queued"),
    success: mocks.success,
    error: mocks.error,
  },
}));
import { reparseDocument } from "./reparse-document";
const doc = {
  id: "doc",
  type: "pdf",
  title: "Paper",
  library: "papers",
} as Document;
beforeEach(() => {
  vi.clearAllMocks();
  mocks.start.mockReset().mockResolvedValue({});
  mocks.refresh.mockResolvedValue(undefined);
});
afterEach(() => vi.restoreAllMocks());
it("queues a clean parse and translation and closes the stale reading tab", async () => {
  await reparseDocument(doc);
  expect(mocks.start).toHaveBeenCalledExactlyOnceWith("doc", "retranslate", {
    restart: true,
    clearHistory: true,
  });
  expect(mocks.close).toHaveBeenCalledWith(["doc"]);
  expect(mocks.success).toHaveBeenCalledWith(
    "已加入重新解析翻译队列。",
    expect.anything(),
  );
});
it("joins repeated requests until the first queue operation finishes", async () => {
  let finish!: () => void;
  mocks.start.mockImplementationOnce(
    () =>
      new Promise<void>((resolve) => {
        finish = resolve;
      }),
  );
  const pending = reparseDocument(doc);
  await reparseDocument(doc);
  expect(mocks.start).toHaveBeenCalledOnce();
  finish();
  await pending;
});
it("keeps the reading tab if starting the reset fails and permits retry", async () => {
  mocks.start.mockRejectedValueOnce(new Error("cannot queue"));
  await reparseDocument(doc);
  expect(mocks.close).not.toHaveBeenCalled();
  expect(mocks.error).toHaveBeenCalledWith("cannot queue", expect.anything());
  await reparseDocument(doc);
  expect(mocks.start).toHaveBeenCalledTimes(2);
});
