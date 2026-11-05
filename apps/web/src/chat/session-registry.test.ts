// @vitest-environment jsdom
import { beforeEach, afterEach, expect, it, vi } from "vitest";
import type { Message } from "@neoread/core";
import { api, watchChatRun } from "@neoread/api";
import {
  createChatDraft,
  createLibraryChatDraft,
  listChatDrafts,
  getChatSession,
  forgetChatSession,
  chatCreatedEvent,
  chatSessionRecordId,
  recoverCreatedChatDrafts,
} from "./session-registry";
vi.mock("../store", () => ({ refreshAIConfig: vi.fn(async () => {}) }));
vi.mock("@neoread/api", () => ({
  api: {
    createChatSession: vi.fn(),
    createLibraryChatSession: vi.fn(),
    chatSessions: vi.fn(),
    sessionMessages: vi.fn(),
    startChatRun: vi.fn(),
    cancelChatRun: vi.fn(),
  },
  watchChatRun: vi.fn(),
}));
const cleanup: Array<{ owner: string; id: string }> = [];
let history: Message[] = [];
beforeEach(() => {
  vi.clearAllMocks();
  history = [];
  vi.stubGlobal("requestAnimationFrame", (fn: FrameRequestCallback) =>
    setTimeout(() => fn(0), 0),
  );
  vi.stubGlobal("cancelAnimationFrame", clearTimeout);
  vi.mocked(api.chatSessions).mockResolvedValue([]);
  vi.mocked(api.sessionMessages).mockImplementation(async () => history);
  vi.mocked(api.createChatSession).mockImplementation(
    async (owner, provider, options) => ({
      id: options!.id!,
      documentId: owner,
      title: options!.title!,
      provider,
      createdAt: "now",
      updatedAt: "now",
    }),
  );
  vi.mocked(api.createLibraryChatSession).mockImplementation(
    async (provider, scope, options) => ({
      id: options!.id!,
      documentId: "library",
      title: options!.title!,
      provider,
      scope,
      createdAt: "now",
      updatedAt: "now",
    }),
  );
  vi.mocked(api.startChatRun).mockImplementation(async (owner, id, input) => {
    history = [
      {
        id: input.userMessageId!,
        documentId: owner,
        sessionId: id,
        role: "user",
        content: input.prompt,
        createdAt: "now",
      },
      {
        id: "answer",
        documentId: owner,
        sessionId: id,
        role: "assistant",
        content: "回答",
        createdAt: "now",
      },
    ];
    return {
      id: "run",
      documentId: owner,
      sessionId: id,
      userMessageId: input.userMessageId!,
      provider: input.provider,
      status: "completed",
      startedAt: "now",
    };
  });
  vi.mocked(watchChatRun).mockImplementation(
    async (_owner, _session, _run, _signal, emit) => {
      emit({ event: "delta", data: { text: "回答" } });
      emit({ event: "done", data: { ok: true } });
    },
  );
});
afterEach(() => {
  for (const item of cleanup.splice(0)) forgetChatSession(item.owner, item.id);
  vi.unstubAllGlobals();
});
const input = {
  provider: "codex",
  prompt: "  比较\n这两篇论文的实验设置  ",
  context: "",
  references: [],
  attachments: [],
};
it.each([false, true])(
  "persists a draft only on first send, names it, and preserves the live store: library=%s",
  async (library) => {
    const owner = library ? "library" : "doc-" + crypto.randomUUID();
    const draft = library
      ? createLibraryChatDraft("api", {
          category: "测试",
          categoryKey: "folder:测试",
          documentIds: ["alpha"],
        })
      : createChatDraft(owner, "api");
    cleanup.push({ owner, id: draft.id });
    const session = getChatSession(owner, draft.id);
    await session.load();
    expect(api.createChatSession).not.toHaveBeenCalled();
    expect(api.createLibraryChatSession).not.toHaveBeenCalled();
    expect(api.sessionMessages).not.toHaveBeenCalled();
    const created = vi.fn();
    window.addEventListener(chatCreatedEvent, created);
    try {
      await session.send(input);
      const savedId = (library ? "library-" : "") + draft.id.slice(6);
      cleanup.push({ owner, id: savedId });
      const create = library
        ? api.createLibraryChatSession
        : api.createChatSession;
      expect(create).toHaveBeenCalledTimes(1);
      expect(vi.mocked(create).mock.calls[0].at(-1)).toEqual({
        id: savedId,
        title: "比较 这两篇论文的实验设置",
      });
      expect(session.sessionId).toBe(savedId);
      expect(getChatSession(owner, savedId)).toBe(session);
      expect(session.viewId).toBe(draft.id);
      expect(listChatDrafts(owner)).not.toContainEqual(draft);
      expect(created).toHaveBeenCalledTimes(1);
      await session.send({ ...input, prompt: "继续比较" });
      expect(create).toHaveBeenCalledTimes(1);
      expect(api.startChatRun).toHaveBeenCalledTimes(2);
    } finally {
      window.removeEventListener(chatCreatedEvent, created);
    }
  },
);
it("retries a lost creation response using the same identity rather than creating a second conversation", async () => {
  const owner = "doc-" + crypto.randomUUID(),
    draft = createChatDraft(owner, "api");
  cleanup.push({ owner, id: draft.id });
  const session = getChatSession(owner, draft.id);
  await session.load();
  vi.mocked(api.createChatSession).mockRejectedValueOnce(
    new Error("connection lost"),
  );
  await session.send(input);
  expect(session.getSnapshot().pending?.phase).toBe("unconfirmed");
  await session.regenerate("codex");
  expect(api.createChatSession).toHaveBeenCalledTimes(2);
  expect(vi.mocked(api.createChatSession).mock.calls[0][2]?.id).toBe(
    vi.mocked(api.createChatSession).mock.calls[1][2]?.id,
  );
  expect(api.startChatRun).toHaveBeenCalledTimes(1);
  cleanup.push({ owner, id: session.sessionId! });
});

