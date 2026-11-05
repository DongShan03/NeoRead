// @vitest-environment jsdom
import { act, type ComponentProps } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { defaultTheme, type Document } from "@neoread/core";
import { api } from "@neoread/api";
import { App } from "./App";
import { useReaderStore } from "./store";
import { paper } from "./papers/fixtures";
import { flushProgress } from "./progress";
import type { DocumentTabs } from "./DocumentTabs";
const fixture = vi.hoisted(() => ({
  documents: [] as Document[],
  tabs: undefined as ComponentProps<typeof DocumentTabs> | undefined,
  shortcuts: new Map<string, () => void>(),
}));
vi.mock("@neoread/api", () => ({
  RequestError: class extends Error {},
  api: {
    documents: vi.fn(async () => fixture.documents),
    libraryPreferences: vi.fn(async () => ({
      mode: "books",
      primaryMode: "books",
    })),
    saveLibraryPreferences: vi.fn(async (value) => value),
    trash: vi.fn(async () => []),
    settings: vi.fn(async () => ({ ...defaultTheme, documentTabs: true })),
    saveSettings: vi.fn(async () => {}),
    update: vi.fn(async () => ({})),
    processing: vi.fn(async () => []),
    connectorRevision: vi.fn(async () => ({ revision: 0 })),
  },
}));
vi.mock("./shortcuts/shortcuts", async (original) => ({
  ...(await original<typeof import("./shortcuts/shortcuts")>()),
  loadBindings: vi.fn(async () => {}),
  useShortcut: (id: string, action: () => void) => {
    fixture.shortcuts.set(id, action);
  },
}));
vi.mock("./progress", () => ({ flushProgress: vi.fn(async () => {}) }));
vi.mock("./ProcessingStatus", () => ({ useProcessing: () => ({ jobs: [] }) }));
vi.mock("./update-progress", () => ({ useUpdateProgress: vi.fn() }));
vi.mock("./appearance", async (original) => ({
  ...(await original<typeof import("./appearance")>()),
  useResolvedTheme: () => defaultTheme,
}));
vi.mock("./Settings", () => ({
  Settings: ({
    open,
    initialTab,
    onOpenChange,
  }: {
    open: boolean;
    initialTab: string;
    onOpenChange: (value: boolean) => void;
  }) =>
    open ? (
      <div role="dialog">
        {initialTab}
        <button onClick={() => onOpenChange(false)}>关闭设置</button>
      </div>
    ) : null,
}));
vi.mock("./DocumentTabs", () => ({
  DocumentTabs: (props: ComponentProps<typeof DocumentTabs>) => {
    fixture.tabs = props;
    return null;
  },
  ReadingWorkspaces: () => null,
}));
vi.mock("./guide/GuideHost", () => ({ GuideHost: () => null }));
vi.mock("./papers/PaperLibrary", () => ({ PaperLibrary: () => null }));
vi.mock("./papers/PaperSidebar", () => ({ PaperSidebar: () => null }));
vi.mock("./papers/ImportPaperDialog", () => ({
  ImportPaperDialog: () => null,
}));
vi.mock("./LibraryDocuments", () => ({
  LibraryDocuments: ({ documents }: { documents: Document[] }) =>
    documents.map((doc) => <article key={doc.id}>{doc.title}</article>),
}));
vi.mock("./DocumentManagement", () => ({
  LibraryFilterBar: () => null,
  LibraryTagFilters: () => null,
}));
vi.mock("./LargePaperDialog", () => ({
  LargePaperConfirmation: () => null,
  LargePaperDialog: () => null,
}));
let root: Root, host: HTMLDivElement;
const button = (text: string) =>
  [...document.querySelectorAll("button")].find(
    (el) => el.textContent?.trim() === text,
  )!;
beforeEach(async () => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.clearAllMocks();
  fixture.documents = [
    paper({ id: "a", title: "Alpha", library: "books", favorite: true }),
    paper({ id: "b", title: "Beta", library: "books" }),
  ];
  useReaderStore.setState({
    theme: defaultTheme,
    active: null,
    tabIds: [],
    visitedTabs: [],
    closedTabGroups: [],
    documents: [],
    trash: [],
    libraryPreferences: {},
    sessionReady: true,
  });
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => root.render(<App />));
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
it("switches existing tabs synchronously without consulting processing", async () => {
  const doc = paper();
  await act(async () => {
    fixture.tabs!.onSelect(doc);
    expect(useReaderStore.getState().active).toBe(doc);
  });
  expect(api.processing).not.toHaveBeenCalled();
  expect(api.update).toHaveBeenCalledWith(doc.id, {});
});
it("flushes progress before refreshing on library shortcuts", async () => {
  vi.mocked(api.documents).mockClear();
  await act(async () => fixture.shortcuts.get("primaryLibrary")!());
  expect(flushProgress).toHaveBeenCalledOnce();
  expect(api.documents).toHaveBeenCalledOnce();
  expect(vi.mocked(flushProgress).mock.invocationCallOrder[0]).toBeLessThan(
    vi.mocked(api.documents).mock.invocationCallOrder[0],
  );
});
it("resets normal settings openings after sync settings", async () => {
  await act(async () =>
    window.dispatchEvent(new Event("neoread:open-sync-settings")),
  );
  expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
    "papers",
  );
  await act(async () => button("关闭设置").click());
  await act(async () => button("设置").click());
  expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
    "agent",
  );
});
it("keeps matching card DOM nodes while typing", async () => {
  await act(async () => button("收藏1").click());
  const card = host.querySelector("article");
  const input = host.querySelector<HTMLInputElement>(
    '[aria-label="搜索书库"]',
  )!;
  const value = Object.getOwnPropertyDescriptor(
    HTMLInputElement.prototype,
    "value",
  )!.set!;
  await act(async () => {
    value.call(input, "Al");
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  expect(host.querySelector("article")).toBe(card);
  expect(host.querySelector("article")?.textContent).toBe("Alpha");
});
it("shows an actionable favorites empty state", async () => {
  await act(async () =>
    useReaderStore
      .getState()
      .setDocuments(
        fixture.documents.map((doc) => ({ ...doc, favorite: false })),
      ),
  );
  await act(async () => button("收藏0").click());
  const empty = host.querySelector(".empty-state")!;
  expect(empty.textContent).toContain("暂无收藏");
  expect(empty.querySelectorAll("button")).toHaveLength(1);
  expect(empty.textContent).toContain("显示全部图书");
  expect(host.querySelector('[aria-current="page"]')?.textContent).toContain(
    "收藏",
  );
});

it("imports directly from a truly empty book library", async () => {
  await act(async () => useReaderStore.getState().setDocuments([]));
  const click = vi.spyOn(host.querySelector<HTMLInputElement>('input[type="file"]')!, "click").mockImplementation(() => {});
  const button = [...host.querySelectorAll<HTMLButtonElement>(".empty-state button")].find((el) => el.textContent === "导入文档")!;
  await act(async () => button.click()); expect(click).toHaveBeenCalledOnce();
});

it("sends motion changes through the existing appearance bridge", async () => {
  const setAppearance = vi.fn(async () => {});
  vi.stubGlobal("neoreadDesktop", { setAppearance });
  await act(async () => useReaderStore.getState().setTheme({ reducedMotion: true }));
  expect(setAppearance).toHaveBeenLastCalledWith("system", expect.objectContaining({ reducedMotion: true }));
  await act(async () => useReaderStore.getState().setTheme({ reducedMotion: false }));
  expect(setAppearance).toHaveBeenLastCalledWith("system", expect.objectContaining({ reducedMotion: false }));
});
