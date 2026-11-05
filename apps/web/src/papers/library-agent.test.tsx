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
vi.mock("../chat/session-registry", () => ({ getChatSession: () => chat }));
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
  expect(api.createLibraryChatSession).toHaveBeenCalledWith("codex", {
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
  expect(api.createLibraryChatSession).toHaveBeenCalledTimes(2);
});

it("keeps a conversation's paper scope fixed when the category membership changes", async () => {
  await render();
  await render("模型安全", ["alpha", "beta", "gamma"]);
  expect(api.createLibraryChatSession).not.toHaveBeenCalled();
  expect(host.textContent).toContain("2 篇");
  await click("使用当前分类新建");
  expect(api.createLibraryChatSession).toHaveBeenCalledWith(
    "codex",
    expect.objectContaining({ documentIds: ["alpha", "beta", "gamma"] }),
  );
  expect(host.textContent).toContain("3 篇");
});

it("separates categories with the same display name and never switches back after a delayed creation", async () => {
  await render();
  let resolve!: (item: typeof saved) => void;
  vi.mocked(api.createLibraryChatSession).mockImplementationOnce(
    (_provider, scope) =>
      new Promise((done) => {
        resolve = done as typeof resolve;
      }),
  );
  await render("模型安全", ["gamma"], "tag:模型安全");
  await click("新建会话");
  expect(api.createLibraryChatSession).toHaveBeenCalledTimes(1);
  await render("新分类", ["delta"]);
  const active = selected().value;
  await act(async () =>
    resolve({
      ...saved,
      id: "library-delayed",
      scope: {
        category: "模型安全",
        categoryKey: "tag:模型安全",
        documentIds: ["gamma"],
      },
    }),
  );
  expect(selected().value).toBe(active);
  expect(host.textContent).toContain("新分类");
  await render("模型安全", ["gamma"], "tag:模型安全");
  expect(selected().value).toBe("library-delayed");
  expect(api.createLibraryChatSession).toHaveBeenCalledTimes(2);
});

it("does not let a failed creation in one category block another category", async () => {
  await render();
  vi.mocked(api.createLibraryChatSession).mockRejectedValueOnce(
    new Error("暂时无法创建"),
  );
  await render("失败分类", ["gamma"]);
  expect(host.textContent).toContain("暂时无法创建");
  await render("其他分类", ["delta"]);
  expect(host.textContent).toContain("其他分类");
  expect(host.querySelector("textarea")).not.toBeNull();
  expect(host.textContent).not.toContain("暂时无法创建");
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