it("recovers a lost creation response from the list without duplicating or discarding the pending question", async () => {
  const owner = "doc-" + crypto.randomUUID(),
    draft = createChatDraft(owner, "api");
  cleanup.push({ owner, id: draft.id });
  const session = getChatSession(owner, draft.id);
  await session.load();
  expect(chatSessionRecordId(owner, draft.id)).toBeUndefined();
  vi.mocked(api.createChatSession).mockRejectedValueOnce(
    new Error("connection lost"),
  );
  await session.send(input);
  const savedId = draft.id.slice(6);
  expect(chatSessionRecordId(owner, draft.id)).toBe(savedId);
  recoverCreatedChatDrafts(owner, [{ ...draft, id: savedId }]);
  expect(listChatDrafts(owner)).toHaveLength(0);
  expect(getChatSession(owner, savedId)).toBe(session);
  expect(session.getSnapshot().pending?.input.prompt).toBe(input.prompt);
  await session.regenerate("codex");
  expect(api.createChatSession).toHaveBeenCalledTimes(1);
  expect(api.startChatRun).toHaveBeenCalledWith(
    owner,
    savedId,
    expect.objectContaining({ prompt: input.prompt }),
  );
  cleanup.push({ owner, id: savedId });
});

it.each([false, true])(
  "stops during first-message creation before starting the provider even if the list already found the saved record: %s",
  async (recovered) => {
    const owner = "doc-" + crypto.randomUUID(),
      draft = createChatDraft(owner, "api");
    cleanup.push({ owner, id: draft.id });
    const session = getChatSession(owner, draft.id);
    await session.load();
    let ready!: (value: any) => void;
    vi.mocked(api.createChatSession).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          ready = resolve;
        }),
    );
    const sent = session.send(input);
    await Promise.resolve();
    if (recovered)
      recoverCreatedChatDrafts(owner, [{ ...draft, id: draft.id.slice(6) }]);
    session.cancel();
    await Promise.resolve();
    ready({
      id: draft.id.slice(6),
      documentId: owner,
      title: "test",
      provider: "codex",
      createdAt: "now",
      updatedAt: "now",
    });
    await sent;
    expect(api.startChatRun).not.toHaveBeenCalled();
    expect(session.getSnapshot().busy).toBe(false);
    expect(session.getSnapshot().pending?.phase).toBe("cancelled");
    cleanup.push({ owner, id: session.sessionId! });
  },
);
