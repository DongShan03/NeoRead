// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { WebDAVSyncSettings } from "./WebDAVSyncSettings";
import { WebDAVSyncButton } from "./WebDAVSyncButton";
import { emptyWebDAVStatus, useWebDAVSync } from "./webdav-sync";

vi.mock("@neoread/api", () => ({
  api: {
    webdavConfig: vi.fn(),
    webdavStatus: vi.fn(),
    saveWebdavConfig: vi.fn(),
    disconnectWebdav: vi.fn(),
    testWebdav: vi.fn(async () => ({ ok: true })),
    runWebdav: vi.fn(),
    keepWebdavNotes: vi.fn(),
    resolveWebdav: vi.fn(),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const config = {
  account: "",
  folder: "论文库",
  deviceName: "Mac",
  intervalMinutes: 15,
  enabled: false,
  passwordSet: false,
};
let root: Root;
let host: HTMLDivElement;
beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal("PointerEvent", MouseEvent);
  vi.mocked(api.webdavConfig).mockResolvedValue(config);
  vi.mocked(api.webdavStatus).mockResolvedValue(emptyWebDAVStatus);
  vi.mocked(api.saveWebdavConfig).mockImplementation(async (value) => ({
    ...value,
    passwordSet: true,
  }));
  useWebDAVSync.setState({
    config: undefined,
    status: emptyWebDAVStatus,
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
const render = (withButton = false) =>
  act(async () => {
    root.render(
      <>
        {withButton && <WebDAVSyncButton />}
        <WebDAVSyncSettings open />
      </>,
    );
  });
const input = (type: string) =>
  host.querySelector<HTMLInputElement>('input[type="' + type + '"]')!;
const change = (type: string, value: string) =>
  act(async () => {
    const node = input(type);
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value",
    )!.set!.call(node, value);
    node.dispatchEvent(new Event("input", { bubbles: true }));
  });
const click = (label: string) =>
  act(async () => {
    [...host.querySelectorAll("button")]
      .find((button) => button.textContent?.trim() === label)!
      .click();
  });
const toggle = () =>
  act(async () =>
    host
      .querySelector<HTMLButtonElement>(
        '[role="switch"][aria-label="同步当前书库"]',
      )!
      .click(),
  );
it("defaults to the current library and accepts custom intervals with suggestions", async () => {
  await render(true);
  expect(host.querySelector('input[type="email"]')).toBeNull();
  expect(host.querySelector("[data-sync-state]")).toBeNull();
  expect(
    host.querySelector('[role="switch"]')?.getAttribute("aria-checked"),
  ).toBe("false");
  await toggle();
  expect(api.saveWebdavConfig).not.toHaveBeenCalled();
  expect(input("number").disabled).toBe(false);
  expect(input("number").value).toBe("15");
  expect(host.textContent).toContain("NeoRead/论文库/");
  expect(
    [...host.querySelectorAll("datalist option")].map((item) =>
      item.getAttribute("value"),
    ),
  ).toEqual(["5", "10", "15", "30", "60"]);
  await change("email", "reader@example.com");
  await change("password", "application-password");
  await change("number", "7");
  await click("保存");
  expect(api.saveWebdavConfig).toHaveBeenCalledWith(
    expect.objectContaining({
      folder: "论文库",
      intervalMinutes: 7,
      enabled: true,
      password: "application-password",
    }),
  );
  expect(api.runWebdav).not.toHaveBeenCalled();
  expect(host.querySelector("[data-sync-state]")).not.toBeNull();
});
it("uses the active independent library name without enabling other libraries", async () => {
  vi.stubGlobal("neoreadDesktop", {
    libraryCatalog: async () => ({
      activeId: "second",
      libraries: [
        { id: "first", name: "论文库", directory: "/first" },
        { id: "second", name: "研究资料", directory: "/second" },
      ],
    }),
  });
  await render();
  await toggle();
  expect(host.textContent).toContain("NeoRead/研究资料/");
  expect(api.saveWebdavConfig).not.toHaveBeenCalled();
});
it("keeps an already stored application password out of the renderer input", async () => {
  vi.mocked(api.webdavConfig).mockResolvedValue({
    ...config,
    account: "reader@example.com",
    passwordSet: true,
    enabled: true,
  });
  await render();
  expect(input("password").placeholder).toContain("已保存");
  expect(input("password").value).toBe("");
  await click("测试连接");
  expect(api.testWebdav).toHaveBeenCalledWith(
    expect.objectContaining({ password: "", passwordSet: true }),
  );
});

it("allows note conflicts to be deferred or kept together without forcing a dialog", async () => {
  const conflict = {
    key: "document/paper",
    id: "conflict",
    title: "Paper",
    canKeepBoth: true,
    versions: [
      {
        id: "a",
        device: "Mac",
        updated: "2026-11-03T10:00:00Z",
        deleted: false,
        preview: "note A",
      },
      {
        id: "b",
        device: "Windows",
        updated: "2026-11-03T10:00:00Z",
        deleted: false,
        preview: "note B",
      },
    ],
  };
  vi.mocked(api.webdavStatus).mockResolvedValue({
    ...emptyWebDAVStatus,
    phase: "conflict",
    conflicts: [conflict],
  });
  vi.mocked(api.keepWebdavNotes).mockResolvedValue({
    ...emptyWebDAVStatus,
    phase: "scanning",
  });
  await render();
  await toggle();
  expect(document.querySelector('[role="dialog"]')).toBeNull();
  await click("选择版本");
  const dialogButton = (text: string) =>
    [...document.querySelectorAll("button")].find(
      (button) => button.textContent?.trim() === text,
    )!;
  await act(async () => dialogButton("稍后处理").click());
  expect(api.keepWebdavNotes).not.toHaveBeenCalled();
  await click("选择版本");
  await act(async () => dialogButton("都保留").click());
  expect(api.keepWebdavNotes).toHaveBeenCalledWith(conflict.key, conflict.id);
  expect(useWebDAVSync.getState().status.conflicts).toHaveLength(0);
  expect(api.resolveWebdav).not.toHaveBeenCalled();
});

it("offers a configured shared connection without enabling a different library", async () => {
  vi.mocked(api.webdavConfig).mockResolvedValue({
    ...config,
    account: "reader@example.com",
    passwordSet: true,
    enabled: false,
    libraryConfigured: false,
  });
  vi.stubGlobal("neoreadDesktop", {
    libraryCatalog: async () => ({
      activeId: "other",
      libraries: [{ id: "other", name: "研究资料", directory: "/other" }],
    }),
  });
  await render();
  expect(host.querySelector('input[type="email"]')).toBeNull();
  expect(
    host.querySelector('[role="switch"]')?.getAttribute("aria-checked"),
  ).toBe("false");
  await toggle();
  expect(input("email").value).toBe("reader@example.com");
  expect(host.textContent).toContain("NeoRead/研究资料/");
  expect(
    host.querySelector('[role="switch"]')?.getAttribute("aria-checked"),
  ).toBe("true");
  expect(api.saveWebdavConfig).not.toHaveBeenCalled();
  expect(
    [...host.querySelectorAll("button")].find(
      (button) => button.textContent?.trim() === "立即同步",
    )?.disabled,
  ).toBe(true);
});

it("turns off configured sync immediately and hides its credentials without deleting cloud data", async () => {
  vi.mocked(api.webdavConfig).mockResolvedValue({
    ...config,
    account: "reader@example.com",
    passwordSet: true,
    enabled: true,
  });
  vi.mocked(api.disconnectWebdav).mockImplementation(async () => {
    vi.mocked(api.webdavConfig).mockResolvedValue({
      ...config,
      account: "reader@example.com",
      passwordSet: true,
      libraryConfigured: false,
    });
  });
  await render(true);
  await toggle();
  expect(api.disconnectWebdav).toHaveBeenCalledOnce();
  expect(host.querySelector('input[type="email"]')).toBeNull();
  expect(
    host.querySelector("[data-sync-state]")?.getAttribute("data-sync-state"),
  ).toBe("disabled");
  await toggle();
  expect(input("email").value).toBe("reader@example.com");
  expect(input("password").value).toBe("");
  expect(api.saveWebdavConfig).not.toHaveBeenCalled();
});
