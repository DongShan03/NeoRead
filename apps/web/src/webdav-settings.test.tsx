// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { WebDAVSyncSettings } from "./WebDAVSyncSettings";
import { emptyWebDAVStatus, useWebDAVSync } from "./webdav-sync";

vi.mock("@neoread/api", () => ({
  api: {
    webdavConfig: vi.fn(),
    webdavStatus: vi.fn(),
    saveWebdavConfig: vi.fn(),
    testWebdav: vi.fn(async () => ({ ok: true })),
    runWebdav: vi.fn(),
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
  vi.mocked(api.webdavConfig).mockResolvedValue(config);
  vi.mocked(api.webdavStatus).mockResolvedValue(emptyWebDAVStatus);
  vi.mocked(api.saveWebdavConfig).mockImplementation(async (value) => ({
    ...value,
    passwordSet: true,
  }));
  useWebDAVSync.setState({ config: undefined, status: emptyWebDAVStatus });
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
    root.render(<WebDAVSyncSettings open />);
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
it("defaults to the current library and accepts custom intervals with suggestions", async () => {
  await render();
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
