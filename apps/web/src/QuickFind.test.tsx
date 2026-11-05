// @vitest-environment jsdom
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { QuickFind } from "./QuickFind";
import { paper } from "./papers/fixtures";
const docs = [paper({ id: "epub", title: "Alpha EPUB", type: "epub", author: "Writer" }), paper({ id: "pdf", title: "Alpha PDF", author: "Researcher" })];
const onDocument = vi.fn(), onSettings = vi.fn(), onImport = vi.fn(), onSwitchLibrary = vi.fn(), scroll = vi.fn();
let root: Root, host: HTMLDivElement;
function Harness({ documents = docs, mode = "books" as "books" | "papers" }) {
  const [open, setOpen] = useState(true);
  return <><button onClick={() => setOpen(true)}>查找</button><QuickFind open={open} onOpenChange={setOpen} documents={documents} mode={mode} onDocument={onDocument} onSettings={onSettings} onImport={onImport} onSwitchLibrary={onSwitchLibrary} /></>;
}
const input = () => document.querySelector<HTMLInputElement>('[role="combobox"]')!;
const options = () => [...document.querySelectorAll<HTMLElement>('[role="option"]')];
async function query(value: string) {
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input(), value);
    input().dispatchEvent(new Event("input", { bubbles: true }));
  });
}
async function key(key: string, isComposing = false) {
  await act(async () => input().dispatchEvent(new KeyboardEvent("keydown", { key, isComposing, bubbles: true, cancelable: true })));
}
beforeEach(async () => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true); vi.clearAllMocks();
  Element.prototype.scrollIntoView = scroll;
  host = document.createElement("div"); document.body.append(host); root = createRoot(host);
  await act(async () => root.render(<Harness />));
});
afterEach(async () => { await act(async () => root.unmount()); host.remove(); vi.unstubAllGlobals(); });
it("shows only actions for an empty query and executes keyboard selection", async () => {
  expect(options()).toHaveLength(3); expect(options()[0].textContent).toBe("打开设置");
  expect(input().getAttribute("aria-activedescendant")).toBe(options()[0].id);
  await key("ArrowDown"); expect(options()[1].getAttribute("aria-selected")).toBe("true");
  await key("Enter"); expect(onImport).toHaveBeenCalledOnce(); expect(input()).toBeNull();
});
it("selects the first matching document and wraps navigation with scrolling", async () => {
  await query("Alpha"); expect(options()).toHaveLength(2);
  expect(options()[0].textContent).toContain("Writer"); expect(options()[0].querySelector(".lucide-book-open")).not.toBeNull();
  expect(options()[1].querySelector(".lucide-file-text")).not.toBeNull();
  await key("ArrowUp"); expect(options()[1].getAttribute("aria-selected")).toBe("true");
  expect(scroll).toHaveBeenCalledWith({ block: "nearest" });
  await key("ArrowDown"); await key("Enter", true); expect(onDocument).not.toHaveBeenCalled();
  await key("Enter"); expect(onDocument).toHaveBeenCalledWith(docs[0]);
});
it("shows no results and clears query and selection on close", async () => {
  await query("nothing matches"); expect(options()).toHaveLength(0);
  expect(document.body.textContent).toContain("没有匹配的文档");
  await key("Enter"); expect(onDocument).not.toHaveBeenCalled();
  await key("Escape"); expect(input()).toBeNull();
  await act(async () => host.querySelector("button")!.click());
  expect(input().value).toBe(""); expect(options()[0].getAttribute("aria-selected")).toBe("true");
});
it("caps matching documents at fifty", async () => {
  await act(async () => root.render(<Harness documents={Array.from({ length: 80 }, (_, i) => paper({ id: String(i), title: `Paper ${i}` }))} />));
  await query("Paper"); expect(options()).toHaveLength(50);
});
it.each([0, 2])("runs action %d", async (index) => {
  await act(async () => options()[index].click());
  expect(index === 0 ? onSettings : onSwitchLibrary).toHaveBeenCalledOnce();
});
it("uses the paper import flow in paper mode", async () => {
  await act(async () => root.render(<Harness mode="papers" />));
  expect(options()[1].textContent).toBe("导入论文");
  await act(async () => options()[1].click()); expect(onImport).toHaveBeenCalledOnce();
});
