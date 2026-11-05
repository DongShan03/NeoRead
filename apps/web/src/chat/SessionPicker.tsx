import { useEffect, useRef, useState, type ReactNode } from "react";
import { Plus, Pencil, Trash2, ChevronDown, Check } from "lucide-react";
import { api } from "@neoread/api";
import type { DocumentChatSession } from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
} from "@neoread/ui/components/dropdown-menu";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
  AlertDialogAction,
} from "@neoread/ui/components/alert-dialog";
import {
  chatSessionBusy,
  chatSessionHasStarted,
  chatSessionRecordId,
  forgetChatSession,
  isDraftSession,
} from "./session-registry";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@neoread/ui/components/dialog";
import { Input } from "@neoread/ui/components/input";
import { toast } from "sonner";

const statusLabels = {
  running: "运行中",
  completed: "已完成",
  failed: "未完成",
  cancelled: "已停止",
  interrupted: "已中断",
};
export function SessionPicker({
  documentId,
  sessions,
  current,
  continueAction,
  onChange,
  onRefresh,
  onCreate,
  onDeleted,
}: {
  documentId: string;
  sessions: DocumentChatSession[];
  current?: DocumentChatSession;
  continueAction?: ReactNode;
  onChange: (id: string) => void;
  onRefresh: () => Promise<void>;
  onCreate: () => Promise<void>;
  onDeleted: (id: string) => void;
}) {
  const [renaming, setRenaming] = useState(false);
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);
  const [open, setOpen] = useState(false);
  const [deleting, setDeleting] = useState<DocumentChatSession>();
  const selection = `${documentId}:${current?.id ?? ""}`;
  const activeSelection = useRef(selection);
  activeSelection.current = selection;
  useEffect(() => {
    setRenaming(false);
    setSaving(false);
  }, [current?.id]);
  const create = async () => {
    setSaving(true);
    try {
      if (current && !(await chatSessionHasStarted(documentId, current.id)))
        return;
      if (activeSelection.current !== selection) return;
      await onCreate();
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      if (activeSelection.current === selection) setSaving(false);
    }
  };
  const remove = async (item: DocumentChatSession) => {
    if (chatSessionBusy(documentId, item.id)) {
      toast.error("请先停止此会话的回答，再删除会话");
      return;
    }
    setSaving(true);
    try {
      const recordId = chatSessionRecordId(documentId, item.id);
      if (recordId) await api.deleteChatSession(documentId, recordId);
      forgetChatSession(documentId, item.id);
      onDeleted(item.id);
      if (current?.id === item.id) {
        const next = sessions.find((s) => s.id !== item.id);
        if (next) onChange(next.id);
      }
      setDeleting(undefined);
      await onRefresh();
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setSaving(false);
    }
  };
  const update = async (title: string) => {
    if (!current) return;
    setSaving(true);
    try {
      await api.updateChatSession(documentId, current.id, { title });
      await onRefresh();
      setRenaming(false);
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setSaving(false);
    }
  };
  return (
    <div className="pointer-events-none absolute inset-x-3 top-2 z-10 min-w-0">
      <div className="flex min-w-0 items-center gap-1 [&>button]:pointer-events-auto">
        <DropdownMenu open={open} onOpenChange={setOpen}>
          <DropdownMenuTrigger
            render={
              <Button
                variant="outline"
                className="h-8 min-w-0 flex-1 justify-between bg-background text-sm dark:bg-background dark:hover:bg-muted"
                aria-label="选择会话"
              />
            }
          >
            <span className="min-w-0 truncate">
              {current?.title ?? "新会话"}
            </span>
            <ChevronDown className="size-4 shrink-0 text-muted-foreground" />
          </DropdownMenuTrigger>
          <DropdownMenuContent className="min-w-60" align="start">
            {sessions.map((s) => (
              <div
                key={s.id}
                className="flex items-center gap-1"
                role="group"
                aria-label={s.title}
              >
                <DropdownMenuItem
                  className="min-w-0 flex-1"
                  aria-current={s.id === current?.id ? true : undefined}
                  onClick={() => onChange(s.id)}
                >
                  <span className="flex size-4 shrink-0 items-center">
                    {s.id === current?.id && <Check className="size-3.5" />}
                  </span>
                  <span className="min-w-0 flex-1 truncate">{s.title}</span>
                  <span className="ml-2 shrink-0 text-xs text-muted-foreground">
                    {s.latestRun ? statusLabels[s.latestRun.status] : ""}
                  </span>
                </DropdownMenuItem>
                <DropdownMenuItem
                  className="size-7 shrink-0 justify-center p-1"
                  variant="destructive"
                  aria-label={`删除会话：${s.title}`}
                  disabled={
                    saving ||
                    s.latestRun?.status === "running" ||
                    chatSessionBusy(documentId, s.id)
                  }
                  onClick={() => {
                    if (isDraftSession(s.id)) void remove(s);
                    else setDeleting(s);
                  }}
                >
                  <Trash2 className="size-3.5" />
                </DropdownMenuItem>
              </div>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
        {continueAction}
        <Button
          variant="ghost"
          size="icon-xs"
          className="bg-background dark:hover:bg-muted disabled:text-muted-foreground disabled:opacity-100"
          aria-label="重命名会话"
          disabled={!current || isDraftSession(current.id) || saving}
          onClick={() => {
            setName(current?.title ?? "");
            setRenaming(true);
          }}
        >
          <Pencil />
        </Button>
        <Button
          variant="ghost"
          size="icon-xs"
          className="bg-background dark:hover:bg-muted disabled:text-muted-foreground disabled:opacity-100"
          aria-label="新建会话"
          disabled={saving}
          onClick={() => void create()}
        >
          <Plus />
        </Button>
      </div>
      <AlertDialog
        open={!!deleting}
        onOpenChange={(value) => {
          if (!value && !saving) setDeleting(undefined);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除此会话？</AlertDialogTitle>
            <AlertDialogDescription>
              “{deleting?.title}”的消息和草稿将被删除。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={saving}>取消</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={saving}
              onClick={() => {
                if (deleting) void remove(deleting);
              }}
            >
              {saving ? "正在删除…" : "删除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <Dialog open={renaming} onOpenChange={setRenaming}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>重命名会话</DialogTitle>
          </DialogHeader>
          <form
            className="flex flex-col gap-3"
            onSubmit={(event) => {
              event.preventDefault();
              void update(name);
            }}
          >
            <Input
              value={name}
              onChange={(event) => setName(event.target.value)}
              maxLength={80}
              aria-label="会话名称"
              autoFocus
            />
            <Button type="submit" disabled={saving || !name.trim()}>
              保存
            </Button>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
