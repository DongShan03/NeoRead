// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { toast } from "sonner";
import { LibraryModeSwitcher } from "./LibraryModeSwitcher";
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));
const create = vi.fn();
const changed = vi.fn();
let root: Root, host: HTMLDivElement;
beforeEach(async () => {
  vi.clearAllMocks();
  create.mockReset().mockResolvedValue(true);
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal("PointerEvent", MouseEvent);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.stubGlobal("matchMedia", () => ({
    matches: true,
    addEventListener() {},
    removeEventListener() {},
  }));
  vi.stubGlobal("neoreadDesktop", {
    libraryCatalog: vi.fn(async () => ({
      activeId: "first",
      libraries: [{ id: "first", name: "论文库", directory: "/library" }],
    })),
    createLibrary: create,
  });
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () =>
    root.render(<LibraryModeSwitcher mode="papers" onChange={changed} />),
  );
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
const openMenu = async () => {
  await act(async () =>
    host
      .querySelector<HTMLButtonElement>('button[aria-label^="切换书库"]')!
      .click(),
  );
  await vi.waitFor(() =>
    expect(document.querySelector('[role="menu"]')).not.toBeNull(),
  );
};
const newItem = () =>
  [...document.querySelectorAll<HTMLElement>('[role="menuitem"]')].find(
    (node) => node.textContent?.trim() === "新建书库",
  )!;
const submit = () =>
  act(async () =>
    document
      .querySelector('[role="dialog"] form')!
      .dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })),
  );
const name = () =>
  document.querySelector<HTMLInputElement>('input[aria-label="书库名称"]')!;
const typeName = (value: string) =>
  act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value",
    )!.set!.call(name(), value);
    name().dispatchEvent(new Event("input", { bubbles: true }));
  });
it("places local library creation above the separator and document-type views", async () => {
  await openMenu();
  const item = newItem();
  const separator = document.querySelector('[role="separator"]')!;
  const paperView = document.querySelector(".library-mode-item")!;
  expect(
    item.compareDocumentPosition(separator) & Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  expect(
    separator.compareDocumentPosition(paperView) &
      Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  await act(async () => item.click());
  expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
    "默认关闭同步",
  );
  expect(name().value).toBe("论文库");
  await typeName("独立研究库");
  await submit();
  expect(create).toHaveBeenCalledExactlyOnceWith("独立研究库");
  expect(changed).not.toHaveBeenCalled();
});
it("keeps the requested name for retry if local library creation fails", async () => {
  create.mockRejectedValueOnce(new Error("磁盘不可写"));
  await openMenu();
  await act(async () => newItem().click());
  await typeName("研究库");
  await submit();
  expect(toast.error).toHaveBeenCalledWith("磁盘不可写");
  expect(name().value).toBe("研究库");
  await submit();
  expect(create).toHaveBeenCalledTimes(2);
});
