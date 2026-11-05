// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  Dialog,
  DialogContent,
  DialogTitle,
} from "@neoread/ui/components/dialog";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogTitle,
} from "@neoread/ui/components/alert-dialog";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@neoread/ui/components/popover";
import { LinkPreviewPanel } from "./LinkPreviewPanel";
import { AnnotationNoteWindow } from "./AnnotationNoteWindow";
import { DetailSheet } from "./DetailSheet";
import { libraryShortcut } from "./library-shortcuts";
let root: Root, host: HTMLDivElement;
beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
const key = (key: string, options: KeyboardEventInit = {}) =>
  new KeyboardEvent("keydown", { key, metaKey: true, ...options });
it("respects the platform modifier and text input's Mac control keys", () => {
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(
    libraryShortcut(key("k", { metaKey: false, ctrlKey: true }), true),
  ).toBeNull();
  expect(
    libraryShortcut(key("o", { metaKey: false, ctrlKey: true }), false),
  ).toBe("import");
  expect(libraryShortcut(key("o"), false)).toBeNull();
});
it("ignores repeating, composing, prevented and additional modifier events", () => {
  for (const options of [
    { repeat: true },
    { isComposing: true },
    { altKey: true },
    { shiftKey: true },
  ])
    expect(libraryShortcut(key("k", options), true)).toBeNull();
  const event = key("k", { cancelable: true });
  event.preventDefault();
  expect(libraryShortcut(event, true)).toBeNull();
});
it("keeps find and import available with a real non-modal DetailSheet", async () => {
  await act(async () =>
    root.render(
      <DetailSheet
        open
        onClose={() => {}}
        container={host}
        label="图书详情"
        saveWidth={() => {}}
      >
        <p>详情内容</p>
      </DetailSheet>,
    ),
  );
  expect(host.querySelector('[role="dialog"]')?.getAttribute("data-slot")).toBe(
    "sheet-content",
  );
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("o"), true)).toBe("import");
});
it("keeps find and import available with a real Popover", async () => {
  await act(async () =>
    root.render(
      <Popover open>
        <PopoverTrigger>打开预览</PopoverTrigger>
        <PopoverContent aria-label="浮动预览">内容</PopoverContent>
      </Popover>,
    ),
  );
  expect(
    document.querySelector('[role="dialog"]')?.getAttribute("data-slot"),
  ).toBe("popover-content");
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("o"), true)).toBe("import");
});
it("blocks shortcuts for a settings-style modal Dialog", async () => {
  await act(async () =>
    root.render(
      <Dialog open>
        <DialogContent>
          <DialogTitle>设置</DialogTitle>
        </DialogContent>
      </Dialog>,
    ),
  );
  expect(
    document.querySelector('[role="dialog"]')?.getAttribute("data-slot"),
  ).toBe("dialog-content");
  expect(libraryShortcut(key("k"), true)).toBeNull();
  expect(libraryShortcut(key("o"), true)).toBeNull();
});
it("only permits closing quick find itself and respects a modal AlertDialog above it", async () => {
  const show = (alert = false) =>
    root.render(
      <>
        <Dialog open>
          <DialogContent data-quick-find>
            <DialogTitle>快速查找</DialogTitle>
          </DialogContent>
        </Dialog>
        {alert && (
          <AlertDialog open>
            <AlertDialogContent>
              <AlertDialogTitle>确认</AlertDialogTitle>
            </AlertDialogContent>
          </AlertDialog>
        )}
      </>,
    );
  await act(async () => show());
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("o"), true)).toBeNull();
  await act(async () => show(true));
  expect(
    document.querySelector('[role="alertdialog"]')?.getAttribute("data-slot"),
  ).toBe("alert-dialog-content");
  expect(libraryShortcut(key("k"), true)).toBeNull();
});

it("keeps shortcuts available with a pinned LinkPreviewPanel", async () => {
  await act(async () =>
    root.render(
      <LinkPreviewPanel
        preview={{
          anchor: { left: 100, top: 100, width: 20, height: 20 },
          page: 7,
          image: "/preview.png",
          ratio: 1.25,
          location: { type: "pdf", page: 7 },
        }}
        onDismiss={() => {}}
        onNavigate={() => {}}
      />,
    ),
  );
  const pin = document.querySelector<HTMLButtonElement>(
    'button[aria-label="固定预览"]',
  )!;
  await act(async () => pin.click());
  expect(
    document.querySelector(
      '[data-slot="dialog-content"][data-neoread-floating-preview]',
    ),
  ).not.toBeNull();
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("o"), true)).toBe("import");
});

it("keeps shortcuts available with a real non-modal annotation window", async () => {
  await act(async () =>
    root.render(
      <AnnotationNoteWindow
        open
        onOpenChange={() => {}}
        title="批注"
        description="编辑批注"
      >
        <textarea />
      </AnnotationNoteWindow>,
    ),
  );
  expect(
    document.querySelector(
      '[data-slot="dialog-content"].annotation-note-window',
    ),
  ).not.toBeNull();
  expect(libraryShortcut(key("k"), true)).toBe("find");
  expect(libraryShortcut(key("o"), true)).toBe("import");
});
