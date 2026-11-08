// @vitest-environment jsdom
import { act, type ReactNode, type ComponentProps } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  defaultTheme,
  type Message,
  type Processing,
  type ReaderAdapter,
  type ReaderEvents,
  type Annotation,
  type ReaderSelection,
  type SearchResult,
} from "@neoread/core";
import { toast } from "sonner";
import { TooltipProvider } from "@neoread/ui/components/tooltip";
import { api } from "@neoread/api";
import { Workspace } from "./Workspace";
import { paper } from "./papers/fixtures";
import { useReaderStore } from "./store";
import { ChatSession } from "./chat/chat-session";
import * as sessionRegistry from "./chat/session-registry";
import type { ChatStreamEvent } from "@neoread/api";
import { tabCloseWarning } from "./document-tab-guards";
import { installMotionPreference } from "./motion";
import type { AnnotationNoteForm } from "./AnnotationNoteForm";
import type { SessionPicker } from "./chat/SessionPicker";
import type { ResizablePanelGroup } from "@neoread/ui/components/resizable";
import type { AssistantPanel } from "./chat/AssistantPanel";
type GroupProps = ComponentProps<typeof ResizablePanelGroup>;

const panels = vi.hoisted(() => ({
  props: undefined as GroupProps | undefined,
  readerEvents: undefined as ReaderEvents | undefined,
  workspaceRenders: 0,
  noteRenders: 0,
  noteFormMounts: 0,
  noteFormUnmounts: 0,
  inputRenders: 0,
  pageNavigationVisible: true,
  readerSearch: undefined as
    { results: SearchResult[]; query: string } | undefined,
  readerRenders: 0,
  assistantRenders: 0,
  sessions: undefined as ComponentProps<typeof SessionPicker> | undefined,
  readerReady: undefined as ((adapter: ReaderAdapter) => void) | undefined,
  assistant: undefined as ComponentProps<typeof AssistantPanel> | undefined,
}));

vi.mock("sonner", () => ({ toast: { info: vi.fn(), error: vi.fn() } }));

vi.mock("@neoread/api", () => ({
  api: {
    aiConfig: vi.fn(async () => ({
      primary: "",
      models: {},
      capabilities: {},
    })),
    recoverAnnotations: vi.fn(async () => ({ checked: true })),
    annotations: vi.fn(async () => []),
    annotate: vi.fn(),
    updateAnnotationNote: vi.fn(),
    updateAnnotation: vi.fn(),
    annotationStream: vi.fn(async (_id, signal, callback) => {
      callback("revision");
      await new Promise<void>((resolve) =>
        signal.addEventListener("abort", () => resolve(), { once: true }),
      );
    }),
    navigation: vi.fn(async () => ({ revision: 0, entries: {} })),
    messages: vi.fn(async () => []),
    chatSessions: vi.fn(async () => []),
    sessionMessages: vi.fn(async () => []),
    startChatRun: vi.fn(async () => ({ id: "run", status: "running" })),
    blocks: vi.fn(async () => []),
    documentNote: vi.fn(async () => ({ body: "note", updatedAt: "" })),
    translations: vi.fn(async () => []),
  },
  setDiagnosticReporter: vi.fn(),
  reportDiagnostic: vi.fn(),
  watchChatRun: vi.fn(async (_doc, _session, _run, _signal, emit) => {
    emit({ event: "done", data: { ok: true } });
  }),
  blockImageURL: vi.fn(),
  publicationURL: vi.fn(),
}));
vi.mock("./progress", () => ({
  scheduleProgress: vi.fn(),
  flushProgress: vi.fn(async () => {}),
}));
vi.mock("@neoread/ui/components/scroll-area", () => ({
  ScrollArea: ({ children }: { children: ReactNode }) => <div>{children}</div>,
}));
vi.mock("./AnnotationNoteForm", async (importOriginal) => {
  const original =
    await importOriginal<typeof import("./AnnotationNoteForm")>();
  const { useEffect } = await import("react");
  return {
    AnnotationNoteForm: (props: ComponentProps<typeof AnnotationNoteForm>) => {
      useEffect(() => {
        panels.noteFormMounts++;
        return () => {
          panels.noteFormUnmounts++;
        };
      }, []);
      return <original.AnnotationNoteForm {...props} />;
    },
  };
});
vi.mock("@neoread/ui/components/input", async (importOriginal) => {
  const original =
    await importOriginal<typeof import("@neoread/ui/components/input")>();
  return {
    Input: (props: ComponentProps<typeof original.Input>) => {
      panels.inputRenders++;
      return <original.Input {...props} />;
    },
  };
});
vi.mock("@neoread/ui/components/textarea", async (importOriginal) => {
  const original =
    await importOriginal<typeof import("@neoread/ui/components/textarea")>();
  return {
    Textarea: (props: ComponentProps<typeof original.Textarea>) => {
      if (
        props.placeholder === "批注内容…" ||
        props.placeholder === "想弄清楚什么？"
      )
        panels.noteRenders++;
      return <original.Textarea {...props} />;
    },
  };
});
vi.mock("./translation/PDFReadingView", () => ({
  PDFReadingView: ({
    onReady,
    events,
    pageNavigation,
    searchResults = [],
    searchQuery = "",
  }: {
    onReady: (adapter: ReaderAdapter, items: []) => void;
    events: ReaderEvents;
    pageNavigation: ReactNode;
    searchResults?: SearchResult[];
    searchQuery?: string;
  }) => {
    panels.readerRenders++;
    panels.readerReady = (adapter) => onReady(adapter, []);
    panels.readerEvents = events;
    panels.readerSearch = { results: searchResults, query: searchQuery };
    return <>{panels.pageNavigationVisible && pageNavigation}</>;
  },
}));
vi.mock("./translation/EPUBReadingView", async () => {
  const { PDFReadingView } = await import("./translation/PDFReadingView");
  return { EPUBReadingView: PDFReadingView };
});
vi.mock("./chat/AssistantPanel", () => ({
  AssistantPanel: (props: ComponentProps<typeof AssistantPanel>) => {
    panels.assistantRenders++;
    panels.assistant = props;
    return null;
  },
}));
vi.mock("./chat/SessionPicker", () => ({
  SessionPicker: (props: ComponentProps<typeof SessionPicker>) => {
    panels.sessions = props;
    return null;
  },
}));
vi.mock("./ModelSelector", () => ({ ModelSelector: () => null }));
vi.mock("./DocumentOverview", () => ({ DocumentOverviewDialog: () => null }));
vi.mock("./ProcessingUsage", () => ({ ProcessingUsageDialog: () => null }));
vi.mock("./annotated-pdf-export", () => ({
  exportAnnotatedPDFWithToast: vi.fn(),
}));
vi.mock("./PaperNote", () => ({
  PaperNote: ({ active }: { active: boolean }) => (
    <div data-testid="paper-note" data-active={active} />
  ),
}));
vi.mock("./NotesPanel", () => ({
  NotesPanel: ({ onExport }: { onExport: () => void }) => (
    <button onClick={onExport}>导出</button>
  ),
}));
vi.mock("@neoread/ui/components/resizable", async () => {
  const { useRef } = await import("react");
  const Panel = ({ children }: { children?: ReactNode }) => (
    <div>{children}</div>
  );
  return {
    ResizablePanel: Panel,
    ResizablePanelGroup: (props: GroupProps) => {
      panels.workspaceRenders++;
      panels.props = props;
      return <Panel>{props.children}</Panel>;
    },
    ResizableHandle: () => null,
    usePanelRef: () => useRef(null),
  };
});

