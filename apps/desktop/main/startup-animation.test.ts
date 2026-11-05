// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { createStartupHTML, startupAnimationScript } from "./startup-animation";

const assets = {
  fontDataURL: "data:font/ttf;base64,AA==",
  logoLightDataURL: "data:image/svg+xml;base64,AA==",
  logoDarkDataURL: "data:image/svg+xml;base64,BB==",
};
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function setup(reduced?: boolean) {
  const page = new DOMParser().parseFromString(createStartupHTML(assets), "text/html");
  document.body.innerHTML = page.body.innerHTML;
  if (reduced === undefined) delete document.documentElement.dataset.reducedMotion;
  else document.documentElement.dataset.reducedMotion = String(reduced);
  const media = Object.assign(new EventTarget(), { matches: reduced, media: "system-motion", onchange: null, addListener() {}, removeListener() {} });
  vi.stubGlobal("matchMedia", vi.fn(() => media));
  Object.defineProperty(document, "hidden", { configurable: true, value: false });
  const animations: { pause: ReturnType<typeof vi.fn>; play: ReturnType<typeof vi.fn>; cancel: ReturnType<typeof vi.fn> }[] = [];
  const animate = vi.fn(() => {
    const animation = { pause: vi.fn(), play: vi.fn(), cancel: vi.fn(), finished: new Promise(() => {}) };
    animations.push(animation);
    return animation as unknown as Animation;
  });
  vi.stubGlobal("Animation", class {});
  Object.defineProperty(Element.prototype, "animate", { configurable: true, writable: true, value: animate });
  // Execute the same trusted script embedded into the desktop page.
  window.eval(startupAnimationScript);
  return { animations, media, animate };
}

it("embeds local branding and leaves readable feedback without JavaScript", () => {
  const html = createStartupHTML(assets);
  const page = new DOMParser().parseFromString(html, "text/html");
  expect(page.body.textContent).toContain("NeoRead");
  expect(page.querySelector('[role="status"]')?.textContent).toContain("正在启动");
  expect(html).toContain(assets.fontDataURL);
  expect(html).toContain(assets.logoLightDataURL);
  expect(html).toContain(assets.logoDarkDataURL);
  expect(page.querySelector("script")?.textContent).toBe(startupAnimationScript);
  expect(page.querySelectorAll("*").length).toBeLessThan(45);
});

it("pauses when hidden, resumes when visible and releases animations on page exit", () => {
  const { animations } = setup();
  expect(animations.length).toBeGreaterThan(0);
  Object.defineProperty(document, "hidden", { configurable: true, value: true });
  document.dispatchEvent(new Event("visibilitychange"));
  expect(animations.every(a => a.pause.mock.calls.length > 0)).toBe(true);
  Object.defineProperty(document, "hidden", { configurable: true, value: false });
  document.dispatchEvent(new Event("visibilitychange"));
  expect(animations.every(a => a.play.mock.calls.length > 0)).toBe(true);
  window.dispatchEvent(new Event("pagehide"));
  expect(animations.every(a => a.cancel.mock.calls.length > 0)).toBe(true);
  Object.defineProperty(document, "hidden", { configurable: true, value: true });
  document.dispatchEvent(new Event("visibilitychange"));
  expect(animations.every(a => a.pause.mock.calls.length === 1)).toBe(true);
});

it.each([undefined, false, true])("uses only the app motion marker (%s)", (reduced) => {
  const state = setup(reduced);
  expect(state.animate.mock.calls.length > 0).toBe(reduced !== true);
  expect(matchMedia).not.toHaveBeenCalled();
  const before = state.animations.length;
  state.media.matches = !state.media.matches;
  state.media.dispatchEvent(new Event("change"));
  expect(state.animations).toHaveLength(before);
  expect(state.animations.every((a) => a.cancel.mock.calls.length === 0)).toBe(true);
  window.dispatchEvent(new Event("pagehide"));
});
