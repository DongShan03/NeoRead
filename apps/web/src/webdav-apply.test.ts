// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { flushProgress } from "./progress";
import { importWithPaperNotes } from "./paper-note-state";
import { emptyWebDAVStatus, installWebDAVSync } from "./webdav-sync";

vi.mock("@neoread/api", () => ({
  api: { webdavConfig: vi.fn(), webdavStatus: vi.fn(), applyWebdav: vi.fn() },
}));
vi.mock("./progress", () => ({
  flushProgress: vi.fn(async () => {}),
  hasPendingProgress: () => false,
}));
vi.mock("./paper-note-state", () => ({
  flushPaperNotes: vi.fn(async () => {}),
  hasPendingPaperNotes: () => false,
  importWithPaperNotes: vi.fn(async (run: () => Promise<unknown>) => run()),
}));
vi.mock("./store", () => ({
  loadLibraryPreferences: vi.fn(),
  refreshLibrary: vi.fn(),
  refreshTrash: vi.fn(),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
let stop: (() => void) | undefined;
beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.webdavConfig).mockResolvedValue({
    account: "reader@example.com",
    passwordSet: true,
    enabled: true,
    folder: "论文库",
    intervalMinutes: 15,
    deviceName: "Mac",
  });
  vi.mocked(api.webdavStatus).mockResolvedValue({
    ...emptyWebDAVStatus,
    phase: "ready",
    applyToken: "pending-apply",
  });
  vi.mocked(api.applyWebdav).mockResolvedValue({
    ...emptyWebDAVStatus,
    phase: "failed",
    failed: true,
  });
});
afterEach(() => {
  stop?.();
  stop = undefined;
});

it.each(["progress", "notes"])(
  "reports a failed %s save without acknowledging remote application",
  async (kind) => {
    if (kind === "progress")
      vi.mocked(flushProgress).mockRejectedValueOnce(new Error("save failed"));
    else
      vi.mocked(importWithPaperNotes).mockRejectedValueOnce(
        new Error("save failed"),
      );
    stop = installWebDAVSync();
    await vi.waitFor(() =>
      expect(api.applyWebdav).toHaveBeenCalledExactlyOnceWith(
        "pending-apply",
        false,
      ),
    );
  },
);