const doc = paper({ id: "export-doc", title: "Export" });
const processing: Processing = {
  documentId: doc.id,
  phase: "ready",
  status: "complete",
  pagesDone: 1,
  pagesTotal: 1,
  translationsDone: 1,
  translationsTotal: 1,
  detail: "",
  updatedAt: "",
};
let root: Root, host: HTMLDivElement;
const render = (
  tick: number,
  active = true,
  type: "pdf" | "epub" = "pdf",
  id = doc.id,
) =>
  root.render(
    <TooltipProvider>
      <Workspace
        active={active}
        document={{ ...doc, type, id }}
        theme={defaultTheme}
        processing={{ ...processing, updatedAt: String(tick) }}
        onBack={() => {}}
        onSettings={() => {}}
      />
    </TooltipProvider>,
  );
const button = (text: string) =>
  [...document.querySelectorAll("button")].find(
    (el) => el.textContent?.trim() === text,
  )!;

it("keeps a non-modal annotation draft open while reading elsewhere and saves against its original selection", async () => {
  await act(async () => render(0));
  const selected = {
    text: "Original sentence.",
    location: { type: "pdf" as const, page: 1, y: 0.2 },
    anchor: { x: 30, top: 20, bottom: 40, bounds: { right: 300, bottom: 180 } },
  };
  await act(async () => panels.readerEvents!.selection(selected));
  await act(async () =>
    host
      .querySelector<HTMLButtonElement>(
        '.selection-bar [aria-label="添加批注"]',
      )!
      .click(),
  );
  const dialog = document.querySelector('[role="dialog"]')!;
  expect((dialog as HTMLElement).style.left).toBe("308px");
  expect((dialog as HTMLElement).style.top).toBe("188px");
  expect(dialog.getAttribute("aria-modal")).not.toBe("true");
  expect(document.querySelector('[data-slot="dialog-overlay"]')).toBeNull();
  const field = dialog.querySelector<HTMLTextAreaElement>("textarea")!;
  expect(document.activeElement).toBe(field);
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLTextAreaElement.prototype,
      "value",
    )!.set!.call(field, "Keep this draft");
    field.dispatchEvent(new Event("input", { bubbles: true }));
  });
  const handle = dialog.querySelector<HTMLButtonElement>(
    '[aria-label="移动批注窗口"]',
  )!;
  handle.setPointerCapture = vi.fn();
  await act(async () =>
    handle.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        button: 0,
        clientX: 20,
        clientY: 20,
      }),
    ),
  );
  const readerRenders = panels.readerRenders;
  const frames = new Map<number, FrameRequestCallback>();
  let frameId = 0;
  const schedule = vi
    .spyOn(window, "requestAnimationFrame")
    .mockImplementation((callback) => {
      frames.set(++frameId, callback);
      return frameId;
    });
  vi.spyOn(window, "cancelAnimationFrame").mockImplementation((id) => {
    frames.delete(id);
  });
  const measure = vi.spyOn(dialog, "getBoundingClientRect");
  await act(async () => {
    for (let i = 0; i < 100; i++)
      handle.dispatchEvent(
        new MouseEvent("pointermove", {
          bubbles: true,
          clientX: 20 + i,
          clientY: 20 + i / 2,
        }),
      );
    handle.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: 80,
        clientY: 60,
      }),
    );
  });
  expect(schedule).toHaveBeenCalledOnce();
  expect((dialog as HTMLElement).style.left).toBe("308px");
  expect((dialog as HTMLElement).style.top).toBe("188px");
  expect(measure).not.toHaveBeenCalled();
  await act(async () => {
    const callback = [...frames.values()][0];
    frames.clear();
    callback(performance.now());
  });
  expect((dialog as HTMLElement).style.transform).toBe(
    "translate3d(60px, 40px, 0)",
  );
  expect(panels.readerRenders).toBe(readerRenders);
  await act(async () => {
    handle.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: 80,
        clientY: 60,
      }),
    );
    handle.dispatchEvent(
      new MouseEvent("pointerup", { bubbles: true, clientX: 80, clientY: 60 }),
    );
    handle.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: 300,
        clientY: 300,
      }),
    );
  });
  expect((dialog as HTMLElement).style.left).toBe("368px");
  expect((dialog as HTMLElement).style.top).toBe("228px");
  expect((dialog as HTMLElement).style.transform).toBe("");
  expect(frames.size).toBe(0);
  schedule.mockRestore();
  const outside = document.createElement("button");
  const clicked = vi.fn();
  outside.addEventListener("click", clicked);
  host.append(outside);
  await act(async () => {
    outside.focus();
    outside.dispatchEvent(
      new MouseEvent("pointerdown", { bubbles: true, button: 0 }),
    );
    outside.click();
    panels.readerEvents!.selection({
      ...selected,
      text: "Another sentence.",
      location: { type: "pdf", page: 2 },
    });
  });
  expect(clicked).toHaveBeenCalledOnce();
  expect(document.activeElement).toBe(outside);
  expect(document.querySelector('[role="dialog"] textarea')).toBe(field);
  expect(field.value).toBe("Keep this draft");
  vi.mocked(api.annotate).mockResolvedValueOnce({
    id: "new-note",
    documentId: doc.id,
    kind: "note",
    quote: selected.text,
    location: selected.location,
    note: "Keep this draft",
    color: "#e6b94c",
    createdAt: "2026-11-04",
  });
  await act(async () => button("保存批注").click());
  expect(api.annotate).toHaveBeenCalledWith(
    doc.id,
    expect.objectContaining({
      quote: selected.text,
      location: selected.location,
      note: "Keep this draft",
    }),
  );
  expect(document.querySelector('[role="dialog"]')).toBeNull();
});

it.each([
  ["pdf", "添加批注"],
  ["pdf", "提问"],
  ["epub", "添加批注"],
  ["epub", "提问"],
] as const)(
  "keeps five %s %s keystrokes inside the note form",
  async (type, action) => {
    vi.useFakeTimers();
    await act(async () => render(0, true, type));
    await act(async () => vi.advanceTimersByTimeAsync(100));
    await act(async () =>
      panels.readerEvents!.selection({
        text: "Original sentence.",
        location:
          type === "pdf"
            ? { type: "pdf", page: 1 }
            : { type: "epub", href: "chapter.xhtml" },
        anchor: { x: 30, top: 20, bottom: 40 },
      }),
    );
    await act(async () =>
      host
        .querySelector<HTMLButtonElement>(
          `.selection-bar [aria-label="${action}"]`,
        )!
        .click(),
    );
    const field = document.querySelector<HTMLTextAreaElement>(
      '[role="dialog"] textarea',
    )!;
    const before = {
      workspace: panels.workspaceRenders,
      reader: panels.readerRenders,
      assistant: panels.assistantRenders,
      note: panels.noteRenders,
    };
    for (const text of ["一", "一二", "一二三", "一二三四", "一二三四五"]) {
      await act(async () => {
        Object.getOwnPropertyDescriptor(
          HTMLTextAreaElement.prototype,
          "value",
        )!.set!.call(field, text);
        field.dispatchEvent(new Event("input", { bubbles: true }));
      });
    }
    expect({
      workspace: panels.workspaceRenders - before.workspace,
      reader: panels.readerRenders - before.reader,
      assistant: panels.assistantRenders - before.assistant,
      note: panels.noteRenders - before.note,
    }).toEqual({ workspace: 0, reader: 0, assistant: 0, note: 5 });
    expect(field.value).toBe("一二三四五");
    expect(tabCloseWarning(doc.id)).not.toBeNull();
  },
);

