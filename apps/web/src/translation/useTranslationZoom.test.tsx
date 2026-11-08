// @vitest-environment jsdom
import { act, useRef } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { useTranslationZoom } from "./useTranslationZoom";

let host: HTMLDivElement, root: Root;
let width: number;
let notify: () => void;
const rendered = vi.fn();
function Pane() {
  const pane = useRef<HTMLDivElement>(null);
  const zoom = useTranslationZoom(pane, true, () => true);
  rendered(zoom.width);
  return (
    <div ref={pane} data-pane>
      <div style={{ width: zoom.width, zoom: zoom.zoom }} />
    </div>
  );
}
beforeEach(async () => {
  vi.useFakeTimers();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(callback: () => void) {
        notify = callback;
      }
      observe() {}
      disconnect() {}
    },
  );
  width = 600;
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(
    () => width,
  );
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => root.render(<Pane />));
  rendered.mockClear();
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

it("commits translation wrapping once after native window resizing settles", async () => {
  const body = host.querySelector<HTMLElement>("[data-pane] > div")!;
  expect(body.style.width).toBe("568px");
  for (const next of [700, 800, 1000]) {
    width = next;
    await act(async () => {
      notify();
      await vi.advanceTimersByTimeAsync(32);
    });
    expect(body.style.width).toBe("568px");
  }
  expect(rendered).not.toHaveBeenCalled();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(160);
  });
  expect(body.style.width).toBe("968px");
  expect(rendered).toHaveBeenCalledExactlyOnceWith(968);
});

it("cancels a pending width change when the pane closes", async () => {
  width = 1000;
  await act(async () => {
    notify();
  });
  await act(async () => root.render(null));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(500);
  });
  expect(rendered).not.toHaveBeenCalled();
});

function scale() {
  return Number(
    host.querySelector<HTMLElement>("[data-pane] > div")!.style.zoom,
  );
}
async function key(target: EventTarget, extra: KeyboardEventInit = {}) {
  const event = new KeyboardEvent("keydown", {
    key: "+",
    metaKey: true,
    bubbles: true,
    cancelable: true,
    ...extra,
  });
  await act(async () => {
    target.dispatchEvent(event);
  });
  return event;
}
async function wheel(target: EventTarget, extra: WheelEventInit = {}) {
  const event = new WheelEvent("wheel", {
    ctrlKey: true,
    deltaY: -30,
    clientX: 120,
    clientY: 160,
    bubbles: true,
    cancelable: true,
    ...extra,
  });
  await act(async () => {
    target.dispatchEvent(event);
  });
  return event;
}
it.each([{ ctrlKey: true }, { ctrlKey: false, metaKey: true }])(
  "zooms modified wheel and pinch input %j around the pointer and keeps fit-width bounds",
  async (modifiers) => {
    const pane = host.querySelector<HTMLElement>("[data-pane]")!;
    pane.getBoundingClientRect = () => new DOMRect(20, 40, 600, 400);
    pane.scrollLeft = 50;
    pane.scrollTop = 100;
    expect((await wheel(pane, modifiers)).defaultPrevented).toBe(true);
    const ratio = Math.exp(0.1);
    expect(scale()).toBeCloseTo(ratio);
    expect(pane.scrollLeft).toBeCloseTo((50 + 100) * ratio - 100);
    expect(pane.scrollTop).toBeCloseTo((100 + 120) * ratio - 120);
    expect((await wheel(pane, { ctrlKey: false })).defaultPrevented).toBe(
      false,
    );
    expect(scale()).toBeCloseTo(ratio);
    await wheel(pane, { deltaY: -100000 });
    expect(scale()).toBe(5);
    await wheel(pane, { deltaY: 100000 });
    expect(scale()).toBe(1);
    await key(document.body, { key: "-" });
    expect(scale()).toBe(1);
  },
);
it.each([
  "<input>",
  "<textarea></textarea>",
  "<select></select>",
  "<div contenteditable><span>edit</span></div>",
  '<div contenteditable="plaintext-only"><span>edit</span></div>',
  '<div role="textbox"><span>edit</span></div>',
  '<div role="dialog"><span>dialog</span></div>',
  '<div role="alertdialog"><span>dialog</span></div>',
  "<dialog open><span>dialog</span></dialog>",
])("leaves keyboard and wheel input native inside %s", async (html) => {
  const pane = host.querySelector<HTMLElement>("[data-pane]")!;
  const wrapper = document.createElement("div");
  wrapper.innerHTML = html;
  pane.append(wrapper);
  const target = wrapper.querySelector("span") ?? wrapper.firstElementChild!;
  expect((await key(target)).defaultPrevented).toBe(false);
  expect((await wheel(target)).defaultPrevented).toBe(false);
  expect(scale()).toBe(1);
});
it("leaves body shortcuts and pane gestures native while a modal is open", async () => {
  const modal = document.createElement("div");
  modal.setAttribute("role", "dialog");
  modal.setAttribute("aria-modal", "true");
  host.append(modal);
  expect((await key(document.body)).defaultPrevented).toBe(false);
  expect(
    (await wheel(host.querySelector("[data-pane]")!)).defaultPrevented,
  ).toBe(false);
  expect(scale()).toBe(1);
});
it("does not reclaim previously consumed shortcuts or gestures", async () => {
  const pane = host.querySelector<HTMLElement>("[data-pane]")!;
  const shortcut = new KeyboardEvent("keydown", {
    key: "+",
    metaKey: true,
    bubbles: true,
    cancelable: true,
  });
  const gesture = new WheelEvent("wheel", {
    ctrlKey: true,
    deltaY: -30,
    bubbles: true,
    cancelable: true,
  });
  shortcut.preventDefault();
  gesture.preventDefault();
  await act(async () => {
    document.body.dispatchEvent(shortcut);
    pane.dispatchEvent(gesture);
  });
  expect(scale()).toBe(1);
});
