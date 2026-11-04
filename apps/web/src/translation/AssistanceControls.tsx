import { useEffect, useRef, useState } from "react";
import {
  Check,
  FileScan,
  FileSearch,
  Pause,
  RotateCcw,
  Languages,
} from "lucide-react";
import type { EPUBLocation, LibraryMode, Processing } from "@neoread/core";
import { api } from "@neoread/api";
import { Button } from "@neoread/ui/components/button";
import {
  AlertDialog,
  AlertDialogTrigger,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
} from "@neoread/ui/components/alert-dialog";
import { toast } from "sonner";
import type { PageRange } from "./chapters";
import { useReaderStore } from "../store";

type Action = "start" | "pause" | "resume" | "chapter";

/**
 * Papers translate the whole document with start, pause and resume. Books
 * translate the current chapter on request; the server caps each request.
 */
export function AssistanceControls({
  documentId,
  processing,
  library = "papers",
  chapter,
  epubChapter,
}: {
  documentId: string;
  processing?: Processing;
  library?: LibraryMode;
  /** The chapter at the reading position, read when the button is pressed. */
  chapter?: () => PageRange | undefined;
  epubChapter?: () => EPUBLocation | undefined;
}) {
  const [saved, setSaved] = useState<Processing>();
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const serial = useRef(0);
  const job =
    saved &&
    (!processing ||
      Date.parse(saved.updatedAt) > Date.parse(processing.updatedAt))
      ? saved
      : processing;
  useEffect(() => {
    let alive = true;
    setSaved(undefined);
    setBusy(false);
    setConfirming(false);
    void api
      .assistance(documentId)
      .then((p) => {
        if (alive) setSaved(p);
      })
      .catch(() => {});
    return () => {
      alive = false;
      serial.current++;
    };
  }, [documentId]);
  const complete = job?.status === "complete";
  const paperParsing =
    library === "papers" &&
    !epubChapter &&
    job?.phase === "learning" &&
    !job.retranslating &&
    !complete;
  const parsingPending = paperParsing && job.status !== "failed";
  const [action, label]: [Action, string] =
    job?.enabled && job.status === "failed"
      ? ["resume", "重试翻译"]
      : job?.enabled && !complete
        ? ["pause", "暂停翻译"]
        : !job?.enabled
          ? epubChapter &&
            (!job?.startedAt || job.translating?.status === "complete")
            ? ["chapter", "翻译本章"]
            : job?.startedAt && !job.manualTranslation
              ? ["resume", "继续翻译"]
              : ["start", "翻译"]
          : library === "books"
            ? ["chapter", "翻译本章"]
            : ["start", "翻译完成"];
  const finished = complete && action !== "chapter";
  const actionLabel = paperParsing
    ? job.status === "failed"
      ? "重试解析"
      : job.status === "running"
        ? "解析中"
        : "等待解析"
    : job?.retranslating
      ? action === "pause"
        ? "暂停重译"
        : "继续重译"
      : label;
  const run = async () => {
    if (action !== "chapter") return api.setAssistance(documentId, action);
    if (epubChapter) {
      const location = epubChapter();
      if (!location?.href) throw new Error("正文仍在加载，请稍后再试");
      await api.translateEPUBChapter(documentId, location);
      return api.assistance(documentId);
    }
    const range = chapter?.();
    if (!range) return undefined;
    const result = await api.translateRange(
      documentId,
      range.fromPage,
      range.toPage,
    );
    if (result.parsing) toast("正在解析本章，完成后开始翻译");
    return api.assistance(documentId);
  };
  return (
    <div className="assistance-controls">
      <Button
        variant="ghost"
        size="sm"
        disabled={busy || !job || finished || parsingPending}
        title={
          action === "chapter"
            ? "翻译当前完整章节，复用已完成译文"
            : job?.enabled
              ? job.detail
              : epubChapter
                ? "继续已请求章节的翻译"
                : "使用当前 AI 连接翻译全文与公式"
        }
        onClick={async () => {
          const operation = ++serial.current;
          setBusy(true);
          try {
            const next = await run();
            if (next && operation === serial.current) setSaved(next);
          } catch (e) {
            if (operation === serial.current) toast.error((e as Error).message);
          } finally {
            if (operation === serial.current) setBusy(false);
          }
        }}
      >
        {parsingPending ? (
          <FileSearch />
        ) : finished ? (
          <Check />
        ) : action === "pause" ? (
          <Pause />
        ) : action === "resume" && job?.status === "failed" ? (
          <RotateCcw />
        ) : (
          <Languages />
        )}
        {actionLabel}
      </Button>
      {finished && library === "papers" && !epubChapter && (
        <AlertDialog open={confirming} onOpenChange={setConfirming}>
          <AlertDialogTrigger
            render={
              <Button
                variant="ghost"
                size="icon-sm"
                disabled={busy}
                title="重新解析翻译"
                aria-label="重新解析翻译"
              />
            }
          >
            <FileScan />
          </AlertDialogTrigger>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>重新解析翻译全文？</AlertDialogTitle>
              <AlertDialogDescription>
                将清理历史解析、图片、公式和译文，再按当前 Agent
                模型重新解析翻译。原文件、笔记、批注和聊天记录保留。
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
              <Button
                disabled={busy}
                onClick={async () => {
                  const operation = ++serial.current;
                  setBusy(true);
                  try {
                    const next = await api.setAssistance(
                      documentId,
                      "retranslate",
                      { restart: true, clearHistory: true },
                    );
                    if (operation === serial.current) {
                      setConfirming(false);
                      setSaved(next);
                      useReaderStore.getState().closeTabs([documentId]);
                    }
                  } catch (e) {
                    if (operation === serial.current)
                      toast.error((e as Error).message);
                  } finally {
                    if (operation === serial.current) setBusy(false);
                  }
                }}
              >
                {busy ? "正在开始…" : "开始重新解析翻译"}
              </Button>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  );
}