it.each(["搜索文档", "跳转 PDF 页码"])(
  "keeps five %s keystrokes out of workspace and reader renders",
  async (label) => {
    vi.useFakeTimers();
    await act(async () => render(0));
    if (label === "搜索文档")
      await act(async () =>
        [...host.querySelectorAll<HTMLButtonElement>('[role="tab"]')]
          .find((tab) => tab.textContent === "搜索")!
          .click(),
      );
    await act(async () => vi.advanceTimersByTimeAsync(100));
    const field = host.querySelector<HTMLInputElement>(
      `input[aria-label="${label}"]`,
    )!;
    const before = {
      workspace: panels.workspaceRenders,
      reader: panels.readerRenders,
      input: panels.inputRenders,
    };
    for (const text of ["1", "12", "123", "1234", "12345"]) {
      await act(async () => {
        Object.getOwnPropertyDescriptor(
          HTMLInputElement.prototype,
          "value",
        )!.set!.call(field, text);
        field.dispatchEvent(new Event("input", { bubbles: true }));
      });
    }
    expect({
      workspace: panels.workspaceRenders - before.workspace,
      reader: panels.readerRenders - before.reader,
      input: panels.inputRenders - before.input,
    }).toEqual({ workspace: 0, reader: 0, input: 5 });
  },
);

it("keeps newer model settings when the initial config response arrives late", async () => {
  let finish!: (config: Awaited<ReturnType<typeof api.aiConfig>>) => void;
  vi.mocked(api.aiConfig).mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  await act(async () => render(0));
  const newer: Awaited<ReturnType<typeof api.aiConfig>> = {
    primary: "claude",
    models: { claude: "new-model" },
    capabilities: {},
    api: { url: "", model: "", hasKey: false },
    textAPI: { url: "", model: "", hasKey: false },
    imageAPI: { url: "", model: "", hasKey: false },
  };
  await act(async () => useReaderStore.getState().setAIConfig(newer));
  await act(async () =>
    finish({
      ...newer,
      primary: "codex",
      models: { codex: "old-model" },
    }),
  );
  expect(useReaderStore.getState().aiConfig).toBe(newer);
});

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.clearAllMocks();
  panels.workspaceRenders = panels.noteRenders = panels.inputRenders = 0;
  panels.noteFormMounts = panels.noteFormUnmounts = 0;
  panels.pageNavigationVisible = true;
  panels.readerRenders = panels.assistantRenders = 0;
  vi.mocked(api.annotate).mockReset();
  vi.mocked(api.updateAnnotationNote).mockReset();
  vi.mocked(api.updateAnnotation).mockReset();
  vi.mocked(api.annotations).mockReset().mockResolvedValue([]);
  vi.mocked(api.chatSessions).mockReset().mockResolvedValue([]);
  vi.mocked(api.aiConfig)
    .mockReset()
    .mockResolvedValue({ primary: "", models: {}, capabilities: {} } as Awaited<
      ReturnType<typeof api.aiConfig>
    >);
  useReaderStore.setState({ theme: defaultTheme });
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal("PointerEvent", MouseEvent);
  vi.stubGlobal("localStorage", { getItem: () => null, setItem: vi.fn() });
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});

it("updates the active outline heading when reading moves within one PDF page", async () => {
  vi.useFakeTimers();
  vi.mocked(api.blocks).mockResolvedValueOnce(
    [0.1, 0.7].map((y, i) => ({
      id: `heading-${i}`,
      page: 1,
      label: "paragraph_title",
      text: `Section ${i + 1}`,
      bounds: { x: 0.1, y, width: 0.8, height: 0.05 },
    })),
  );
  await act(async () => render(0));
  // Settle the initial annotation snapshot before measuring scroll renders.
  await act(async () => vi.advanceTimersByTimeAsync(100));
  const heading = (name: string) =>
    host.querySelector<HTMLElement>(`[aria-label="目录：${name}"]`)!;
  await act(async () =>
    panels.readerEvents!.location(
      { type: "pdf", page: 1, x: 0.1, y: 0.12 },
      0.5,
    ),
  );
  expect(heading("Section 1").classList.contains("selected")).toBe(true);
  const renders = panels.readerRenders;
  for (let i = 0; i < 30; i++)
    await act(async () =>
      panels.readerEvents!.location(
        { type: "pdf", page: 1, x: 0.1, y: 0.15 + i * 0.019 },
        0.5,
      ),
    );
  expect(panels.readerRenders).toBe(renders);
  await act(async () =>
    panels.readerEvents!.location(
      { type: "pdf", page: 1, x: 0.1, y: 0.72 },
      0.5,
    ),
  );
  expect(heading("Section 2").classList.contains("selected")).toBe(true);
  expect(heading("Section 1").classList.contains("selected")).toBe(false);
});

it("dismisses stale selection toolbars on collapse or outside clicks while keeping toolbar actions usable", async () => {
  await act(async () => render(0));
  const text = document.createElement("p");
  text.textContent = "selected reading text";
  host.append(text);
  const select = async () => {
    const range = document.createRange();
    range.selectNodeContents(text);
    window.getSelection()!.removeAllRanges();
    window.getSelection()!.addRange(range);
    await act(async () =>
      panels.readerEvents!.selection({
        text: text.textContent!,
        location: { type: "pdf", page: 1 },
        anchor: { x: 30, top: 20, bottom: 40 },
      }),
    );
    expect(host.querySelector(".selection-bar")).not.toBeNull();
  };
  await select();
  await act(async () =>
    host
      .querySelector(".selection-bar button")!
      .dispatchEvent(
        new MouseEvent("pointerdown", { bubbles: true, button: 0 }),
      ),
  );
  expect(host.querySelector(".selection-bar")).not.toBeNull();
  await act(async () => {
    window.getSelection()!.collapseToStart();
    document.dispatchEvent(new Event("selectionchange"));
  });
  expect(host.querySelector(".selection-bar")).toBeNull();
  await select();
  await act(async () =>
    document.body.dispatchEvent(
      new MouseEvent("pointerdown", { bubbles: true, button: 0 }),
    ),
  );
  expect(host.querySelector(".selection-bar")).toBeNull();
  await select();
  await act(async () => window.dispatchEvent(new Event("blur")));
  expect(host.querySelector(".selection-bar")).toBeNull();
  window.getSelection()!.removeAllRanges();
  text.remove();
});

it("restores user sidebar sizes and collapsed state without replacing expanded sizes with zero", async () => {
  function NeoRead() {
    const theme = useReaderStore((state) => state.theme);
    return (
      <TooltipProvider>
        <Workspace
          document={doc}
          theme={theme}
          onBack={() => {}}
          onSettings={() => {}}
        />
      </TooltipProvider>
    );
  }
  await act(async () => root.render(<NeoRead />));
  const change = (
    navigation: number,
    assistant: number,
    isUserInteraction: boolean,
  ) =>
    panels.props!.onLayoutChanged!(
      { navigation, assistant, reading: 100 - navigation - assistant },
      { isUserInteraction },
    );
  await act(async () => change(20, 32, true));
  await act(async () => change(0, 32, true));
  await act(async () => change(0, 40, false));
  expect(useReaderStore.getState().theme.readerLayout).toMatchObject({
    navigationWidth: 20,
    assistantWidth: 32,
    navigationOpen: false,
  });
  const saved = JSON.parse(JSON.stringify(useReaderStore.getState().theme));
  await act(async () => root.unmount());
  useReaderStore.setState({ theme: saved });
  root = createRoot(host);
  await act(async () => root.render(<NeoRead />));
  expect(panels.props!.defaultLayout).toEqual({
    navigation: 0,
    reading: 68,
    assistant: 32,
  });
  await act(async () =>
    host.querySelector<HTMLButtonElement>('[aria-label="目录与搜索"]')!.click(),
  );
  await act(async () =>
    host
      .querySelector<HTMLButtonElement>('[aria-label="Agent 与批注"]')!
      .click(),
  );
  expect(useReaderStore.getState().theme.readerLayout).toMatchObject({
    navigationWidth: 20,
    assistantWidth: 32,
    navigationOpen: true,
    assistantOpen: false,
  });
});
afterEach(async () => {
  await act(async () => root.unmount());
  vi.useRealTimers();
  host.remove();
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it.each(["取消", "Close"])(
  "keeps one export dialog across progress refreshes and removes it with %s",
  async (close) => {
    await act(async () => render(0));
    await act(async () =>
      [...document.querySelectorAll<HTMLElement>('[role="tab"]')]
        .find((el) => el.textContent?.startsWith("批注"))!
        .click(),
    );
    await act(async () => button("导出").click());
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(1);
    const dialog = document.querySelector('[role="dialog"]');
    for (let tick = 1; tick <= 3; tick++) await act(async () => render(tick));
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(1);
    expect(document.querySelector('[role="dialog"]')).toBe(dialog);
    await act(async () => button(close).click());
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(0);
    await act(async () => button("导出").click());
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(1);
  },
);

it("loads saved annotations without running recovery on open or progress updates", async () => {
  await act(async () => render(0));
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 60));
  });
  expect(api.annotations).toHaveBeenCalledWith(doc.id, expect.any(AbortSignal));
  expect(api.recoverAnnotations).not.toHaveBeenCalled();
  await act(async () => render(1));
  expect(api.recoverAnnotations).not.toHaveBeenCalled();
});

