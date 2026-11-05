// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { useReaderStore } from "../store";
import { LibraryAgentPanel } from "./LibraryAgentPanel";

const { chat, saved } = vi.hoisted(() => {
  const summary = { busy: false, loaded: true };
  return {
    chat: {
      subscribe: () => () => {},
      getSummary: () => summary,
      activate: vi.fn(),
      load: vi.fn(async () => {}),
      send: vi.fn(async () => {}),
      regenerate: vi.fn(async () => {}),
      cancel: vi.fn(),
    },
    saved: {
      id: "library-existing",
      documentId: "library",
      title: "研究路线",
      provider: "codex",
      createdAt: "now",
      updatedAt: "now",
      scope: { category: "模型安全", documentIds: ["alpha", "beta"] },
    },
  };
});
vi.mock("@neoread/api", () => ({
  api: {
    libraryChatSessions: vi.fn(async () => [saved]),
    createLibraryChatSession: vi.fn(async (provider, scope) => ({
      ...saved,
      id: "library-created",
      provider,
      scope,
    })),
    validateLibraryEvidence: vi.fn(),
  },
}));
vi.mock("../chat/session-registry", () => ({ getChatSession: () => chat }));
vi.mock("../ModelSelector", () => ({
  ModelSelector: () => <span>Codex</span>,
}));
vi.mock("../chat/SessionPicker", () => ({
  SessionPicker: ({
    current,
    onCreate,
  }: {
    current?: { title: string };
    onCreate: () => void;
  }) => (
    <div>
      <span>{current?.title}</span>
      <button onClick={onCreate}>新建会话</button>
    </div>
  ),
}));
vi.mock("../chat/AssistantPanel", () => ({
  AssistantPanel: ({ onQuote }: { onQuote: (text: string) => void }) => (
    <div className="chat-scroll">
      <button onClick={() => onQuote("比较它们的实验预算")}>
        引用所选聊天
      </button>
    </div>
  ),
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));
let root: Root, host: HTMLDivElement;
beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  const storage = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
    clear: () => storage.clear(),
  });
  localStorage.clear();
  useReaderStore.setState({
    aiConfig: { primary: "codex" } as never,
    aiModelSaving: false,
  });
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
const render = (category = "模型安全", documentIds = ["alpha", "beta"]) =>
  act(async () => {
    root.render(
      <LibraryAgentPanel
        scope={{ category, documentIds }}
        onClose={() => {}}
        onSettings={() => {}}
      />,
    );
  });
const click = (text: string) =>
  act(async () => {
    [...host.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === text)!
      .click();
  });
const type = (text: string) =>
  act(async () => {
    const area = host.querySelector<HTMLTextAreaElement>("textarea")!;
    area.value = text;
    area.dispatchEvent(new Event("input", { bubbles: true }));
  });

it("keeps the existing paper scope until a new conversation is explicitly created", async () => {
  await render();
  expect(api.createLibraryChatSession).not.toHaveBeenCalled();
  await render("视觉攻击", ["gamma"]);
  expect(host.textContent).toContain("模型安全");
  expect(host.textContent).toContain("2 篇");
  expect(host.textContent).toContain("使用当前分类新建");
  await type("保留这段草稿");
  await click("使用当前分类新建");
  expect(api.createLibraryChatSession).toHaveBeenCalledWith("codex", {
    category: "视觉攻击",
    documentIds: ["gamma"],
  });
  expect(host.textContent).toContain("视觉攻击");
  expect(host.textContent).toContain("1 篇");
});

it("keeps Chinese composition and quotes separate from native draft text", async () => {
  await render();
  const text = "长中文草稿".repeat(300);
  await type(text);
  await click("引用所选聊天");
  const area = host.querySelector<HTMLTextAreaElement>("textarea")!;
  expect(area.value).toBe(text);
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "比较它们的实验预算",
  );
  await act(async () => {
    area.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "Enter",
        keyCode: 229,
        isComposing: true,
        bubbles: true,
      }),
    );
  });
  expect(chat.send).not.toHaveBeenCalled();
  await act(async () => {
    host.querySelector<HTMLButtonElement>('[aria-label="发送问题"]')!.click();
  });
  expect(chat.send).toHaveBeenCalledWith(
    expect.objectContaining({
      prompt: text,
      references: [{ kind: "chat", text: "比较它们的实验预算" }],
    }),
  );
  expect(area.value).toBe("");
});
