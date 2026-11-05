// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { libraryShortcut } from "./library-shortcuts";
afterEach(() => { document.body.innerHTML = ""; });
const key = (key: string, options: KeyboardEventInit = {}) =>
  new KeyboardEvent("keydown", { key, metaKey: true, ...options });
it("respects the platform modifier and text input's Mac control keys", () => {
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("k", { metaKey: false, ctrlKey: true }), true)).toBeNull();
  expect(libraryShortcut(key("o", { metaKey: false, ctrlKey: true }), false)).toBe("import");
  expect(libraryShortcut(key("o"), false)).toBeNull();
});
it("ignores repeating, composing, prevented and additional modifier events", () => {
  for (const options of [{ repeat: true }, { isComposing: true }, { altKey: true }, { shiftKey: true }])
    expect(libraryShortcut(key("k", options), true)).toBeNull();
  const event = key("k", { cancelable: true }); event.preventDefault();
  expect(libraryShortcut(event, true)).toBeNull();
});
it("only permits closing quick find while its dialog is open", () => {
  const dialog = document.createElement("div"); dialog.role = "dialog"; document.body.append(dialog);
  expect(libraryShortcut(key("k"), true)).toBeNull();
  expect(libraryShortcut(key("o"), true)).toBeNull();
  dialog.setAttribute("data-quick-find", "");
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("o"), true)).toBeNull();
  const alert = document.createElement("div"); alert.role = "alertdialog"; document.body.append(alert);
  expect(libraryShortcut(key("k"), true)).toBeNull();
});
