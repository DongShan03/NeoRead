// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { DocumentChatSession } from "@neoread/core";
import { SessionPicker } from "./SessionPicker";

const mocks = vi.hoisted(() => ({
  create: vi.fn(),
  update: vi.fn(),
  remove: vi.fn(),
  forget: vi.fn(),
  busy: vi.fn(() => false),
  started: vi.fn(async () => true),
  rename: vi.fn(async () => {}),
}));
vi.mock("@neoread/api", () => ({
  api: {
    createChatSession: mocks.create,
    updateChatSession: mocks.update,
    deleteChatSession: mocks.remove,
  },
}));
vi.mock("./session-registry", () => ({
  chatSessionBusy: mocks.busy,
  chatSessionHasStarted: mocks.started,
  getChatSession: (_owner: string, id: string) => ({ viewId: id }),
  renameChatSession: mocks.rename,
  chatSessionRecordId: (_owner: string, id: string) =>
    id.startsWith("draft-") ? undefined : id,
  forgetChatSession: mocks.forget,
  isDraftSession: (id: string) => id.startsWith("draft-"),
}));
let root: Root, host: HTMLDivElement;
beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.clearAllMocks();
  mocks.started.mockResolvedValue(true);
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.unstubAllGlobals();
});
it("creates using the updated settings choice rather than the current session's previous backend", async () => {
  const current: DocumentChatSession = {
    id: "old-session",
    documentId: "doc",
    title: "方法问题",
    provider: "api",
    createdAt: "now",
    updatedAt: "now",
  };
  const onChange = vi.fn(),
    onRefresh = vi.fn().mockResolvedValue(undefined);
  const render = async (provider: string) => {
    await act(async () =>
      root.render(
        <SessionPicker
          documentId="doc"
          sessions={[current]}
          current={current}
          onCreate={async () => {
            mocks.create(provider);
          }}
          onDeleted={() => {}}
          onChange={onChange}
          onRefresh={onRefresh}
        />,
      ),
    );
  };
  await render("api");
  await render("codex");

  await act(async () =>
    (
      host.querySelector('[aria-label="新建会话"]') as HTMLButtonElement
    ).click(),
  );
  expect(mocks.create).toHaveBeenCalledExactlyOnceWith("codex");
  expect(onChange).not.toHaveBeenCalled();
  expect(mocks.update).not.toHaveBeenCalled();
  expect(host.querySelector('[aria-label="会话后端"]')).toBeNull();
  expect(host.querySelector('[aria-label="选择会话"]')).not.toBeNull();
});

it.each([
  "running",
  "completed",
  "failed",
  "cancelled",
  "interrupted",
] as const)(
  "does not show a separate status row for %s sessions",
  async (status) => {
    const current: DocumentChatSession = {
      id: "session",
      documentId: "doc",
      title: "方法问题",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
      latestRun: {
        id: "old-run",
        sessionId: "session",
        documentId: "doc",
        userMessageId: "question",
        provider: "api",
        status,
        startedAt: "now",
      },
    };
    await act(async () =>
      root.render(
        <SessionPicker
          documentId="doc"
          sessions={[current]}
          current={current}
          onCreate={async () => {}}
          onDeleted={() => {}}
          onChange={() => {}}
          onRefresh={async () => {}}
        />,
      ),
    );
    expect(host.querySelector('[role="status"]')).toBeNull();
    expect(host.querySelector('[aria-label="选择会话"]')).not.toBeNull();
  },
);

