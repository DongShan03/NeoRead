import {
  Check,
  ChevronDown,
  Cloud,
  CloudOff,
  LoaderCircle,
  Plus,
  Trash2,
} from "lucide-react";
import { api } from "@neoread/api";
import { Switch } from "@neoread/ui/components/switch";
import { useWebDAVSync } from "./webdav-sync";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import type { LibraryCatalogSnapshot } from "../../../packages/neoread-core/src/library-archive";
import { Input } from "@neoread/ui/components/input";
import { doubleClickWindow } from "./DetailSheet";
import type { LibraryMode, WebDAVLibrary } from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@neoread/ui/components/dialog";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "@neoread/ui/components/dropdown-menu";

export const libraryModes: Record<
  LibraryMode,
  { label: string; description: string }
> = {
  papers: { label: "论文库", description: "论文、论文信息与引用" },
  books: { label: "图书库", description: "EPUB 与 PDF 书籍、文章" },
};

export function LibraryModeSwitcher({
  mode,
  onChange,
}: {
  mode: LibraryMode;
  onChange: (mode: LibraryMode) => void;
}) {
  const desktop = window.neoreadDesktop;
  const [catalog, setCatalog] = useState<LibraryCatalogSnapshot>();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("论文库");
  const [deleting, setDeleting] = useState(false);
  const [deleteCloud, setDeleteCloud] = useState(false);
  const syncConfig = useWebDAVSync((state) => state.config);
  const [remote, setRemote] = useState<WebDAVLibrary[]>([]);
  const [remoteLoading, setRemoteLoading] = useState(false);
  const [remoteError, setRemoteError] = useState("");
  const [connecting, setConnecting] = useState<WebDAVLibrary>();
  const remoteRequest = useRef(0);
  const remoteLoaded = useRef(0);
  const configured = !!syncConfig?.passwordSet;
  const cloudConnected = !!syncConfig?.remoteId;
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const saving = useRef(false);
  const canceled = useRef(false);
  useEffect(() => {
    remoteRequest.current++;
    remoteLoaded.current = 0;
    setRemote([]);
    setRemoteLoading(false);
    setRemoteError("");
    setConnecting(undefined);
    return () => {
      remoteRequest.current++;
    };
  }, [syncConfig?.account, configured]);
  const loadRemote = async () => {
    if (!configured || !desktop?.syncRemoteLibrary) return;
    const request = ++remoteRequest.current;
    setRemoteLoading(true);
    setRemoteError("");
    try {
      const libraries = await api.webdavLibraries();
      if (request !== remoteRequest.current) return;
      setRemote(libraries);
      remoteLoaded.current = Date.now();
    } catch (error) {
      if (request === remoteRequest.current)
        setRemoteError((error as Error).message);
    } finally {
      if (request === remoteRequest.current) setRemoteLoading(false);
    }
  };
  useEffect(() => {
    let current = true;
    void desktop
      ?.libraryCatalog?.()
      .then((value) => {
        if (current) setCatalog(value);
      })
      .catch((error) => toast.error(error.message));
    return () => {
      current = false;
      clearTimeout(timer.current);
    };
  }, [desktop]);
  const active = catalog?.libraries.find(
    (entry) => entry.id === catalog.activeId,
  );
  const name = active?.name ?? libraryModes[mode].label;
  const remoteOnly = remote.filter(
    (cloud) =>
      !catalog?.libraries.some(
        (entry) =>
          entry.sync?.remoteId === cloud.id &&
          entry.sync.account === syncConfig?.account,
      ),
  );
  const connect = async () => {
    if (busy || !connecting || !desktop?.syncRemoteLibrary) return;
    setBusy(true);
    try {
      if (await desktop.syncRemoteLibrary(connecting.id))
        setConnecting(undefined);
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const rename = async () => {
    if (
      canceled.current ||
      saving.current ||
      !active ||
      !desktop?.renameLibrary
    )
      return;
    saving.current = true;
    try {
      setCatalog(await desktop.renameLibrary(active.id, draft));
      setEditing(false);
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      saving.current = false;
    }
  };
  const create = async () => {
    if (busy || !desktop?.createLibrary || !newName.trim()) return;
    setBusy(true);
    try {
      if (await desktop.createLibrary(newName.trim())) setCreating(false);
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    if (busy || !active || !desktop?.deleteLibrary) return;
    setBusy(true);
    try {
      if (await desktop.deleteLibrary(active.id, deleteCloud))
        setDeleting(false);
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setBusy(false);
    }
  };
  if (editing)
    return (
      <Input
        autoFocus
        aria-label="书库名称"
        value={draft}
        maxLength={80}
        className="h-9 text-lg font-semibold"
        onChange={(event) => setDraft(event.target.value)}
        onBlur={() => void rename()}
        onKeyDown={(event) => {
          if (event.key === "Enter") {
            event.preventDefault();
            void rename();
          }
          if (event.key === "Escape") {
            canceled.current = true;
            setEditing(false);
          }
        }}
      />
    );
  return (
    <>
      <DropdownMenu
        open={open}
        onOpenChange={(next, details) => {
          if (
            next &&
            !remoteLoading &&
            (remoteError || Date.now() - remoteLoaded.current > 60_000)
          )
            void loadRemote();
          if (next)
            void desktop
              ?.libraryCatalog?.()
              .then(setCatalog)
              .catch((error) => toast.error(error.message));
          clearTimeout(timer.current);
          if (
            next &&
            active &&
            details.reason === "trigger-press" &&
            details.event instanceof MouseEvent
          )
            timer.current = setTimeout(() => setOpen(true), doubleClickWindow);
          else setOpen(next);
        }}
      >
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              className="library-mode-trigger"
              aria-label={`切换书库，当前为${name}`}
              disabled={busy}
              title={active ? "双击重命名" : undefined}
              onDoubleClick={(event) => {
                if (!active || !desktop?.renameLibrary) return;
                event.preventDefault();
                clearTimeout(timer.current);
                setOpen(false);
                canceled.current = false;
                setDraft(name);
                setEditing(true);
              }}
            />
          }
        >
          <span className="min-w-0 truncate">{name}</span>
          <ChevronDown className="size-4 text-muted-foreground" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-60">
          {catalog?.libraries.map((entry) => (
            <DropdownMenuItem
              key={entry.id}
              disabled={busy}
              onClick={() => {
                if (entry.id === catalog.activeId) return;
                setBusy(true);
                void desktop
                  ?.switchLibrary?.(entry.id)
                  .catch((error) => toast.error(error.message))
                  .finally(() => setBusy(false));
              }}
            >
              {(
                entry.id === catalog.activeId
                  ? (syncConfig?.enabled ?? entry.sync?.enabled)
                  : entry.sync?.enabled
              ) ? (
                <Cloud
                  className="size-4 text-muted-foreground"
                  aria-label="已开启同步"
                />
              ) : (
                <CloudOff
                  className="size-4 text-muted-foreground"
                  aria-label="未开启同步"
                />
              )}
              <span className="min-w-0 flex-1 truncate">{entry.name}</span>
              {entry.id === catalog.activeId && <Check className="size-4" />}
            </DropdownMenuItem>
          ))}
          {remoteOnly.map((entry) => (
            <DropdownMenuItem
              key={"cloud-" + entry.id}
              className="text-muted-foreground"
              disabled={busy}
              onClick={() => {
                setOpen(false);
                setConnecting(entry);
              }}
            >
              <Cloud className="size-4" aria-label="云端书库" />
              <span className="min-w-0 flex-1 truncate">{entry.folder}</span>
            </DropdownMenuItem>
          ))}
          {remoteLoading && (
            <DropdownMenuItem disabled>
              <LoaderCircle className="size-4 animate-spin" />
              正在获取云端书库…
            </DropdownMenuItem>
          )}
          {remoteError && (
            <DropdownMenuItem
              className="text-muted-foreground"
              title={remoteError}
              onClick={() => void loadRemote()}
            >
              重新获取云端书库
            </DropdownMenuItem>
          )}
          {desktop?.createLibrary && (
            <DropdownMenuItem
              disabled={busy}
              onClick={() => {
                setOpen(false);
                setNewName("论文库");
                setCreating(true);
              }}
            >
              <Plus className="size-4" />
              新建书库
            </DropdownMenuItem>
          )}
          {desktop?.deleteLibrary && (
            <DropdownMenuItem
              variant="destructive"
              disabled={busy || !catalog || catalog.libraries.length <= 1}
              onClick={() => {
                setOpen(false);
                setDeleteCloud(false);
                setDeleting(true);
              }}
            >
              <Trash2 className="size-4" />
              删除当前书库…
            </DropdownMenuItem>
          )}
          {(!!catalog?.libraries.length || !!desktop?.createLibrary) && (
            <DropdownMenuSeparator />
          )}
          {(Object.keys(libraryModes) as LibraryMode[]).map((key) => (
            <DropdownMenuItem
              key={key}
              className="library-mode-item"
              onClick={() => onChange(key)}
            >
              <span className="min-w-0 flex-1">
                <span className="block text-sm">
                  {catalog
                    ? key === "papers"
                      ? "论文"
                      : "图书"
                    : libraryModes[key].label}
                </span>
                <span className="block text-xs text-muted-foreground">
                  {libraryModes[key].description}
                </span>
              </span>
              {key === mode && <Check className="size-4" />}
            </DropdownMenuItem>
          ))}
        </DropdownMenuContent>
      </DropdownMenu>
      <Dialog
        open={!!connecting}
        onOpenChange={(value) => {
          if (!busy && !value) setConnecting(undefined);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>同步“{connecting?.folder}”到本地？</DialogTitle>
            <DialogDescription>
              创建对应的本地书库并开始下载，默认开启后续双向同步。
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => setConnecting(undefined)}
            >
              取消
            </Button>
            <Button disabled={busy} onClick={() => void connect()}>
              {busy ? "正在连接…" : "同步到本地"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog
        open={creating}
        onOpenChange={(value) => {
          if (!busy) setCreating(value);
        }}
      >
        <DialogContent>
          <form
            className="grid gap-4"
            onSubmit={(event) => {
              event.preventDefault();
              void create();
            }}
          >
            <DialogHeader>
              <DialogTitle>新建书库</DialogTitle>
              <DialogDescription>
                新书库独立保存，默认关闭同步。
              </DialogDescription>
            </DialogHeader>
            <Input
              aria-label="书库名称"
              autoFocus
              maxLength={80}
              value={newName}
              disabled={busy}
              onChange={(event) => setNewName(event.target.value)}
            />
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                disabled={busy}
                onClick={() => setCreating(false)}
              >
                取消
              </Button>
              <Button type="submit" disabled={busy || !newName.trim()}>
                {busy ? "正在创建…" : "创建并切换"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      <Dialog
        open={deleting}
        onOpenChange={(value) => {
          if (!busy) setDeleting(value);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>确认删除“{name}”？</DialogTitle>
            <DialogDescription>
              本机书库及其文件移到系统废纸篓。删除后返回默认书库；删除默认书库时，将切换到剩余书库。
            </DialogDescription>
          </DialogHeader>
          {cloudConnected && (
            <label className="flex items-center justify-between gap-4 text-sm">
              <span>
                同时删除坚果云副本
                <span className="block text-xs text-muted-foreground">
                  云端文件会删除，其他设备停止同步此书库。
                </span>
              </span>
              <Switch
                aria-label="同时删除坚果云副本"
                checked={deleteCloud}
                disabled={busy}
                onCheckedChange={(value) => setDeleteCloud(!!value)}
              />
            </label>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => setDeleting(false)}
            >
              取消
            </Button>
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => void remove()}
            >
              {busy ? "正在删除…" : "确认删除书库"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