it("activates the independent note tab separately from annotations", async () => {
  await act(async () => render(0));
  const note = () => host.querySelector('[data-testid="paper-note"]');
  expect(note()?.getAttribute("data-active")).toBe("false");
  const tab = (name: string) =>
    [...host.querySelectorAll<HTMLElement>('[role="tab"]')].find(
      (el) => el.textContent === name,
    )!;
  await act(async () => tab("笔记").click());
  expect(note()?.getAttribute("data-active")).toBe("true");
  await act(async () => tab("批注").click());
  expect(note()?.getAttribute("data-active")).toBe("false");
});

it.each(["a".repeat(9000), "中".repeat(3000)])(
  "keeps a withdrawal draft that exceeds the UTF-8 byte limit",
  async (content) => {
    vi.mocked(api.aiConfig).mockResolvedValueOnce({
      primary: "api",
      models: {},
      capabilities: {},
    } as Awaited<ReturnType<typeof api.aiConfig>>);
    await act(async () => render(0));
    await act(async () => panels.readerReady?.({} as ReaderAdapter));
    const original: Message = {
      id: "original",
      documentId: doc.id,
      role: "user",
      content,
      createdAt: "2026-10-15",
    };
    await act(async () => panels.assistant?.onWithdraw?.(original));
    await act(async () =>
      panels.assistant?.onWithdraw?.({ ...original, id: "withdrawn" }),
    );
    await act(async () =>
      host.querySelector<HTMLButtonElement>('[aria-label="发送问题"]')!.click(),
    );
    expect(toast.info).toHaveBeenCalledWith(
      "问题最多 16000 字节，请缩短后再发送",
    );
    expect(
      host.querySelector<HTMLTextAreaElement>(
        'textarea[aria-label="向 AI 提问"]',
      )!.value,
    ).toBe(`${content}\n\n${content}`);
  },
);

it("keeps a merged withdrawal draft when it exceeds the image limit", async () => {
  vi.mocked(api.aiConfig).mockResolvedValueOnce({
    primary: "api",
    models: {},
    capabilities: {},
  } as Awaited<ReturnType<typeof api.aiConfig>>);
  await act(async () => render(0));
  await act(async () => panels.readerReady?.({} as ReaderAdapter));
  const original: Message = {
    id: "draft",
    documentId: doc.id,
    role: "user",
    content: "draft question",
    createdAt: "2026-10-15",
    attachments: Array.from({ length: 4 }, (_, i) => ({
      id: `image-${i}`,
      page: 1,
      label: "image",
      caption: "",
    })),
  };
  await act(async () => panels.assistant?.onWithdraw?.(original));
  await act(async () =>
    panels.assistant?.onWithdraw?.({
      ...original,
      id: "withdrawn",
      content: "withdrawn question",
      attachments: [
        { id: "image-extra", page: 2, label: "image", caption: "" },
      ],
    }),
  );
  const composer = host.querySelector(
    'textarea[aria-label="向 AI 提问"]',
  ) as HTMLTextAreaElement;
  const send = host.querySelector(
    '[aria-label="发送问题"]',
  ) as HTMLButtonElement;
  expect(send.disabled).toBe(false);
  await act(async () => send.click());
  expect(toast.info).toHaveBeenCalledWith(
    "每次最多附加 4 张图片，请移除多余图片后再发送",
  );
  expect(composer.value).toBe("draft question\n\nwithdrawn question");
  expect(host.querySelectorAll('[aria-label^="移除附件："]')).toHaveLength(5);
});

const composer = () =>
  host.querySelector<HTMLTextAreaElement>('textarea[aria-label="向 AI 提问"]')!;
const enterPrompt = async (text: string) => {
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLTextAreaElement.prototype,
      "value",
    )!.set!.call(composer(), text);
    composer().dispatchEvent(new Event("input", { bubbles: true }));
  });
};

it("keeps typing local and preserves the latest text across workspace updates", async () => {
  vi.useFakeTimers();
  vi.mocked(api.chatSessions).mockResolvedValue([
    {
      id: "typing",
      documentId: doc.id,
      title: "test",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    },
  ]);
  vi.mocked(api.aiConfig).mockResolvedValueOnce({
    primary: "api",
    models: {},
    capabilities: {},
  } as Awaited<ReturnType<typeof api.aiConfig>>);
  await act(async () => render(0));
  await act(async () =>
    panels.readerReady?.({
      getLocation: () => undefined,
    } as unknown as ReaderAdapter),
  );
  await act(async () => vi.advanceTimersByTimeAsync(100));
  expect(api.annotations).toHaveBeenCalledOnce();
  const send = host.querySelector<HTMLButtonElement>(
    '[aria-label="发送问题"]',
  )!;
  expect(send.disabled).toBe(true);
  const emptyRenders = panels.readerRenders;
  await enterPrompt("第");
  expect(panels.readerRenders).toBe(emptyRenders);
  expect(send.disabled).toBe(false);
  expect(tabCloseWarning(doc.id)).not.toBeNull();
  const readerRenders = panels.readerRenders,
    assistantRenders = panels.assistantRenders;
  for (const text of ["第二", "第二个问题", "第二个问题\n补充条件"])
    await enterPrompt(text);
  expect(panels.readerRenders).toBe(readerRenders);
  expect(panels.assistantRenders).toBe(assistantRenders);
  await act(async () => render(1));
  expect(composer().value).toBe("第二个问题\n补充条件");
  const sent = vi
    .spyOn(ChatSession.prototype, "send")
    .mockResolvedValue(undefined);
  await act(async () => send.click());
  expect(sent).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({ prompt: "第二个问题\n补充条件" }),
  );
  expect(composer().value).toBe("");
  expect(
    host.querySelector<HTMLButtonElement>('[aria-label="发送问题"]')!.disabled,
  ).toBe(true);
  expect(tabCloseWarning(doc.id)).toBeNull();
});