it.each(["doc", "library"])(
  "deletes the selected row without choosing it for %s",
  async (documentId) => {
    const current: DocumentChatSession = {
      id: "current",
      documentId,
      title: "当前会话",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    };
    const other = { ...current, id: "other", title: "其他会话" };
    const onChange = vi.fn(),
      onDeleted = vi.fn(),
      onRefresh = vi.fn(async () => {});
    mocks.remove.mockResolvedValue(undefined);
    await act(async () =>
      root.render(
        <SessionPicker
          documentId={documentId}
          sessions={[current, other]}
          current={current}
          onCreate={async () => {}}
          onChange={onChange}
          onDeleted={onDeleted}
          onRefresh={onRefresh}
        />,
      ),
    );
    await act(async () =>
      host.querySelector<HTMLButtonElement>('[aria-label="选择会话"]')!.click(),
    );
    const remove = document.querySelector<HTMLElement>(
      '[aria-label="删除会话：其他会话"]',
    )!;
    expect(remove).not.toBeNull();
    await act(async () => remove.click());
    expect(onChange).not.toHaveBeenCalled();
    expect(mocks.remove).not.toHaveBeenCalled();
    await act(async () =>
      [
        ...document.querySelectorAll<HTMLButtonElement>(
          '[data-slot="alert-dialog-action"]',
        ),
      ][0].click(),
    );
    expect(mocks.remove).toHaveBeenCalledWith(documentId, "other");
    expect(onDeleted).toHaveBeenCalledWith("other");
    expect(onChange).not.toHaveBeenCalled();
  },
);
it.each([
  ["doc", "draft-local"],
  ["library", "draft-local"],
  ["doc", "saved-empty-default"],
  ["library", "library-saved-empty"],
])(
  "reuses the current empty conversation on new and allows its deletion: %s / %s",
  async (documentId, sessionId) => {
    mocks.started.mockResolvedValue(false);
    const draft: DocumentChatSession = {
      id: sessionId,
      documentId,
      title: sessionId.startsWith("draft-") ? "新会话" : "默认会话",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    };
    const saved = { ...draft, id: "saved", title: "已保存" };
    const onChange = vi.fn(),
      onDeleted = vi.fn(),
      onCreate = vi.fn(async () => {});
    await act(async () =>
      root.render(
        <SessionPicker
          documentId={documentId}
          sessions={[draft, saved]}
          current={draft}
          onCreate={onCreate}
          onChange={onChange}
          onDeleted={onDeleted}
          onRefresh={async () => {}}
        />,
      ),
    );
    await act(async () => {
      const create = host.querySelector<HTMLButtonElement>(
        '[aria-label="新建会话"]',
      )!;
      create.click();
      create.click();
    });
    expect(onCreate).not.toHaveBeenCalled();
    expect(onChange).not.toHaveBeenCalled();
    expect(mocks.started).toHaveBeenCalledWith(documentId, sessionId);
    await act(async () =>
      host.querySelector<HTMLButtonElement>('[aria-label="选择会话"]')!.click(),
    );
    expect(
      document.querySelector('[data-slot="dropdown-menu-content"]')
        ?.textContent,
    ).not.toContain("未发送");
    await act(async () =>
      document
        .querySelector<HTMLElement>(`[aria-label="删除会话：${draft.title}"]`)!
        .click(),
    );
    if (sessionId.startsWith("draft-")) {
      expect(mocks.remove).not.toHaveBeenCalled();
    } else {
      await act(async () =>
        document
          .querySelector<HTMLButtonElement>(
            '[data-slot="alert-dialog-action"]',
          )!
          .click(),
      );
      expect(mocks.remove).toHaveBeenCalledWith(documentId, sessionId);
    }
    expect(mocks.forget).toHaveBeenCalledWith(documentId, sessionId);
    expect(onDeleted).toHaveBeenCalledWith(sessionId);
    expect(onChange).toHaveBeenCalledWith("saved");
  },
);

it("does not create a conversation in a category or document that was left while history was loading", async () => {
  let ready!: (started: boolean) => void;
  let currentReady!: (started: boolean) => void;
  mocks.started.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        ready = resolve;
      }),
  );
  mocks.started.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        currentReady = resolve;
      }),
  );
  const onCreate = vi.fn(async () => {});
  const render = (id: string) =>
    act(async () =>
      root.render(
        <SessionPicker
          documentId="doc"
          sessions={[]}
          current={{
            id,
            documentId: "doc",
            title: "会话",
            provider: "api",
            createdAt: "now",
            updatedAt: "now",
          }}
          onCreate={onCreate}
          onChange={() => {}}
          onDeleted={() => {}}
          onRefresh={async () => {}}
        />,
      ),
    );
  await render("previous");
  await act(async () =>
    host.querySelector<HTMLButtonElement>('[aria-label="新建会话"]')!.click(),
  );
  await render("current");
  await act(async () =>
    host.querySelector<HTMLButtonElement>('[aria-label="新建会话"]')!.click(),
  );
  await act(async () => ready(true));
  expect(onCreate).not.toHaveBeenCalled();
  expect(
    host.querySelector<HTMLButtonElement>('[aria-label="新建会话"]')!.disabled,
  ).toBe(true);
  await act(async () => currentReady(true));
  expect(onCreate).toHaveBeenCalledTimes(1);
});

it.each(["doc", "library"])(
  "allows renaming an empty draft and creating another conversation after it is named: %s",
  async (documentId) => {
    const current: DocumentChatSession = {
      id: "draft-rename",
      documentId,
      title: "默认会话",
      provider: "api",
      createdAt: "now",
      updatedAt: "now",
    };
    const onCreate = vi.fn(async () => {}),
      onRefresh = vi.fn(async () => {});
    const render = (item: DocumentChatSession) =>
      act(async () =>
        root.render(
          <SessionPicker
            documentId={documentId}
            sessions={[item]}
            current={item}
            onCreate={onCreate}
            onDeleted={() => {}}
            onChange={() => {}}
            onRefresh={onRefresh}
          />,
        ),
      );
    await render(current);
    const rename = host.querySelector<HTMLButtonElement>(
      '[aria-label="重命名会话"]',
    )!;
    expect(rename.disabled).toBe(false);
    await act(async () => rename.click());
    await act(async () =>
      document
        .querySelector("form")!
        .dispatchEvent(
          new Event("submit", { bubbles: true, cancelable: true }),
        ),
    );
    expect(mocks.rename).toHaveBeenCalledWith(
      documentId,
      current.id,
      "默认会话",
    );
    expect(onRefresh).toHaveBeenCalledTimes(1);
    mocks.started.mockResolvedValue(false);
    await render({ ...current, id: "saved-renamed", titleManual: true });
    await act(async () =>
      host.querySelector<HTMLButtonElement>('[aria-label="新建会话"]')!.click(),
    );
    expect(onCreate).toHaveBeenCalledTimes(1);
  },
);
