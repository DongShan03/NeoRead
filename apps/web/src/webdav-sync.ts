import { create } from "zustand";
import { api } from "@neoread/api";
import {
  webdavBusy,
  type WebDAVConfig,
  type WebDAVStatus,
} from "@neoread/core";
import {
  loadLibraryPreferences,
  refreshLibrary,
  refreshTrash,
  useReaderStore,
} from "./store";
import { flushProgress } from "./progress";
import { discardPaperNotes, importWithPaperNotes } from "./paper-note-state";
import { refreshSyncedChatSessions } from "./chat/session-registry";

export const emptyWebDAVStatus: WebDAVStatus = {
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
}>()(() => ({ status: emptyWebDAVStatus }));

export const wakeWebDAVSync = () =>
  window.dispatchEvent(new Event("neoread:webdav-changed"));

export async function loadWebDAVSettings() {
  const [config, status] = await Promise.all([
    api.webdavConfig(),
    api.webdavStatus(),
  ]);
  useWebDAVSync.setState({ config, status });
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
      useWebDAVSync.setState({ status });
      if (status.phase === "ready" && status.applyToken && !applying) {
        applying = status.applyToken;
        try {
          await flushProgress();
          // Hold the note editors only during the local commit. Network
          // downloads/uploads happen outside this save boundary.
          const result = await importWithPaperNotes(async () => {
            const committed = await api.applyWebdav(applying);
            await refreshViews(committed);
            return committed;
          }, true);
          if (active) useWebDAVSync.setState({ status: result });
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
  window.addEventListener("neoread:webdav-changed", reload);
  window.addEventListener("online", reload);
  reload();
  return () => {
    active = false;
    clearTimeout(timer);
    window.removeEventListener("neoread:webdav-changed", reload);
    window.removeEventListener("online", reload);
  };
}
