import { create } from "zustand";
import { api } from "@neoread/api";
import {
  webdavBusy,
  type WebDAVConfig,
  type WebDAVStatus,
} from "@neoread/core";
import {
  flushLibraryPreferences,
  loadLibraryPreferences,
  refreshLibrary,
  refreshTrash,
  useReaderStore,
} from "./store";
import { flushProgress, hasPendingProgress } from "./progress";
import {
  discardPaperNotes,
  importWithPaperNotes,
  flushPaperNotes,
  hasPendingPaperNotes,
} from "./paper-note-state";
import { subscribeLibraryChanges } from "../../../packages/neoread-core/src/library-changes";
import { refreshSyncedChatSessions } from "./chat/session-registry";
import { toast } from "sonner";

export const emptyWebDAVStatus: WebDAVStatus = {
  manual: false,
  dirty: true,
  failed: false,
  phase: "idle",
  conflicts: [],
  deferred: 0,
  transferred: 0,
  changed: [],
  revision: 0,
};
export const useWebDAVSync = create<{
  config?: WebDAVConfig;
  status: WebDAVStatus;
  starting: boolean;
  localFailure?: string;
}>()(() => ({ status: emptyWebDAVStatus, starting: false }));

let notifiedRun = "";
export function setWebDAVStatus(status: WebDAVStatus) {
  if (
    status.runId &&
    status.runId !== notifiedRun &&
    ["complete", "conflict", "failed", "cancelled"].includes(status.phase)
  ) {
    notifiedRun = status.runId;
    if (status.phase === "failed")
      toast.error(status.manual ? "书库同步失败" : "书库自动同步失败", {
        id: "neoread-webdav-sync",
        description: status.error,
        position: "bottom-right",
      });
    else if (
      status.manual &&
      (status.phase === "complete" || status.phase === "conflict")
    )
      toast.success(
        status.conflicts.length
          ? "同步完成，有不同版本可稍后处理"
          : "书库同步完成",
        {
          id: "neoread-webdav-sync",
          position: "bottom-right",
          description:
            status.dirty && !status.conflicts.length
              ? "仍有新的本地修改待同步"
              : undefined,
        },
      );
  }
  let { localFailure } = useWebDAVSync.getState();
  if (
    localFailure !== undefined &&
    status.lastSync &&
    status.lastSync !== localFailure &&
    !status.dirty &&
    status.phase === "complete"
  )
    localFailure = undefined;
  useWebDAVSync.setState({
    localFailure,
    status: {
      ...status,
      dirty: status.dirty || hasPendingProgress() || hasPendingPaperNotes(),
      failed: status.failed || localFailure !== undefined,
    },
  });
}

export async function startWebDAVSync() {
  const current = useWebDAVSync.getState();
  if (current.starting || webdavBusy(current.status.phase)) return;
  useWebDAVSync.setState({ starting: true });
  try {
    await flushLibraryPreferences();
    await flushProgress();
    await flushPaperNotes();
    setWebDAVStatus(await api.runWebdav());
  } catch (error) {
    const { status } = useWebDAVSync.getState();
    useWebDAVSync.setState({
      localFailure: status.lastSync ?? "",
      status: { ...status, failed: true, error: (error as Error).message },
    });
    toast.error("书库同步失败", {
      id: "neoread-webdav-sync",
      description: (error as Error).message,
      position: "bottom-right",
    });
    throw error;
  } finally {
    useWebDAVSync.setState({ starting: false });
    wakeWebDAVSync();
  }
}

export const wakeWebDAVSync = () =>
  window.dispatchEvent(new Event("neoread:webdav-changed"));

export async function loadWebDAVSettings() {
  const [config, status] = await Promise.all([
    api.webdavConfig(),
    api.webdavStatus(),
  ]);
  useWebDAVSync.setState({ config });
  setWebDAVStatus(status);
  return config;
}

/** Run once per renderer, outside the reading/typing render subscriptions. */
export function installWebDAVSync() {
  let active = true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let polling = false;
  let applying = "";
  let seenRevision = 0;
  const refreshViews = async (status: WebDAVStatus) => {
    if (!status.revision || status.revision === seenRevision) return;
    if (status.changed.includes("library")) await loadLibraryPreferences(true);
    await Promise.all([refreshLibrary(), refreshTrash()]);
    const ids = new Set(
      useReaderStore.getState().documents.map((item) => item.id),
    );
    const removed = status.changed.filter(
      (id) => id !== "library" && !ids.has(id),
    );
    discardPaperNotes(removed);
    useReaderStore.getState().closeTabs(removed);
    await refreshSyncedChatSessions(status.changed);
    window.dispatchEvent(
      new CustomEvent("neoread:sync-applied", { detail: status.changed }),
    );
    seenRevision = status.revision;
  };
  const check = async () => {
    if (!active || polling) return;
    polling = true;
    clearTimeout(timer);
    try {
      const status = await api.webdavStatus();
      if (!active) return;
      setWebDAVStatus(status);
      if (status.phase === "ready" && status.applyToken && !applying) {
        applying = status.applyToken;
        let acknowledged = false;
        try {
          await flushLibraryPreferences();
          await flushProgress();
          // Hold the note editors only during the local commit. Network
          // downloads/uploads happen outside this save boundary.
          const result = await importWithPaperNotes(async () => {
            acknowledged = true;
            const committed = await api.applyWebdav(applying);
            await refreshViews(committed);
            return committed;
          }, true);
          if (active) setWebDAVStatus(result);
        } catch (error) {
          // A failed local save must terminate the run without applying remote
          // content; persist its failure so polling can notify and offer retry.
          if (!acknowledged) await api.applyWebdav(applying, false);
          throw error;
        } finally {
          applying = "";
        }
      } else {
        await refreshViews(status);
      }
    } catch {
      // Normal server restarts are handled by the desktop. Sync failures have
      // their own persisted/public status instead of a repeating toast.
    } finally {
      polling = false;
      if (active) {
        const current = useWebDAVSync.getState();
        if (current.config?.account || webdavBusy(current.status.phase))
          timer = setTimeout(
            () => void check(),
            webdavBusy(current.status.phase) ? 2000 : 15000,
          );
      }
    }
  };
  const reload = () => {
    void loadWebDAVSettings()
      .then(() => check())
      .catch(() => {});
  };
  const stopChanges = subscribeLibraryChanges(() => {
    const { status, config } = useWebDAVSync.getState();
    if (status.dirty) return;
    useWebDAVSync.setState({ status: { ...status, dirty: true } });
    if (config?.account && !polling) {
      clearTimeout(timer);
      timer = setTimeout(() => void check(), 2000);
    }
  });
  window.addEventListener("neoread:webdav-changed", reload);
  window.addEventListener("online", reload);
  reload();
  return () => {
    active = false;
    clearTimeout(timer);
    stopChanges();
    window.removeEventListener("neoread:webdav-changed", reload);
    window.removeEventListener("online", reload);
  };
}