it("preserves session drafts through remounts and checks every draft before closing", async () => {
  vi.mocked(api.chatSessions).mockResolvedValue(
    ["draft-a", "draft-b"].map((id) => ({
      id,
      documentId: doc.id,
      title: id,
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    })),
  );
  await act(async () => render(0));
  await enterPrompt("first draft");
  await enterPrompt("first draft with latest typing");
  await act(async () => panels.assistant!.onQuote!("quote for draft A"));
  await act(async () => panels.sessions!.onChange("draft-b"));
  expect(composer().value).toBe("");
  expect(host.querySelector(".quote-chips")).toBeNull();
  expect(tabCloseWarning(doc.id)?.message).toContain("未发送");
  await enterPrompt("second draft");
  await act(async () => panels.sessions!.onChange("draft-a"));
  expect(composer().value).toBe("first draft with latest typing");
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "quote for draft A",
  );
  const tab = (name: string) =>
    [...host.querySelectorAll<HTMLElement>('[role="tab"]')].find(
      (el) => el.textContent === name,
    )!;
  await act(async () => tab("笔记").click());
  await act(async () => tab("Agent").click());
  expect(composer().value).toBe("first draft with latest typing");
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "quote for draft A",
  );
  await act(async () =>
    host.querySelector<HTMLButtonElement>('[aria-label="移除引用"]')!.click(),
  );
  await enterPrompt("   ");
  expect(tabCloseWarning(doc.id)?.message).toContain("未发送");
  await act(async () => panels.sessions!.onChange("draft-b"));
  await enterPrompt("");
  expect(tabCloseWarning(doc.id)).toBeNull();
});

it("sends from a mounted reader after other chat sessions fill the cache", async () => {
  vi.useFakeTimers();
  vi.mocked(api.aiConfig).mockResolvedValue({
    primary: "api",
    models: {},
    capabilities: {},
  } as Awaited<ReturnType<typeof api.aiConfig>>);
  vi.mocked(api.chatSessions).mockResolvedValue([
    {
      id: "cache-current",
      documentId: doc.id,
      title: "current",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    },
  ]);
  await act(async () => render(0));
  await act(async () =>
    panels.readerReady?.({
      getLocation: () => undefined,
    } as unknown as ReaderAdapter),
  );
  await act(async () => vi.advanceTimersByTimeAsync(100));
  for (let i = 0; i < 24; i++)
    sessionRegistry.getChatSession(`cache-document-${i}`, `cache-session-${i}`);
  await enterPrompt("send this draft");
  const send = host.querySelector<HTMLButtonElement>(
    '[aria-label="发送问题"]',
  )!;
  expect(send.disabled).toBe(false);
  await act(async () => send.click());
  expect(api.startChatRun).toHaveBeenCalledExactlyOnceWith(
    doc.id,
    "cache-current",
    expect.objectContaining({ prompt: "send this draft" }),
  );
});

it("keeps long Chinese composition and paste native across workspace updates", async () => {
  vi.useFakeTimers();
  const errors = vi.spyOn(console, "error");
  await act(async () => render(0));
  await act(async () => vi.advanceTimersByTimeAsync(100));
  expect(api.annotations).toHaveBeenCalledOnce();
  const long = "长中文草稿需要保留输入法候选和光标。".repeat(200);
  await enterPrompt(long);
  composer().setSelectionRange(20, 20);
  const input = composer();
  for (let tick = 1; tick <= 20; tick++) await act(async () => render(tick));
  expect(composer()).toBe(input);
  expect(composer().defaultValue).toBe("");
  expect(composer().value).toBe(long);
  expect(composer().selectionStart).toBe(20);
  expect(
    errors.mock.calls.filter((call) => String(call[0]).includes("same key")),
  ).toHaveLength(0);
  const renders = panels.readerRenders;
  await act(async () => {
    composer().dispatchEvent(
      new CompositionEvent("compositionstart", { bubbles: true }),
    );
  });
  for (const candidate of ["zh", "zhong", "中文输入"])
    await enterPrompt(long + candidate);
  await act(async () => {
    composer().dispatchEvent(
      new CompositionEvent("compositionend", {
        data: "中文输入",
        bubbles: true,
      }),
    );
  });
  expect(panels.readerRenders).toBe(renders);
  expect(composer().value).toBe(long + "中文输入");
  await enterPrompt(long + "一次粘贴大量中文。".repeat(300));
  await act(async () => render(21));
  expect(composer().defaultValue).toBe("");
  expect(composer().value).toBe(long + "一次粘贴大量中文。".repeat(300));
});

it("does not copy the full long draft for native typing or IME updates", async () => {
  vi.useFakeTimers();
  await act(async () => render(0));
  await act(async () => vi.advanceTimersByTimeAsync(100));
  const input = composer();
  expect(input.getAttribute("spellcheck")).toBe("false");
  expect(input.getAttribute("writingsuggestions")).toBe("false");
  const setter = Object.getOwnPropertyDescriptor(
    HTMLTextAreaElement.prototype,
    "value",
  )!.set!;
  const reads = vi.spyOn(HTMLTextAreaElement.prototype, "value", "get");
  let text = "长草稿和中文输入法保持原生编辑。".repeat(500);
  for (const candidate of ["zh", "zhong", "中文", "继续输入"]) {
    text += candidate;
    await act(async () => {
      setter.call(input, text);
      input.dispatchEvent(
        new InputEvent("input", {
          bubbles: true,
          data: candidate,
          inputType: "insertCompositionText",
          isComposing: true,
        }),
      );
    });
  }
  expect(reads).not.toHaveBeenCalled();
  reads.mockRestore();
  expect(input.value).toBe(text);
  expect(tabCloseWarning(doc.id)).not.toBeNull();
  await act(async () => {
    setter.call(input, " ");
    input.dispatchEvent(
      new InputEvent("input", {
        bubbles: true,
        inputType: "deleteContentBackward",
      }),
    );
  });
  expect(tabCloseWarning(doc.id)).toBeNull();
});

it("avoids unchanged session refreshes and pauses catalog polling for background tabs", async () => {
  vi.useFakeTimers();
  try {
    const catalog = [
      {
        id: "polling",
        documentId: doc.id,
        title: "original",
        provider: "api",
        createdAt: "now",
        updatedAt: "now",
      },
    ];
    vi.mocked(api.chatSessions).mockResolvedValue(catalog);
    await act(async () => render(0));
    await act(async () => vi.advanceTimersByTimeAsync(100));
    const reads = vi.mocked(api.chatSessions).mock.calls.length;
    const renders = panels.readerRenders;
    await act(async () => vi.advanceTimersByTimeAsync(3000));
    expect(api.chatSessions).toHaveBeenCalledTimes(reads + 1);
    expect(panels.readerRenders).toBe(renders);
    vi.mocked(api.chatSessions).mockResolvedValue([
      { ...catalog[0], title: "renamed" },
    ]);
    await act(async () => vi.advanceTimersByTimeAsync(3000));
    expect(panels.sessions?.sessions[0].title).toBe("renamed");
    let resolveCatalog!: (items: typeof catalog) => void;
    vi.mocked(api.chatSessions).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveCatalog = resolve;
        }),
    );
    await act(async () => vi.advanceTimersByTimeAsync(3000));
    await act(async () => render(1, false));
    const backgroundRenders = panels.readerRenders;
    await act(async () =>
      resolveCatalog([{ ...catalog[0], title: "late background response" }]),
    );
    expect(panels.sessions?.sessions[0].title).toBe("renamed");
    expect(panels.readerRenders).toBe(backgroundRenders);
    const paused = vi.mocked(api.chatSessions).mock.calls.length;
    await act(async () => vi.advanceTimersByTimeAsync(9000));
    expect(api.chatSessions).toHaveBeenCalledTimes(paused);
    await act(async () => render(2, true));
    expect(api.chatSessions).toHaveBeenCalledTimes(paused + 1);
  } finally {
    vi.useRealTimers();
  }
});

