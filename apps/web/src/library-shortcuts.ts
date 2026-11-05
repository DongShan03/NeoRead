import { bindingFromEvent, isMac } from "./shortcuts/shortcuts";

export function libraryShortcut(event: KeyboardEvent, mac = isMac()) {
  if (event.defaultPrevented || event.repeat || event.isComposing) return null;
  const binding = bindingFromEvent(event, mac);
  if (!binding || binding.shift) return null;
  const dialogs = [
    ...document.querySelectorAll(
      '[data-slot="dialog-content"]:not([data-neoread-floating-preview]):not(.annotation-note-window), [data-slot="alert-dialog-content"]',
    ),
  ];
  if (binding.key === "o") return dialogs.length ? null : "import";
  if (
    binding.key === "k" &&
    dialogs.every((dialog) => dialog.hasAttribute("data-quick-find"))
  )
    return "find";
  return null;
}
