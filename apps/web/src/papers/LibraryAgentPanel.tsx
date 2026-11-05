import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import {
  BookOpen,
  Bot,
  ChevronDown,
  Folder,
  LoaderCircle,
  Quote,
  Square,
  X,
} from "lucide-react";
import { api } from "@neoread/api";
import {
  locationLabel,
  type ChatQuote,
  type DocumentSourceReference,
  type LibraryChatScope,
  type LibraryChatSession,
  type Message,
} from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import { Textarea } from "@neoread/ui/components/textarea";
import {
  Popover,
  PopoverContent,
  PopoverTitle,
  PopoverTrigger,
} from "@neoread/ui/components/popover";
import { toast } from "sonner";
import { ModelSelector } from "../ModelSelector";
import { useReaderStore, openLibraryDocument } from "../store";
import { getChatSession } from "../chat/session-registry";
import { AssistantPanel } from "../chat/AssistantPanel";
import { SessionPicker } from "../chat/SessionPicker";
import { ComposerSendButton } from "../chat/ComposerSendButton";
import { ChatCitationNavigation } from "../chat/citations";
import {
  focusSelectionScope,
  selectAllInScope,
} from "../readers/scoped-selection";

const drafts = new Map<string, { text: string; quotes: ChatQuote[] }>();
const sameScope = (a: LibraryChatScope, b: LibraryChatScope) =>
  a.category === b.category &&
  [...a.documentIds].sort().join("\n") === [...b.documentIds].sort().join("\n");