it("keeps composition and modified Enter keys from sending and submits the latest text with Enter", async () => {
  vi.mocked(api.chatSessions).mockResolvedValue([
    {
      id: "keyboard",
      documentId: doc.id,
      title: "test",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    },
  ]);
  vi.mocked(api.aiConfig).mockResolvedValueOnce({
    primary: "api",
    models: {},
    capabilities: {},
  } as Awaited<ReturnType<typeof api.aiConfig>>);
  await act(async () => render(0));
  await act(async () =>
    panels.readerReady?.({
      getLocation: () => undefined,
    } as unknown as ReaderAdapter),
  );
  const sent = vi
    .spyOn(ChatSession.prototype, "send")
    .mockResolvedValue(undefined);
  await enterPrompt("你好");
  for (const options of [
    { isComposing: true },
    { keyCode: 229 },
    { shiftKey: true },
    { altKey: true },
    { repeat: true },
  ]) {
    await act(async () =>
      composer().dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "Enter",
          bubbles: true,
          cancelable: true,
          ...options,
        }),
      ),
    );
  }
  expect(sent).not.toHaveBeenCalled();
  await enterPrompt("你好，新问题");
  await act(async () =>
    composer().dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "Enter",
        bubbles: true,
        cancelable: true,
      }),
    ),
  );
  expect(sent).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({ prompt: "你好，新问题" }),
  );
  expect(composer().value).toBe("");
});

it("keeps streamed answer deltas out of the reader while accepting new draft input", async () => {
  vi.useFakeTimers();
  vi.mocked(api.chatSessions).mockResolvedValue([
    {
      id: "stream",
      documentId: doc.id,
      title: "test",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    },
  ]);
  let emit!: (event: ChatStreamEvent) => void, finish!: () => void;
  const waiting = new Promise<void>((resolve) => {
    finish = resolve;
  });
  const session = new ChatSession(doc.id, {
    messages: async () => [],
    stream: async (_doc, _input, _signal, callback) => {
      emit = callback;
      await waiting;
    },
  });
  vi.spyOn(sessionRegistry, "getChatSession").mockReturnValue(session);
  let running: Promise<void> | undefined;
  try {
    await act(async () => render(0));
    await act(async () =>
      panels.readerReady?.({
        getLocation: () => undefined,
      } as unknown as ReaderAdapter),
    );
    await act(async () => {
      running = session.send({
        provider: "api",
        prompt: "question",
        context: "",
        references: [],
        attachments: [],
      });
    });
    await enterPrompt("new draft");
    // Settle initial annotation loading (50 ms) and the reader frame before
    // measuring streamed updates, so initialization is not counted as a delta.
    await act(async () => vi.advanceTimersByTimeAsync(100));
    expect(api.annotations).toHaveBeenCalledOnce();
    const before = panels.readerRenders;
    for (const text of ["first", " second", " third"]) {
      await act(async () => {
        emit({ event: "delta", data: { text } });
        await vi.advanceTimersByTimeAsync(25);
      });
    }
    await enterPrompt("new draft typed while streaming");
    expect(panels.readerRenders).toBe(before);
    expect(composer().value).toBe("new draft typed while streaming");
    expect(session.getSnapshot().pending?.content).toBe("first second third");
  } finally {
    await act(async () => {
      finish();
      await running;
    });
    session.dispose();
    vi.useRealTimers();
  }
});

it("keeps chat quotations in the existing quote chips through sending and withdrawal", async () => {
  vi.mocked(api.chatSessions).mockResolvedValue([
    {
      id: "quotes",
      documentId: doc.id,
      title: "test",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    },
  ]);
  vi.mocked(api.aiConfig).mockResolvedValueOnce({
    primary: "api",
    models: {},
    capabilities: {},
  } as Awaited<ReturnType<typeof api.aiConfig>>);
  await act(async () => render(0));
  await act(async () =>
    panels.readerReady?.({
      getLocation: () => undefined,
    } as unknown as ReaderAdapter),
  );
  await enterPrompt("existing draft");
  await act(async () => panels.assistant!.onQuote!("first line\nsecond line"));
  expect(composer().value).toBe("existing draft");
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "first line",
  );
  expect(tabCloseWarning(doc.id)).not.toBeNull();
  await act(async () => render(1));
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "second line",
  );
  await act(async () =>
    host.querySelector<HTMLButtonElement>('[aria-label="移除引用"]')!.click(),
  );
  expect(host.querySelector(".quote-chips")).toBeNull();
  expect(composer().value).toBe("existing draft");
  await act(async () => panels.assistant!.onQuote!("first line\nsecond line"));
  const sent = vi
    .spyOn(ChatSession.prototype, "send")
    .mockResolvedValue(undefined);
  await act(async () =>
    host.querySelector<HTMLButtonElement>('[aria-label="发送问题"]')!.click(),
  );
  const references = [
    { kind: "chat" as const, text: "first line\nsecond line" },
  ];
  expect(sent).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({
      prompt: "existing draft",
      references,
      context: "[引用对话 · 不作为原文证据]\nfirst line\nsecond line",
    }),
  );
  expect(host.querySelector(".quote-chips")).toBeNull();
  expect(composer().value).toBe("");
  await act(async () =>
    panels.assistant!.onWithdraw!({
      id: "withdraw",
      documentId: doc.id,
      role: "user",
      content: "existing draft",
      references,
      createdAt: "now",
    }),
  );
  expect(composer().value).toBe("existing draft");
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "first line",
  );
});

it("uses the existing toolbar to save a hovered sentence without a native selection", async () => {
  await act(async () => render(0));
  const selected: import("@neoread/core").ReaderSelection = {
    text: "完整译文。",
    anchor: { x: 30, top: 20, bottom: 40 },
    location: {
      type: "pdf",
      page: 1,
      translation: {
        blockId: "p1-b1",
        sourceHash: "hash",
        sentenceIndexes: [0],
        start: 0,
        end: 0,
        recovered: true,
        wholeSentence: true,
        bilingual: true,
        sourceText: "Whole sentence.",
      },
    },
  };
  vi.mocked(api.annotate).mockResolvedValue({
    id: "shared-sentence",
    documentId: doc.id,
    kind: "highlight",
    color: "#e6b94c",
    quote: selected.text,
    note: "",
    createdAt: "",
    location: selected.location,
  });
  window.getSelection()!.removeAllRanges();
  await act(async () => panels.readerEvents!.sentenceHover!(selected));
  expect(
    host.querySelector('.selection-bar [aria-label="添加批注"]'),
  ).not.toBeNull();
  await act(async () => document.dispatchEvent(new Event("selectionchange")));
  expect(host.querySelector(".selection-bar")).not.toBeNull();
  await act(async () =>
    host
      .querySelector<HTMLButtonElement>(
        '.selection-bar [aria-label="黄色高亮"]',
      )!
      .click(),
  );
  expect(api.annotate).toHaveBeenCalledExactlyOnceWith(
    doc.id,
    expect.objectContaining({
      quote: selected.text,
      location: selected.location,
    }),
  );
  expect(host.querySelector(".selection-bar")).toBeNull();
  expect(window.getSelection()?.toString()).toBe("");
  await act(async () => panels.readerEvents!.sentenceHover!(selected));
  await act(async () =>
    document.body.dispatchEvent(
      new MouseEvent("pointerdown", { bubbles: true }),
    ),
  );
  expect(host.querySelector(".selection-bar")).toBeNull();
});

it("does not rerender mounted workspaces for another document's job update", async () => {
  const { ReadingWorkspaces } = await import("./DocumentTabs");
  const hidden = paper({ id: "hidden-doc" });
  useReaderStore.setState({
    documents: [doc, hidden],
    tabIds: [doc.id, hidden.id],
    visitedTabs: [doc.id, hidden.id],
    active: doc,
  });
  const onLibrary = () => {},
    onSettings = () => {};
  const show = (jobs: Processing[]) =>
    root.render(
      <TooltipProvider>
        <ReadingWorkspaces
          theme={defaultTheme}
          jobs={jobs}
          onLibrary={onLibrary}
          onSettings={onSettings}
        />
      </TooltipProvider>,
    );
  await act(async () => show([processing]));
  const before = panels.readerRenders;
  await act(async () =>
    show([
      processing,
      { ...processing, documentId: "unrelated", updatedAt: "new" },
    ]),
  );
  expect(panels.readerRenders).toBe(before);
});

