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
  chatSessionRecordId: (_owner: string, id: string) =>
    id.startsWith("draft-") ? undefined : id,
  forgetChatSession: mocks.forget,
  isDraftSession: (id: string) => id.startsWith("draft-"),
}));
let root: Root, host: HTMLDivElement;
beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.clearAllMocks();
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
it.each(["doc", "library"])(
  "reuses the current unsent draft on new and can discard it locally for %s",
  async (documentId) => {
    const draft: DocumentChatSession = {
      id: "draft-local",
      documentId,
      title: "新会话",
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
    await act(async () =>
      host.querySelector<HTMLButtonElement>('[aria-label="选择会话"]')!.click(),
    );
    expect(
      document.querySelector('[data-slot="dropdown-menu-content"]')
        ?.textContent,
    ).not.toContain("未发送");
    await act(async () =>
      document
        .querySelector<HTMLElement>('[aria-label="删除会话：新会话"]')!
        .click(),
    );
    expect(mocks.remove).not.toHaveBeenCalled();
    expect(mocks.forget).toHaveBeenCalledWith(documentId, "draft-local");
    expect(onDeleted).toHaveBeenCalledWith("draft-local");
    expect(onChange).toHaveBeenCalledWith("saved");
  },
);
