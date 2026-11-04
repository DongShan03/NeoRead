// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";

vi.mock("@neoread/api", () => ({
  api: {
    saveLibraryPreferences: vi.fn(async (value) => value),
    saveSettings: vi.fn(async (value) => value),
  },
}));
vi.mock("./progress", () => ({ flushProgress: vi.fn(async () => {}) }));
vi.mock("./paper-note-state", () => ({
  flushPaperNotes: vi.fn(async () => {}),
  hasPendingPaperNotes: () => false,
  takePaperNoteSaveWarning: () => false,
}));

beforeEach(() => {
  vi.resetModules();
  vi.clearAllMocks();
});
afterEach(() => {
  Object.defineProperty(window, "neoreadDesktop", {
    configurable: true,
    value: undefined,
  });
});
function deferred() {
  let finish!: () => void;
  const promise = new Promise<void>((resolve) => {
    finish = resolve;
  });
  return { promise, finish };
}
async function setup() {
  let close!: () => Promise<unknown>;
  Object.defineProperty(window, "neoreadDesktop", {
    configurable: true,
    value: {
      onBeforeClose: (callback: typeof close) => {
        close = callback;
      },
    },
  });
  const { installCloseFlush } = await import("./close-flush");
  const store = await import("./store");
  const uninstall = installCloseFlush();
  return { close, uninstall, ...store };
}

it("awaits the latest queued preference before acknowledging close or export", async () => {
  const h = await setup();
  const started = deferred(),
    first = deferred(),
    second = deferred();
  vi.mocked(api.saveLibraryPreferences)
    .mockImplementationOnce(async (value) => {
      started.finish();
      await first.promise;
      return value;
    })
    .mockImplementationOnce(async (value) => {
      await second.promise;
      return value;
    });
  const a = h.updateLibraryPreferences({ papers: { categories: ["first"] } });
  const b = h.updateLibraryPreferences({
    papers: { categories: ["first", "second"] },
  });
  await started.promise;
  let closed = false;
  const closing = h.close().then(() => {
    closed = true;
  });
  await Promise.resolve();
  expect(closed).toBe(false);
  first.finish();
  await a;
  expect(closed).toBe(false);
  second.finish();
  await Promise.all([b, closing]);
  expect(api.saveLibraryPreferences).toHaveBeenLastCalledWith(
    expect.objectContaining({ papers: { categories: ["first", "second"] } }),
  );
  expect(closed).toBe(true);
  h.uninstall();
});

it("also saves preference edits made while other close-time saves are pending", async () => {
  const h = await setup();
  const settings = deferred(),
    started = deferred(),
    preference = deferred();
  vi.mocked(api.saveSettings).mockImplementationOnce(async (value) => {
    started.finish();
    await settings.promise;
    return value;
  });
  let closed = false;
  const closing = h.close().then(() => {
    closed = true;
  });
  await started.promise;
  vi.mocked(api.saveLibraryPreferences).mockImplementationOnce(
    async (value) => {
      await preference.promise;
      return value;
    },
  );
  const saving = h.updateLibraryPreferences({ primaryMode: "books" });
  settings.finish();
  for (let i = 0; i < 12; i++) await Promise.resolve();
  expect(closed).toBe(false);
  preference.finish();
  await Promise.all([saving, closing]);
  h.uninstall();
});

it("reports a failed preference flush and can retry without another edit", async () => {
  const h = await setup();
  vi.mocked(api.saveLibraryPreferences)
    .mockRejectedValueOnce(new Error("offline"))
    .mockRejectedValueOnce(new Error("offline"));
  await expect(
    h.updateLibraryPreferences({ primaryMode: "books" }),
  ).rejects.toThrow("offline");
  await expect(h.close()).rejects.toThrow("offline");
  await expect(h.close()).resolves.toBeUndefined();
  expect(api.saveLibraryPreferences).toHaveBeenLastCalledWith(
    expect.objectContaining({ primaryMode: "books" }),
  );
  h.uninstall();
});