const noteField = () =>
  document.querySelector<HTMLTextAreaElement>('[role="dialog"] textarea')!;
const selectedText: ReaderSelection = {
  text: "Original sentence.",
  location: { type: "pdf", page: 1 },
  anchor: { x: 30, top: 20, bottom: 40 },
};
const savedNote: Annotation = {
  id: "note-input",
  documentId: doc.id,
  kind: "note",
  quote: selectedText.text,
  location: selectedText.location,
  note: "保留草稿",
  color: "#e6b94c",
  createdAt: "",
};
async function enterField(
  field: HTMLInputElement | HTMLTextAreaElement,
  text: string,
) {
  await act(async () => {
    const prototype =
      field instanceof HTMLInputElement
        ? HTMLInputElement.prototype
        : HTMLTextAreaElement.prototype;
    Object.getOwnPropertyDescriptor(prototype, "value")!.set!.call(field, text);
    field.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
async function openNote(action = "添加批注", selection = selectedText) {
  await act(async () => panels.readerEvents!.selection(selection));
  await act(async () =>
    host
      .querySelector<HTMLButtonElement>(
        `.selection-bar [aria-label="${action}"]`,
      )!
      .click(),
  );
}
async function showSearch() {
  await act(async () =>
    [...host.querySelectorAll<HTMLButtonElement>('[role="tab"]')]
      .find((tab) => tab.textContent === "搜索")!
      .click(),
  );
}
async function submitField(field: HTMLInputElement) {
  await act(async () =>
    field.form!.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    ),
  );
}

it.each(["添加批注", "提问"])(
  "retains a failed %s draft through updates and background tabs, then retries",
  async (action) => {
    vi.useFakeTimers();
    await act(async () => render(0));
    await act(async () => vi.advanceTimersByTimeAsync(100));
    await openNote(action);
    const save = () => button(action === "提问" ? "保存问题" : "保存批注");
    expect(save().disabled).toBe(true);
    expect(document.activeElement).toBe(noteField());
    expect(tabCloseWarning(doc.id)).toBeNull();
    await enterField(noteField(), savedNote.note);
    await act(async () => render(1));
    expect(noteField().value).toBe(savedNote.note);
    await act(async () => render(2, false));
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(tabCloseWarning(doc.id)).not.toBeNull();
    await act(async () => render(3));
    expect(noteField().value).toBe(savedNote.note);

    let fail!: (error: Error) => void;
    vi.mocked(api.annotate).mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          fail = reject;
        }),
    );
    await act(async () => save().click());
    expect(save().disabled).toBe(true);
    expect(tabCloseWarning(doc.id)?.blocked).toBe(true);
    await act(async () => button("Close").click());
    expect(noteField().value).toBe(savedNote.note);
    await act(async () => fail(new Error("保存失败")));
    expect(toast.error).toHaveBeenCalledWith("保存失败");
    expect(tabCloseWarning(doc.id)?.blocked).not.toBe(true);
    expect(noteField().value).toBe(savedNote.note);
    expect(save().disabled).toBe(false);
    vi.mocked(api.annotate).mockResolvedValueOnce({
      ...savedNote,
      kind: action === "提问" ? "question" : "note",
    });
    await act(async () => save().click());
    expect(api.annotate).toHaveBeenLastCalledWith(
      doc.id,
      expect.objectContaining({ note: savedNote.note }),
    );
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(tabCloseWarning(doc.id)).toBeNull();
    await openNote(action);
    expect(noteField().value).toBe("");
  },
);

it("clears a closed draft and resets when opening a different selection", async () => {
  await act(async () => render(0));
  await openNote();
  await enterField(noteField(), "待清除");
  await act(async () => button("Close").click());
  expect(tabCloseWarning(doc.id)).toBeNull();
  await openNote();
  expect(noteField().value).toBe("");
  await enterField(noteField(), "另一个草稿");
  await openNote();
  expect(noteField().value).toBe("");
});

it.each([false, true])(
  "keeps the closed annotation form mounted and resets new selections and editing with reduced motion %s",
  async (reducedMotion) => {
    vi.useFakeTimers();
    vi.mocked(api.annotations).mockResolvedValue([savedNote]);
    useReaderStore.setState({ theme: { ...defaultTheme, reducedMotion } });
    const removeMotion = installMotionPreference(document);
    try {
      await act(async () => render(0));
      await act(async () => vi.advanceTimersByTimeAsync(100));
      await openNote();
      await enterField(noteField(), "关闭前的草稿");
      const mounts = panels.noteFormMounts,
        unmounts = panels.noteFormUnmounts;
      await act(async () => button("Close").click());
      expect(panels.noteFormMounts).toBe(mounts);
      expect(panels.noteFormUnmounts).toBe(unmounts);
      expect(tabCloseWarning(doc.id)).toBeNull();

      await openNote("添加批注", {
        ...selectedText,
        text: "Different sentence.",
        location: { type: "pdf", page: 2 },
      });
      expect(noteField().value).toBe("");
      expect(document.querySelector(".note-preview")?.textContent).toBe(
        "Different sentence.",
      );
      expect(tabCloseWarning(doc.id)).toBeNull();
      await act(async () => button("Close").click());
      await act(async () => {
        panels.readerEvents!.selection(null);
        panels.readerEvents!.annotation!({
          ids: [savedNote.id],
          anchor: selectedText.anchor!,
        });
      });
      await act(async () =>
        host
          .querySelector<HTMLButtonElement>(
            '.selection-bar [aria-label="编辑批注"]',
          )!
          .click(),
      );
      expect(noteField().value).toBe(savedNote.note);
      expect(document.querySelector(".note-preview")?.textContent).toBe(
        savedNote.quote,
      );
      expect(document.documentElement.dataset.reducedMotion).toBe(
        String(reducedMotion),
      );
      expect(
        document
          .querySelector('[role="dialog"]')!
          .classList.contains("motion-reduce:animate-none"),
      ).toBe(true);
      await act(async () => button("Close").click());
      expect(tabCloseWarning(doc.id)).toBeNull();
    } finally {
      removeMotion();
    }
  },
);

it.each([
  ["note", "原批注", true],
  ["highlight", "", false],
  ["question", "原问题", false],
] as const)(
  "preserves %s editing and its empty-content rule",
  async (kind, note, canClear) => {
    vi.useFakeTimers();
    const original = { ...savedNote, kind, note };
    vi.mocked(api.annotations).mockResolvedValue([original]);
    await act(async () => render(0));
    await act(async () => vi.advanceTimersByTimeAsync(100));
    await act(async () =>
      panels.readerEvents!.annotation!({
        ids: [original.id],
        anchor: selectedText.anchor!,
      }),
    );
    const label =
      kind === "question" ? "编辑问题" : note ? "编辑批注" : "添加批注";
    await act(async () =>
      host
        .querySelector<HTMLButtonElement>(
          `.selection-bar [aria-label="${label}"]`,
        )!
        .click(),
    );
    expect(noteField().value).toBe(note);
    expect(button("保存并让 AI 回答")).toBeUndefined();
    const save = () => button(kind === "question" ? "保存问题" : "保存批注");
    await enterField(noteField(), "");
    expect(save().disabled).toBe(!canClear);
    await enterField(noteField(), "修改后的内容");
    vi.mocked(api.updateAnnotationNote).mockRejectedValueOnce(
      new Error("编辑失败"),
    );
    await act(async () => save().click());
    expect(noteField().value).toBe("修改后的内容");
    expect(api.annotate).not.toHaveBeenCalled();
    expect(toast.error).toHaveBeenCalledWith("编辑失败");
    const text = canClear ? "" : "修改后的内容";
    await enterField(noteField(), text);
    vi.mocked(api.updateAnnotationNote).mockResolvedValueOnce({
      ...original,
      note: text,
    });
    await act(async () => save().click());
    expect(api.updateAnnotationNote).toHaveBeenLastCalledWith(
      doc.id,
      original.id,
      text,
    );
    expect(document.querySelector('[role="dialog"]')).toBeNull();
  },
);