export function LibraryAgentPanel({
  scope,
  onClose,
  onSettings,
}: {
  scope: LibraryChatScope;
  onClose: () => void;
  onSettings: () => void;
}) {
  const provider = useReaderStore((state) => state.aiConfig?.primary || "api");
  const [sessions, setSessions] = useState<LibraryChatSession[]>([]);
  const [selected, setSelected] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const latest = useRef({ scope, provider });
  latest.current = { scope, provider };
  const live = useRef(true);
  const refresh = useCallback(async () => {
    const items = await api.libraryChatSessions(AbortSignal.timeout(15_000));
    if (live.current) setSessions(items);
  }, []);
  const create = useCallback(async () => {
    const { scope, provider } = latest.current;
    if (!scope.documentIds.length)
      throw new Error("当前分类没有可供提问的论文");
    const item = await api.createLibraryChatSession(provider, scope);
    if (!live.current) return;
    setSessions((items) => [item, ...items]);
    setSelected(item.id);
    localStorage.setItem("neoread.library-chat-session", item.id);
  }, []);
  useEffect(() => {
    live.current = true;
    let current = true;
    void api
      .libraryChatSessions(AbortSignal.timeout(15_000))
      .then(async (items) => {
        if (!current) return;
        setSessions(items);
        const previous = localStorage.getItem("neoread.library-chat-session");
        const match =
          items.find((item) => item.id === previous) ??
          items.find((item) => sameScope(item.scope, latest.current.scope));
        if (match) setSelected(match.id);
        else if (latest.current.scope.documentIds.length) await create();
      })
      .catch((failure: Error) => {
        if (current) setError(failure.message);
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
      live.current = false;
    };
  }, [create]);
  const current = sessions.find((item) => item.id === selected);
  useEffect(() => {
    const applied = (event: Event) => {
      const ids = (event as CustomEvent<string[]>).detail;
      if (ids?.includes("library"))
        void refresh().catch((failure) => toast.error(failure.message));
    };
    window.addEventListener("neoread:sync-applied", applied);
    return () => window.removeEventListener("neoread:sync-applied", applied);
  }, [refresh]);
  return (
    <aside
      className="library-agent-pane"
      aria-label="书库 Agent 对话"
      onPointerDownCapture={(event) => focusSelectionScope(event.nativeEvent)}
      onKeyDownCapture={(event) => selectAllInScope(event.nativeEvent)}
    >
      <header className="library-agent-heading">
        <span className="flex min-w-0 items-center gap-2 text-sm font-medium">
          <Bot className="size-4" />
          书库 Agent
        </span>
        <Button
          size="icon-sm"
          variant="ghost"
          aria-label="关闭书库 Agent"
          onClick={onClose}
        >
          <X />
        </Button>
      </header>
      {loading ? (
        <div className="library-agent-empty">
          <LoaderCircle className="size-4 animate-spin" />
          正在加载对话…
        </div>
      ) : error ? (
        <div className="library-agent-empty" role="alert">
          {error}
          <Button
            variant="outline"
            size="sm"
            onClick={() =>
              void refresh()
                .then(() => setError(""))
                .catch((e) => setError(e.message))
            }
          >
            重新加载
          </Button>
        </div>
      ) : (
        <>
          <div className="library-agent-session">
            <SessionPicker
              documentId="library"
              sessions={sessions}
              current={current}
              provider={provider}
              onCreate={create}
              onRefresh={refresh}
              onChange={(id) => {
                setSelected(id);
                localStorage.setItem("neoread.library-chat-session", id);
              }}
            />
          </div>
          {current ? (
            <LibraryConversation
              key={current.id}
              item={current}
              activeScope={scope}
              provider={provider}
              onSettings={onSettings}
              onNew={create}
              onRefresh={refresh}
            />
          ) : (
            <div className="library-agent-empty">
              <p>选择包含论文的分类，开始对话。</p>
              <Button
                variant="outline"
                disabled={!scope.documentIds.length}
                onClick={() =>
                  void create().catch((e) => toast.error(e.message))
                }
              >
                开始对话
              </Button>
            </div>
          )}
        </>
      )}
    </aside>
  );
}

function LibraryConversation({
  item,
  activeScope,
  provider,
  onSettings,
  onNew,
  onRefresh,
}: {
  item: LibraryChatSession;
  activeScope: LibraryChatScope;
  provider: string;
  onSettings: () => void;
  onNew: () => Promise<void>;
  onRefresh: () => Promise<void>;
}) {
  const documents = useReaderStore((state) => state.documents);
  const modelSaving = useReaderStore((state) => state.aiModelSaving);
  const session = useMemo(() => getChatSession("library", item.id), [item.id]);
  const summary = useSyncExternalStore(session.subscribe, session.getSummary);
  const prompt = useRef(drafts.get(item.id)?.text ?? "");
  const changed = useRef<((hasText: boolean) => void) | undefined>(undefined);
  const input = useRef<HTMLTextAreaElement>(null);
  const [quotes, setQuotes] = useState<ChatQuote[]>(
    drafts.get(item.id)?.quotes ?? [],
  );
  const quoteRef = useRef(quotes);
  quoteRef.current = quotes;
  useEffect(() => {
    session.activate();
    void session.load();
    return () => {
      drafts.set(item.id, { text: prompt.current, quotes: quoteRef.current });
    };
  }, [session, item.id]);
  const updateDraft = () => {
    prompt.current = input.current?.value ?? "";
    changed.current?.(!!prompt.current.trim());
  };
  const send = async (suggestion?: string) => {
    const text = suggestion ?? prompt.current;
    if (summary.busy || !summary.loaded || !text.trim() || modelSaving) return;
    const references = [...quotes];
    if (input.current) input.current.value = "";
    prompt.current = "";
    changed.current?.(false);
    setQuotes([]);
    drafts.delete(item.id);
    try {
      await session.send({
        provider,
        prompt: text,
        context: references.map((ref) => ref.text).join("\n\n"),
        references,
        attachments: [],
      });
      await onRefresh();
    } catch (failure) {
      if (
        input.current &&
        !input.current.value &&
        !session.getSnapshot().pending
      ) {
        input.current.value = text;
        updateDraft();
        setQuotes(references);
      }
      toast.error((failure as Error).message);
    }
  };
  const restore = (message: Message) => {
    if (input.current) {
      input.current.value = input.current.value
        ? `${input.current.value}\n\n${message.content}`
        : message.content;
      updateDraft();
      input.current.focus();
    }
    setQuotes((current) => [
      ...current,
      ...(message.references ?? []).filter(
        (ref): ref is ChatQuote => ref.kind === "chat",
      ),
    ]);
  };
  const scopeChanged = !sameScope(item.scope, activeScope);
  return (
    <>
      <div className="library-agent-scope">
        <Popover>
          <PopoverTrigger
            render={<Button variant="ghost" size="sm" className="max-w-full" />}
          >
            <Folder className="shrink-0" />
            <span className="min-w-0 truncate">{item.scope.category}</span>
            <span className="shrink-0 text-muted-foreground">
              {item.scope.documentIds.length} 篇
            </span>
            <ChevronDown className="shrink-0" />
          </PopoverTrigger>
          <PopoverContent
            className="w-80 max-h-80 overflow-y-auto"
            align="start"
          >
            <PopoverTitle className="mb-2 text-sm font-medium">
              本会话的论文范围
            </PopoverTitle>
            {item.scope.documentIds.map((id) => {
              const doc = documents.find((doc) => doc.id === id);
              return (
                <div key={id} className="py-2 text-sm break-words">
                  {doc?.title ?? "已移除的论文"}
                </div>
              );
            })}
          </PopoverContent>
        </Popover>
        {scopeChanged && (
          <Button
            size="xs"
            variant="outline"
            disabled={!activeScope.documentIds.length}
            onClick={() => void onNew().catch((e) => toast.error(e.message))}
          >
            使用当前分类新建
          </Button>
        )}
      </div>
      <AssistantPanel
        session={session}
        onRegenerate={() =>
          void session.regenerate(provider).catch((e) => toast.error(e.message))
        }
        onWithdraw={restore}
        onQuote={(text) =>
          setQuotes((current) => [...current, { kind: "chat", text }])
        }
        regenerateDisabled={modelSaving || summary.busy}
        extras={() => null}
        references={() => null}
        renderAnswer={(message, content) => (
          <LibraryAnswer message={message} sessionId={item.id}>
            {content}
          </LibraryAnswer>
        )}
        empty={
          <div className="library-agent-empty">
            <p className="text-sm text-muted-foreground">
              围绕这 {item.scope.documentIds.length} 篇论文提问
            </p>
            {[
              "梳理这些论文的研究路线与主要分歧",
              "比较各篇论文的方法、实验设置和局限",
            ].map((text) => (
              <Button
                key={text}
                size="sm"
                variant="outline"
                className="h-auto whitespace-normal text-left"
                disabled={!summary.loaded || modelSaving}
                onClick={() => void send(text)}
              >
                {text}
              </Button>
            ))}
          </div>
        }
      />
      <div className="chat-compose">
        {quotes.length > 0 && (
          <div className="quote-chips">
            {quotes.map((quote, i) => (
              <div key={i}>
                <Quote size={12} />
                <span>{quote.text}</span>
                <Button
                  size="icon-xs"
                  variant="ghost"
                  aria-label="移除引用"
                  onClick={() =>
                    setQuotes((current) =>
                      current.filter((_, index) => index !== i),
                    )
                  }
                >
                  <X />
                </Button>
              </div>
            ))}
          </div>
        )}
        <Textarea
          ref={input}
          defaultValue={prompt.current}
          aria-label="向书库 Agent 提问"
          placeholder="比较论文、梳理思路、撰写综述…"
          spellCheck={false}
          autoCorrect="off"
          autoCapitalize="off"
          onInput={updateDraft}
          onKeyDown={(event) => {
            if (
              event.key === "Enter" &&
              !event.shiftKey &&
              !event.altKey &&
              !event.nativeEvent.isComposing &&
              event.nativeEvent.keyCode !== 229
            ) {
              event.preventDefault();
              if (!event.repeat) void send();
            }
          }}
        />
        <div className="compose-footer">
          <ModelSelector disabled={summary.busy} onSettings={onSettings} />
          {summary.busy ? (
            <Button
              size="icon-sm"
              variant="secondary"
              aria-label="停止回答"
              onClick={() => session.cancel()}
            >
              <Square className="size-3" />
            </Button>
          ) : (
            <ComposerSendButton
              prompt={prompt}
              changed={changed}
              available={summary.loaded && !modelSaving}
              hasImages={false}
              onSend={() => void send()}
            />
          )}
        </div>
      </div>
    </>
  );
}

function LibraryAnswer({
  message,
  sessionId,
  children,
}: {
  message: Message;
  sessionId: string;
  children: ReactNode;
}) {
  const refs = (message.references ?? []).filter(
    (ref): ref is DocumentSourceReference =>
      ref.kind !== "chat" && !!ref.sourceId && !!ref.documentId,
  );
  const [pending, setPending] = useState("");
  const visit = async (source: string) => {
    if (pending) return;
    setPending(source);
    try {
      const verified = await api.validateLibraryEvidence(
        sessionId,
        message.id,
        source,
      );
      const doc = useReaderStore
        .getState()
        .documents.find((doc) => doc.id === verified.documentId);
      if (!doc) throw new Error("论文已移除，请刷新书库");
      await openLibraryDocument(doc, {
        id: doc.id,
        ...(verified.location.type === "pdf"
          ? { page: verified.location.page }
          : {}),
      });
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setPending("");
    }
  };
  return (
    <ChatCitationNavigation.Provider
      value={{
        sourceIds: refs.map((ref) => ref.sourceId!),
        previews: Object.fromEntries(
          refs.map((ref) => [
            ref.sourceId!,
            {
              text: ref.text,
              label: `${ref.documentTitle} · ${locationLabel(ref.location)}`,
            },
          ]),
        ),
        navigate: (id) => void visit(id),
      }}
    >
      {children}
      {refs.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-1" aria-label="论文来源">
          {refs.map((ref) => (
            <Button
              key={ref.sourceId}
              variant="outline"
              size="xs"
              className="max-w-full"
              title={`${ref.documentTitle} · ${locationLabel(ref.location)}`}
              disabled={!!pending}
              onClick={() => void visit(ref.sourceId!)}
            >
              <BookOpen className="shrink-0 size-3" />
              <span className="truncate">
                [{ref.sourceId}] {ref.documentTitle}
              </span>
              <span className="shrink-0 text-muted-foreground">
                {locationLabel(ref.location)}
              </span>
            </Button>
          ))}
        </div>
      )}
    </ChatCitationNavigation.Provider>
  );
}
