import { api } from "@neoread/api";
import type { Document } from "@neoread/core";
import { toast } from "sonner";
import { refreshLibrary, useReaderStore } from "./store";

const pending = new Set<string>();

export async function reparseDocument(doc: Document) {
  if (pending.has(doc.id)) return;
  pending.add(doc.id);
  const message = toast.loading(`正在开始重新解析翻译《${doc.title}》…`);
  try {
    await api.setAssistance(doc.id, "retranslate", {
      restart: true,
      clearHistory: true,
    });
    useReaderStore.getState().closeTabs([doc.id]);
    await refreshLibrary().catch(() => {});
    toast.success("已加入重新解析翻译队列。", { id: message });
  } catch (error) {
    toast.error((error as Error).message, { id: message });
  } finally {
    pending.delete(doc.id);
  }
}