it("answers only after the question is saved and keeps the draft when saving fails", async () => {
  vi.mocked(api.aiConfig).mockResolvedValueOnce({
    primary: "api",
    models: {},
    capabilities: {},
  } as Awaited<ReturnType<typeof api.aiConfig>>);
  vi.mocked(api.chatSessions).mockResolvedValue([
    {
      id: "annotation-question",
      documentId: doc.id,
      title: "test",
      provider: "api",
      createdAt: "",
      updatedAt: "",
    },
  ]);
  await act(async () => render(0));
  await openNote("提问");
  const answer = () => button("保存并让 AI 回答");
  expect(answer().disabled).toBe(true);
  await enterField(noteField(), "这个结论如何得到？");
  const send = vi
    .spyOn(ChatSession.prototype, "send")
    .mockResolvedValue(undefined);
  vi.mocked(api.annotate).mockRejectedValueOnce(new Error("写入失败"));
  await act(async () => answer().click());
  expect(send).not.toHaveBeenCalled();
  expect(noteField().value).toBe("这个结论如何得到？");
  let finish!: (annotation: Annotation) => void;
  vi.mocked(api.annotate).mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  await act(async () => answer().click());
  expect(send).not.toHaveBeenCalled();
  expect(answer().disabled).toBe(true);
  await act(async () =>
    finish({ ...savedNote, kind: "question", note: "这个结论如何得到？" }),
  );
  expect(send).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({
      prompt: expect.stringContaining("这个结论如何得到？"),
    }),
  );
  expect(document.querySelector('[role="dialog"]')).toBeNull();
});

it("submits sidebar searches explicitly and clears results without rendering Workspace", async () => {
  vi.useFakeTimers();
  const found: SearchResult[] = [
    {
      id: "search-result",
      location: { type: "pdf", page: 3 },
      excerpt: "原文命中",
      side: "source",
    },
  ];
  const search = vi.fn<ReaderAdapter["search"]>().mockResolvedValue(found);
  const focusLocation = vi.fn(async () => {});
  await act(async () => render(0));
  await act(async () =>
    panels.readerReady!({ search, focusLocation } as unknown as ReaderAdapter),
  );
  await showSearch();
  await act(async () => vi.advanceTimersByTimeAsync(100));
  const field = host.querySelector<HTMLInputElement>(
    '[aria-label="搜索文档"]',
  )!;
  const workspaceRenders = panels.workspaceRenders;
  await enterField(field, "  keyword  ");
  expect(search).not.toHaveBeenCalled();
  await submitField(field);
  expect(search).toHaveBeenCalledExactlyOnceWith("keyword");
  expect(panels.readerSearch).toEqual({ results: found, query: "keyword" });
  expect(host.querySelectorAll(".search-result")).toHaveLength(1);
  await act(async () =>
    host.querySelector<HTMLButtonElement>(".search-result")!.click(),
  );
  expect(focusLocation).toHaveBeenCalledExactlyOnceWith(found[0].location);
  await enterField(field, "new");
  expect(host.querySelectorAll(".search-result")).toHaveLength(0);
  expect(panels.readerSearch).toEqual({ results: [], query: "" });
  const readerRenders = panels.readerRenders;
  for (const text of ["new k", "new key", "new keyword", ""])
    await enterField(field, text);
  expect(panels.readerRenders).toBe(readerRenders);
  expect(panels.workspaceRenders).toBe(workspaceRenders);

  let finish!: (results: SearchResult[]) => void;
  search.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  await enterField(field, "stale");
  await submitField(field);
  expect(host.textContent).toContain("正在搜索…");
  await enterField(field, "latest");
  await act(async () => finish(found));
  expect(host.querySelectorAll(".search-result")).toHaveLength(0);
  expect(panels.readerSearch).toEqual({ results: [], query: "" });
  expect(host.textContent).not.toContain("正在搜索…");
  expect(panels.workspaceRenders).toBe(workspaceRenders);

  await act(async () =>
    [...host.querySelectorAll<HTMLButtonElement>('[role="tab"]')]
      .find((tab) => tab.textContent === "目录")!
      .click(),
  );
  await act(async () =>
    window.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "f",
        ctrlKey: !/Mac/.test(navigator.platform),
        metaKey: /Mac/.test(navigator.platform),
        bubbles: true,
        cancelable: true,
      }),
    ),
  );
  await act(async () => vi.advanceTimersByTimeAsync(50));
  expect(document.activeElement).toBe(field);
  expect(field.value).toBe("latest");
  await act(async () => render(1, true, "pdf", "another-document"));
  expect(
    host.querySelector<HTMLInputElement>('[aria-label="搜索文档"]')!.value,
  ).toBe("");
  expect(panels.readerSearch).toEqual({ results: [], query: "" });
});

it("keeps PDF page typing local, validates submission and follows reader locations", async () => {
  vi.useFakeTimers();
  const goTo = vi.fn(async () => {});
  await act(async () => render(0));
  await act(async () =>
    panels.readerReady!({ goTo } as unknown as ReaderAdapter),
  );
  await act(async () => vi.advanceTimersByTimeAsync(100));
  await act(async () =>
    panels.readerEvents!.location({ type: "pdf", page: 42 }, 0.5),
  );
  const field = host.querySelector<HTMLInputElement>(
    '[aria-label="跳转 PDF 页码"]',
  )!;
  expect(field.value).toBe("42");
  const workspaceRenders = panels.workspaceRenders,
    readerRenders = panels.readerRenders;
  await enterField(field, "17");
  expect(goTo).not.toHaveBeenCalled();
  await submitField(field);
  expect(goTo).toHaveBeenCalledExactlyOnceWith({ type: "pdf", page: 17 });
  for (const text of ["", "0", "-1", "1.5", "abc"]) {
    await enterField(field, text);
    await submitField(field);
  }
  expect(goTo).toHaveBeenCalledOnce();
  await act(async () =>
    panels.readerEvents!.location({ type: "pdf", page: 18 }, 0.6),
  );
  expect(field.value).toBe("18");
  expect(panels.workspaceRenders).toBe(workspaceRenders);
  expect(panels.readerRenders).toBe(readerRenders);
});

it("preserves the PDF page draft when reading modes move the navigation controls", async () => {
  vi.useFakeTimers();
  await act(async () => render(0));
  await act(async () => vi.advanceTimersByTimeAsync(100));
  const field = () =>
    host.querySelector<HTMLInputElement>('[aria-label="跳转 PDF 页码"]')!;
  await enterField(field(), "25");
  panels.pageNavigationVisible = false;
  await act(async () => render(1));
  panels.pageNavigationVisible = true;
  await act(async () => render(2));
  expect(field().value).toBe("25");
  panels.pageNavigationVisible = false;
  await act(async () => render(3));
  await act(async () =>
    panels.readerEvents!.location({ type: "pdf", page: 26 }, 0.5),
  );
  panels.pageNavigationVisible = true;
  await act(async () => render(4));
  expect(field().value).toBe("26");
});
