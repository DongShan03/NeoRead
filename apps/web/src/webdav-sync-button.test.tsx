// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { beforeEach, afterEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { toast } from "sonner";
import { flushProgress } from "./progress";
import { flushPaperNotes, importWithPaperNotes } from "./paper-note-state";
import { flushLibraryPreferences } from "./store";
import { WebDAVSyncButton } from "./WebDAVSyncButton";
import {
  emptyWebDAVStatus,
  installWebDAVSync,
  setWebDAVStatus,
  useWebDAVSync,
} from "./webdav-sync";

vi.mock("@neoread/api", () => ({
  api: {
    runWebdav: vi.fn(),
    webdavConfig: vi.fn(),
    webdavStatus: vi.fn(),
    applyWebdav: vi.fn(),
  },
}));
vi.mock("./progress", () => ({
  flushProgress: vi.fn(async () => {}),
  hasPendingProgress: () => false,
}));
vi.mock("./paper-note-state", () => ({
  flushPaperNotes: vi.fn(async () => {}),
  importWithPaperNotes: vi.fn(async (run: () => Promise<unknown>) => run()),
  hasPendingPaperNotes: () => false,
}));
vi.mock("./store", () => ({
  flushLibraryPreferences: vi.fn(async () => {}),
  loadLibraryPreferences: vi.fn(),
  refreshLibrary: vi.fn(),
  refreshTrash: vi.fn(),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const config = {
  account: "reader@example.com",
  passwordSet: true,
  enabled: true,
  folder: "论文库",
  intervalMinutes: 15,
  deviceName: "Mac",
};
const clean = {
  ...emptyWebDAVStatus,
  phase: "complete" as const,
  dirty: false,
  lastSync: "2026-11-03T10:00:00Z",
};
let host: HTMLDivElement, root: Root;
beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  useWebDAVSync.setState({
    config,
    status: clean,
    starting: false,
    localFailure: undefined,
  });
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
const render = () =>
  act(async () => {
    root.render(<WebDAVSyncButton />);
  });
const marker = () =>
  host.querySelector("[data-sync-state]")?.getAttribute("data-sync-state");
const click = () =>
  act(async () => {
    host.querySelector("button")!.click();
  });

it("hides the icon before any WebDAV connection is configured", async () => {
  useWebDAVSync.setState({ config: undefined });
  await render();
  expect(host.querySelector("button")).toBeNull();
});
it("shows an empty ring for a disabled library and opens its sync settings", async () => {
  useWebDAVSync.setState({
    config: { ...config, enabled: false },
    status: { ...clean, failed: true },
  });
  const opened = vi.fn();
  window.addEventListener("neoread:open-sync-settings", opened);
  try {
    await render();
    expect(marker()).toBe("disabled");
    expect(host.querySelectorAll("svg")).toHaveLength(1);
    await click();
    expect(opened).toHaveBeenCalledTimes(1);
    expect(api.runWebdav).not.toHaveBeenCalled();
  } finally {
    window.removeEventListener("neoread:open-sync-settings", opened);
  }
});
it("rotates without a center signal during sync, including after a failure", async () => {
  useWebDAVSync.setState({
    status: { ...clean, phase: "uploading", failed: true },
  });
  await render();
  expect(marker()).toBe("syncing");
  expect(host.querySelectorAll("svg")).toHaveLength(1);
  expect(host.querySelector("svg")?.classList.contains("animate-spin")).toBe(
    true,
  );
  expect(
    host.querySelector("svg")?.classList.contains("motion-reduce:animate-none"),
  ).toBe(true);
  expect(host.querySelector("button")?.disabled).toBe(true);
});
it("changes the check to an exclamation after a library edit", async () => {
  await render();
  expect(marker()).toBe("synced");
  await act(async () => setWebDAVStatus({ ...clean, dirty: true }));
  expect(marker()).toBe("pending");
  expect(host.querySelectorAll("svg")).toHaveLength(2);
});
it("flushes drafts before manual sync and shows a green success notification", async () => {
  vi.mocked(api.runWebdav).mockImplementation(async () => {
    expect(flushProgress).toHaveBeenCalledTimes(1);
    expect(flushPaperNotes).toHaveBeenCalledTimes(1);
    expect(flushLibraryPreferences).toHaveBeenCalledTimes(1);
    return {
      ...clean,
      manual: true,
      runId: "manual-success",
      lastSync: "2026-11-03T10:10:00Z",
    };
  });
  await render();
  await click();
  expect(marker()).toBe("synced");
  expect(toast.success).toHaveBeenCalledWith(
    "书库同步完成",
    expect.objectContaining({ position: "bottom-right" }),
  );
  expect(host.querySelector("svg")?.classList.contains("animate-spin")).toBe(
    false,
  );
});
it("keeps the failed marker through edits and old status polls until a new success", async () => {
  vi.mocked(api.runWebdav).mockRejectedValueOnce(new Error("offline"));
  await render();
  await click();
  expect(marker()).toBe("failed");
  expect(toast.error).toHaveBeenCalledTimes(1);
  await act(async () => setWebDAVStatus({ ...clean, dirty: true }));
  expect(marker()).toBe("failed");
  await act(async () => setWebDAVStatus(clean));
  expect(marker()).toBe("failed");
  await act(async () =>
    setWebDAVStatus({
      ...clean,
      manual: true,
      runId: "retry-success",
      lastSync: "2026-11-03T10:20:00Z",
    }),
  );
  expect(marker()).toBe("synced");
});
it("reports automatic failures once per run using the red notification", async () => {
  const failed = {
    ...clean,
    runId: "automatic-failure",
    manual: false,
    failed: true,
    phase: "failed" as const,
    error: "connection failed",
  };
  await render();
  await act(async () => {
    setWebDAVStatus(failed);
    setWebDAVStatus({ ...failed, dirty: true });
  });
  expect(toast.error).toHaveBeenCalledExactlyOnceWith(
    "书库自动同步失败",
    expect.objectContaining({ position: "bottom-right" }),
  );
  expect(marker()).toBe("failed");
});

it.each(["preferences", "progress", "notes"])(
  "ends automatic sync when the %s save fails",
  async (kind) => {
    vi.mocked(api.webdavConfig).mockResolvedValue(config);
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
    if (kind === "preferences")
      vi.mocked(flushLibraryPreferences).mockRejectedValueOnce(
        new Error("save failed"),
      );
    else if (kind === "progress")
      vi.mocked(flushProgress).mockRejectedValueOnce(new Error("save failed"));
    else
      vi.mocked(importWithPaperNotes).mockRejectedValueOnce(
        new Error("save failed"),
      );
    const stop = installWebDAVSync();
    try {
      await vi.waitFor(() =>
        expect(api.applyWebdav).toHaveBeenCalledExactlyOnceWith(
          "pending-apply",
          false,
        ),
      );
    } finally {
      stop();
    }
  },
);
