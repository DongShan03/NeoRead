import { api, watchChatRun } from "@neoread/api";
import type {
  ChatRun,
  DocumentChatSession,
  LibraryChatSession,
  LibraryChatScope,
} from "@neoread/core";
import { ChatSession, type ChatTransport } from "./chat-session";
import { refreshAIConfig } from "../store";

const sessions = new Map<string, ChatSession>();
const drafts = new Map<string, DocumentChatSession | LibraryChatSession>();
export const chatCreatedEvent = "neoread:chat-session-created";
export interface ChatSessionCreated {
  documentId: string;
  draftId: string;
  session: DocumentChatSession | LibraryChatSession;
}
export const isDraftSession = (id: string) => id.startsWith("draft-");
const savedDraftId = (documentId: string, draftId: string) =>
  (documentId === "library" ? "library-" : "") + draftId.slice(6);
export function createChatDraft(
  documentId: string,
  provider: string,
  scope?: LibraryChatScope,
) {
  const stamp = new Date().toISOString();
  const item: DocumentChatSession | LibraryChatSession = {
    id: "draft-" + crypto.randomUUID().replaceAll("-", ""),
    documentId,
    title: "新会话",
    provider,
    createdAt: stamp,
    updatedAt: stamp,
    ...(scope ? { scope: structuredClone(scope) } : {}),
  };
  drafts.set(documentId + ":" + item.id, item);
  return item;
}
export function createLibraryChatDraft(
  provider: string,
  scope: LibraryChatScope,
) {
  return createChatDraft("library", provider, scope) as LibraryChatSession;
}
export function listChatDrafts(documentId: string) {
  return [...drafts.values()].filter((item) => item.documentId === documentId);
}
export function chatSessionBusy(documentId: string, id: string) {
  return sessions.get(documentId + ":" + id)?.getSnapshot().busy ?? false;
}
/** A failed first send may have created a record before its response was lost. */
export function chatSessionRecordId(documentId: string, id: string) {
  if (!isDraftSession(id)) return id;
  return sessions.get(documentId + ":" + id)?.getSnapshot().pending
    ? savedDraftId(documentId, id)
    : undefined;
}
function saveChatDraft(
  documentId: string,
  draftId: string,
  item: DocumentChatSession | LibraryChatSession,
) {
  const key = documentId + ":" + draftId;
  if (!drafts.delete(key)) return;
  const store = sessions.get(key);
  if (store) {
    store.sessionId = item.id;
    sessions.set(documentId + ":" + item.id, store);
  }
  announceCreation(documentId, draftId, item);
}
/** Recover a creation whose POST response was lost, preserving its live store. */
export function recoverCreatedChatDrafts(
  documentId: string,
  items: DocumentChatSession[],
) {
  for (const draft of listChatDrafts(documentId)) {
    const saved = items.find(
      (item) => item.id === savedDraftId(documentId, draft.id),
    );
    if (saved) saveChatDraft(documentId, draft.id, saved);
  }
}
export function forgetChatSession(documentId: string, id: string) {
  const key = documentId + ":" + id;
  drafts.delete(key);
  const store = sessions.get(key);
  if (store) {
    store.deleted = true;
    store.dispose();
    for (const [alias, item] of sessions) {
      if (item === store) sessions.delete(alias);
    }
  }
}
function conversationTitle(prompt: string) {
  const text = [...prompt.trim().replace(/\s+/g, " ")];
  return text.slice(0, 32).join("") + (text.length > 32 ? "…" : "");
}
function announceCreation(
  documentId: string,
  draftId: string,
  item: DocumentChatSession | LibraryChatSession,
) {
  // A conversation may finish opening while its viewer is in another tab.
  try {
    if (documentId === "library" && "scope" in item) {
      const key = "neoread.library-chat-selections";
      const selections = JSON.parse(localStorage.getItem(key) ?? "{}");
      const category = item.scope.categoryKey ?? item.scope.category;
      if (selections[category] === draftId) {
        selections[category] = item.id;
        localStorage.setItem(key, JSON.stringify(selections));
      }
    } else {
      const key = "neoread.chat-session." + documentId;
      if (localStorage.getItem(key) === draftId)
        localStorage.setItem(key, item.id);
    }
  } catch {}
  window.dispatchEvent(
    new CustomEvent<ChatSessionCreated>(chatCreatedEvent, {
      detail: { documentId, draftId, session: item },
    }),
  );
}

