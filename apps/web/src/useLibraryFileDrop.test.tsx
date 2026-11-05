// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { useLibraryFileDrop } from "./useLibraryFileDrop";
let root: Root, host: HTMLDivElement;
const imported = vi.fn();
function Harness() {
  const { dragging, ...events } = useLibraryFileDrop(imported);
  return <div {...events} data-dragging={dragging}><input /><div data-attachment onDrop={(event) => event.stopPropagation()} /></div>;
}
async function drag(type: string, files = true, target: Element = host.firstElementChild!) {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperty(event, "dataTransfer", { value: { types: files ? ["Files"] : ["text/plain"], files: files ? [new File(["pdf"], "paper.pdf")] : [] } });
  await act(async () => { target.dispatchEvent(event); }); return event;
}
const visible = () => host.firstElementChild!.getAttribute("data-dragging") === "true";
beforeEach(async () => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true); imported.mockClear();
  host = document.createElement("div"); document.body.append(host); root = createRoot(host);
  await act(async () => root.render(<Harness />));
});
afterEach(async () => { await act(async () => root.unmount()); host.remove(); vi.unstubAllGlobals(); });
it("preserves text drag defaults and only intercepts files", async () => {
  expect((await drag("dragover", false)).defaultPrevented).toBe(false);
  expect((await drag("drop", false)).defaultPrevented).toBe(false);
  await drag("dragenter", false); expect(visible()).toBe(false); expect(imported).not.toHaveBeenCalled();
  expect((await drag("dragover")).defaultPrevented).toBe(true);
});
it("counts nested enters and leaves without flickering", async () => {
  await drag("dragenter"); await drag("dragenter", true, host.querySelector("input")!);
  await drag("dragleave", true, host.querySelector("input")!); expect(visible()).toBe(true);
  await drag("dragleave"); expect(visible()).toBe(false);
});
it.each(["drop", "dragend", "blur"])("resets feedback after %s", async (event) => {
  await drag("dragenter");
  if (event === "drop") { await drag("drop"); expect(imported).toHaveBeenCalledOnce(); }
  else await act(async () => window.dispatchEvent(new Event(event)));
  expect(visible()).toBe(false);
});
it("lets attachment drops stop propagation while resetting the overlay", async () => {
  await drag("dragenter"); await drag("drop", true, host.querySelector("[data-attachment]")!);
  expect(imported).not.toHaveBeenCalled(); expect(visible()).toBe(false);
});
