// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@neoread/api";
import { useReaderStore } from "../store";
import { LibraryAgentPanel } from "./LibraryAgentPanel";

const { chat, saved, localDrafts, stores, makeDraft } = vi.hoisted(() => {
  const summary = { busy: false, loaded: true };
  return {
    localDrafts: new Map(),
    stores: new Map(),
    makeDraft: vi.fn(),
    chat: {
      subscribe: () => () => {},
      getSummary: () => summary,
      getSnapshot: vi.fn(() => summary),
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
      scope: {
        category: "模型安全",
        categoryKey: "folder:模型安全",
        documentIds: ["alpha", "beta"],
      },
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
vi.mock("../chat/session-registry", () => ({
  getChatSession: (_owner: string, id: string) => {
    if (!stores.has(id)) stores.set(id, { ...chat, sessionId: id, viewId: id });
    return stores.get(id);
  },
  createLibraryChatDraft: makeDraft,
  listChatDrafts: () => [...localDrafts.values()],
  recoverCreatedChatDrafts: () => {},
  isDraftSession: (id: string) => id.startsWith("draft-"),
  chatCreatedEvent: "neoread:chat-session-created",
}));
vi.mock("../ModelSelector", () => ({
  ModelSelector: () => <span>Codex</span>,
}));
vi.mock("../chat/SessionPicker", () => ({
  SessionPicker: ({
    current,
    onCreate,
    sessions,
    onChange,
  }: {
    current?: { id: string; title: string };
    sessions: { id: string; title: string }[];
    onChange: (id: string) => void;
    onCreate: () => void;
  }) => (
    <div>
      <select
        aria-label="选择会话"
        value={current?.id ?? ""}
        onChange={(event) => onChange(event.target.value)}
      >
        {sessions.map((item) => (
          <option key={item.id} value={item.id}>
            {item.title}
          </option>
        ))}
      </select>
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
let root: Root,
  host: HTMLDivElement,
  serial = 0;
beforeEach(() => {
  vi.clearAllMocks();
  localDrafts.clear();
  stores.clear();
  makeDraft.mockImplementation((provider, scope) => {
    const item = { ...saved, id: `draft-${++serial}`, provider, scope };
    localDrafts.set(item.id, item);
    return item;
  });
  saved.id = `library-existing-${++serial}`;
  vi.mocked(api.libraryChatSessions).mockResolvedValue([saved]);
  vi.mocked(api.createLibraryChatSession).mockImplementation(
    async (provider, scope) => ({
      ...saved,
      id: `library-created-${++serial}`,
      provider,
      scope,
    }),
  );
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
const render = (
  category = "模型安全",
  documentIds = ["alpha", "beta"],
  categoryKey = `folder:${category}`,
) =>
  act(async () => {
    root.render(
      <LibraryAgentPanel
        scope={{ category, categoryKey, documentIds }}
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

const selected = () =>
  host.querySelector<HTMLSelectElement>('select[aria-label="选择会话"]')!;
const choose = (id: string) =>
  act(async () => {
    selected().value = id;
    selected().dispatchEvent(new Event("change", { bubbles: true }));
  });

it("switches conversations and preserves drafts by category, with multiple conversations in each category", async () => {
  chat.getSnapshot.mockReturnValueOnce({ busy: false, loaded: false });
  await render();
  expect(chat.load).toHaveBeenCalledTimes(1);
  await type("保留模型安全草稿");
  await render("视觉攻击", ["gamma"]);
  expect(makeDraft).toHaveBeenCalledWith("codex", {
    category: "视觉攻击",
    categoryKey: "folder:视觉攻击",
    documentIds: ["gamma"],
  });
  expect(host.textContent).toContain("视觉攻击");
  expect(selected().options).toHaveLength(1);
  await type("视觉攻击的草稿");
  await render();
  expect(selected().value).toBe(saved.id);
  expect(host.querySelector("textarea")?.value).toBe("保留模型安全草稿");
  expect(chat.load).toHaveBeenCalledTimes(1);
  await click("新建会话");
  expect(selected().options).toHaveLength(2);
  expect(selected().value).not.toBe(saved.id);
  expect(host.querySelector("textarea")?.value).toBe("");
  await choose(saved.id);
  expect(host.querySelector("textarea")?.value).toBe("保留模型安全草稿");
  await render("视觉攻击", ["gamma"]);
  expect(host.querySelector("textarea")?.value).toBe("视觉攻击的草稿");
  expect(makeDraft).toHaveBeenCalledTimes(2);
  expect(api.createLibraryChatSession).not.toHaveBeenCalled();
});

it("keeps the current conversation when synchronization adds a newer conversation", async () => {
  await render();
  await type("正在编辑的草稿");
  vi.mocked(api.libraryChatSessions).mockResolvedValue([
    { ...saved, id: "library-synced", title: "另一台设备的新会话" },
    saved,
  ]);
  await act(async () => {
    window.dispatchEvent(
      new CustomEvent("neoread:sync-applied", { detail: ["library"] }),
    );
  });
  expect(selected().options).toHaveLength(2);
  expect(selected().value).toBe(saved.id);
  expect(host.querySelector("textarea")?.value).toBe("正在编辑的草稿");
});

it("keeps a conversation's paper scope fixed when the category membership changes", async () => {
  await render();
  await render("模型安全", ["alpha", "beta", "gamma"]);
  expect(api.createLibraryChatSession).not.toHaveBeenCalled();
  expect(host.textContent).toContain("2 篇");
  await click("使用当前分类新建");
  expect(makeDraft).toHaveBeenCalledWith(
    "codex",
    expect.objectContaining({ documentIds: ["alpha", "beta", "gamma"] }),
  );
  expect(host.textContent).toContain("3 篇");
});

it("separates same-named categories using local drafts without creating server conversations", async () => {
  await render();
  await render("模型安全", ["gamma"], "tag:模型安全");
  const tagDraft = selected().value;
  expect(tagDraft).toMatch(/^draft-/);
  await type("标签分类草稿");
  await render("新分类", ["delta"]);
  const current = selected().value;
  expect(current).not.toBe(tagDraft);
  await render("模型安全", ["gamma"], "tag:模型安全");
  expect(selected().value).toBe(tagDraft);
  expect(host.querySelector("textarea")?.value).toBe("标签分类草稿");
  expect(api.createLibraryChatSession).not.toHaveBeenCalled();
});
it("does not switch back when a background draft becomes a saved conversation", async () => {
  await render("背景分类", ["gamma"]);
  const draftId = selected().value;
  await type("首次发送期间输入的下一条草稿");
  await click("引用所选聊天");
  await render("其他分类", ["delta"]);
  const active = selected().value;
  localDrafts.delete(draftId);
  await act(async () =>
    window.dispatchEvent(
      new CustomEvent("neoread:chat-session-created", {
        detail: {
          documentId: "library",
          draftId,
          session: {
            ...saved,
            id: "library-created-background",
            scope: {
              category: "背景分类",
              categoryKey: "folder:背景分类",
              documentIds: ["gamma"],
            },
          },
        },
      }),
    ),
  );
  expect(selected().value).toBe(active);
  await render("背景分类", ["gamma"]);
  expect(selected().value).toBe("library-created-background");
  expect(host.querySelector("textarea")?.value).toBe(
    "首次发送期间输入的下一条草稿",
  );
  expect(host.querySelector(".quote-chips")?.textContent).toContain(
    "比较它们的实验预算",
  );
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