export async function refreshSyncedChatSessions(documents: string[]) {
  const changed = new Set(documents);
  const stores = new Set(
    [...sessions.entries()]
      .filter(
        ([key, session]) =>
          changed.has(key.slice(0, key.indexOf(":"))) &&
          !session.getSnapshot().busy,
      )
      .map(([, session]) => session),
  );
  await Promise.all([...stores].map((session) => session.refreshSynced()));
}
/** Viewer lifetime is independent of run lifetime. Active stores are never evicted. */
export function getChatSession(
  documentId: string,
  sessionId: string,
): ChatSession {
  const key = `${documentId}:${sessionId}`;
  const existing = sessions.get(key);
  if (existing) return existing;
  const currentId = () => session.sessionId ?? sessionId;
  const draft = drafts.get(key);
  let run: ChatRun | undefined;
  let starting: Promise<ChatRun> | undefined;
  let opening = false;
  const latestRun = async () =>
    isDraftSession(currentId())
      ? undefined
      : (await api.chatSessions(documentId, AbortSignal.timeout(15_000))).find(
          (s) => s.id === currentId(),
        )?.latestRun;
  const io: ChatTransport = {
    messages: (_id, signal) =>
      isDraftSession(currentId())
        ? Promise.resolve([])
        : api.sessionMessages(documentId, currentId(), signal),
    changeMessage: (messageId, change) =>
      api.changeSessionMessage(documentId, currentId(), messageId, change),
    latestRun,
    resume: async (active, signal, emit) => {
      run = active;
      await watchChatRun(documentId, currentId(), active.id, signal, emit);
    },
    stream: async (_id, input, signal, emit) => {
      if (isDraftSession(currentId())) {
        if (!draft) throw new Error("草稿已关闭，请新建会话");
        const options = {
          id: savedDraftId(documentId, sessionId),
          title: conversationTitle(input.prompt),
        };
        opening = true;
        try {
          const created =
            documentId === "library" && "scope" in draft
              ? await api.createLibraryChatSession(
                  input.provider,
                  draft.scope,
                  options,
                )
              : await api.createChatSession(
                  documentId,
                  input.provider,
                  options,
                );
          saveChatDraft(documentId, sessionId, created);
        } finally {
          opening = false;
        }
      }
      if (signal.aborted) return;
      starting = api
        .startChatRun(documentId, currentId(), {
          ...input,
          attachments: input.attachments.map((i) => i.id),
        })
        .catch(async (error) => {
          // POST may have been accepted before its response was lost. Query by
          // stable question ID; never retry the POST automatically.
          const accepted = await latestRun();
          if (accepted && accepted.userMessageId === input.userMessageId)
            return accepted;
          throw error;
        });
      try {
        run = await starting;
        await watchChatRun(documentId, currentId(), run.id, signal, emit);
      } finally {
        starting = undefined;
        void refreshAIConfig().catch(() => {});
      }
    },
    cancel: async () => {
      if (opening || isDraftSession(currentId())) return false;
      const active = starting ? await starting : (run ?? (await latestRun()));
      if (active) await api.cancelChatRun(documentId, currentId(), active.id);
    },
  };
  const session = new ChatSession(documentId, io, sessionId);
  sessions.set(key, session);
  if (sessions.size > 24) {
    for (const [oldKey, old] of sessions) {
      if (oldKey !== key && old.canEvict()) {
        old.dispose();
        for (const [alias, item] of sessions)
          if (item === old) sessions.delete(alias);
        if (sessions.size <= 24) break;
      }
    }
  }
  return session;
}
