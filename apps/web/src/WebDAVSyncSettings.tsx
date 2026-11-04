import { useEffect, useState } from "react";
import { Cloud, LoaderCircle, RefreshCw } from "lucide-react";
import { api } from "@neoread/api";
import {
  webdavBusy,
  webdavIntervals,
  type WebDAVConfig,
  type WebDAVConflict,
} from "@neoread/core";
import { Button } from "@neoread/ui/components/button";
import { Input } from "@neoread/ui/components/input";
import { Switch } from "@neoread/ui/components/switch";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@neoread/ui/components/dialog";
import { toast } from "sonner";
import {
  loadWebDAVSettings,
  useWebDAVSync,
  wakeWebDAVSync,
  startWebDAVSync,
  setWebDAVStatus,
} from "./webdav-sync";

const defaults: WebDAVConfig = {
  account: "",
  folder: "论文库",
  deviceName: "这台设备",
  intervalMinutes: 15,
  enabled: false,
  passwordSet: false,
};
const phases: Record<string, string> = {
  idle: "尚未同步",
  scanning: "正在扫描变更…",
  downloading: "正在接收云端变更…",
  uploading: "正在上传变更…",
  ready: "正在保存阅读状态…",
  applying: "正在更新本地书库…",
  complete: "同步完成",
  conflict: "有内容存在不同版本，可稍后处理",
  cancelled: "同步已停止",
  failed: "同步未完成",
};

export function WebDAVSyncSettings({ open }: { open: boolean }) {
  const status = useWebDAVSync((state) => state.status);
  const [draft, setDraft] = useState(defaults);
  const [password, setPassword] = useState("");
  const [verified, setVerified] = useState<{
    account: string;
    password: string;
  } | null>(null);
  const [loading, setLoading] = useState(true);
  const [operation, setOperation] = useState("");
  const [error, setError] = useState("");
  const [conflict, setConflict] = useState<WebDAVConflict>();
  const running = webdavBusy(status.phase);
  useEffect(() => {
    if (!open) return;
    let current = true;
    setLoading(true);
    setVerified(null);
    void Promise.all([
      loadWebDAVSettings(),
      window.neoreadDesktop?.libraryCatalog?.(),
    ])
      .then(([config, catalog]) => {
        if (!current) return;
        const local = catalog?.libraries.find(
          (item) => item.id === catalog.activeId,
        );
        setDraft(
          config.account && config.libraryConfigured !== false
            ? config
            : {
                ...config,
                enabled: false,
                folder: local?.name || "论文库",
              },
        );
        setPassword("");
        setError("");
      })
      .catch((failure) => {
        if (current) setError(failure.message);
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [open]);
  const run = async (name: string, action: () => Promise<void>) => {
    if (operation) return;
    setOperation(name);
    setError("");
    try {
      await action();
    } catch (failure) {
      setError((failure as Error).message);
    } finally {
      setOperation("");
      if (name !== "sync") wakeWebDAVSync();
    }
  };
  const toggle = (enabled: boolean) => {
    if (!enabled) setVerified(null);
    if (enabled || !useWebDAVSync.getState().config?.enabled) {
      setDraft({ ...draft, enabled });
      if (!enabled) setPassword("");
      return;
    }
    void run("disconnect", async () => {
      await api.disconnectWebdav();
      setPassword("");
      useWebDAVSync.setState({ localFailure: undefined });
      const disconnected = await loadWebDAVSettings();
      setDraft({ ...disconnected, folder: draft.folder, enabled: false });
      toast.success("已关闭此书库同步，云端数据保留");
    });
  };
  const input = { ...draft, password };
  const connectionVerified =
    verified?.account === draft.account.trim() &&
    verified?.password === password;
  return (
    <section>
      <div className="mb-3 flex items-center justify-between gap-3">
        <h3 className="flex items-center gap-2 text-sm font-medium">
          <Cloud className="size-4" />
          坚果云同步
        </h3>
        <Switch
          aria-label="同步当前书库"
          checked={draft.enabled}
          disabled={loading || !!operation || running}
          onCheckedChange={(enabled) => toggle(!!enabled)}
        />
      </div>
      <p className="mb-4 text-xs leading-5 text-muted-foreground">
        仅同步当前独立书库，包含其中的论文、图书、批注笔记、译文和已保存的对话。其他书库需分别开启。
      </p>
      {draft.enabled && (
        <div className="grid gap-3">
          <label className="grid gap-1.5 text-sm">
            坚果云账号
            <Input
              type="email"
              autoComplete="username"
              value={draft.account}
              disabled={loading || !!operation || running}
              onChange={(event) => {
                setVerified(null);
                setDraft({ ...draft, account: event.target.value });
              }}
            />
          </label>
          <label className="grid gap-1.5 text-sm">
            第三方应用密码
            <Input
              type="password"
              autoComplete="new-password"
              value={password}
              disabled={loading || !!operation || running}
              placeholder={
                draft.passwordSet
                  ? "已保存，留空保持不变"
                  : "填写坚果云生成的应用密码"
              }
              onChange={(event) => {
                setVerified(null);
                setPassword(event.target.value);
              }}
            />
          </label>
          <p className="text-xs leading-5 text-muted-foreground">
            在坚果云网页的“账户信息 → 安全选项 → 第三方应用管理”中生成应用密码。
          </p>
          <div className="text-sm">
            同步目录
            <p className="mt-1 break-all text-xs text-muted-foreground">
              NeoRead/{draft.folder}/
            </p>
            <p className="mt-1 text-xs text-muted-foreground">
              测试连接时创建，跟随本地书库重命名。
            </p>
          </div>
          <label className="grid gap-1.5 text-sm">
            设备名称
            <Input
              value={draft.deviceName}
              disabled={loading || !!operation || running}
              onChange={(event) =>
                setDraft({ ...draft, deviceName: event.target.value })
              }
            />
          </label>
          <label className="grid gap-1.5 text-sm">
            同步间隔（分钟）
            <Input
              type="number"
              min={1}
              max={525600}
              step={1}
              list="neoread-webdav-intervals"
              value={draft.intervalMinutes}
              disabled={loading || !!operation || running}
              onChange={(event) =>
                setDraft({
                  ...draft,
                  intervalMinutes: Number(event.target.value),
                })
              }
            />
            <datalist id="neoread-webdav-intervals">
              {webdavIntervals.map((value) => (
                <option value={value} key={value}>
                  {value} 分钟
                </option>
              ))}
            </datalist>
          </label>
          <div className="flex flex-wrap gap-2">
            <Button
              disabled={
                loading ||
                !!operation ||
                running ||
                !draft.account ||
                (!password && !draft.passwordSet) ||
                !connectionVerified
              }
              title={connectionVerified ? undefined : "请先测试连接"}
              onClick={() =>
                void run("save", async () => {
                  const saved = await api.saveWebdavConfig(input);
                  setDraft(saved);
                  setPassword("");
                  setVerified({ account: saved.account.trim(), password: "" });
                  useWebDAVSync.setState({ config: saved });
                  toast.success("同步设置已保存");
                })
              }
            >
              {operation === "save" && (
                <LoaderCircle className="animate-spin" />
              )}
              保存
            </Button>
            <Button
              variant="outline"
              disabled={
                loading ||
                !!operation ||
                running ||
                !draft.account ||
                (!password && !draft.passwordSet)
              }
              onClick={() =>
                void run("test", async () => {
                  setVerified(null);
                  await api.testWebdav(input);
                  setVerified({ account: draft.account.trim(), password });
                  toast.success("坚果云读写正常");
                })
              }
            >
              {operation === "test" && (
                <LoaderCircle className="animate-spin" />
              )}
              测试连接
            </Button>
            {running ? (
              <Button
                variant="outline"
                disabled={!!operation}
                onClick={() =>
                  void run("cancel", async () => {
                    await api.cancelWebdav();
                  })
                }
              >
                停止同步
              </Button>
            ) : (
              <Button
                variant="outline"
                disabled={
                  loading ||
                  !!operation ||
                  !draft.passwordSet ||
                  draft.libraryConfigured === false
                }
                onClick={() =>
                  void run("sync", async () => {
                    await startWebDAVSync();
                  })
                }
              >
                <RefreshCw />
                立即同步
              </Button>
            )}
          </div>
          <div
            className="text-xs leading-5 text-muted-foreground"
            role="status"
          >
            {running && (
              <LoaderCircle className="mr-1 inline size-3 animate-spin" />
            )}
            {phases[status.phase]}
            {status.transferred > 0 && (
              <span className="ml-2">
                已传输 {(status.transferred / (1024 * 1024)).toFixed(1)} MiB
              </span>
            )}
            {status.lastSync && (
              <span className="ml-2">
                上次同步：{new Date(status.lastSync).toLocaleString()}
              </span>
            )}
            {status.deferred > 0 && (
              <p>{status.deferred} 份文档正在处理，将在后续同步时继续。</p>
            )}
          </div>
          {status.conflicts.map((item) => (
            <div
              className="flex items-center justify-between gap-3 rounded-md border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-sm"
              key={item.key}
            >
              <span className="min-w-0 truncate">{item.title}</span>
              <Button
                variant="outline"
                size="sm"
                onClick={() => setConflict(item)}
              >
                选择版本
              </Button>
            </div>
          ))}
        </div>
      )}
      {(error || status.error) && (
        <p role="alert" className="mt-3 text-xs text-destructive">
          {error || status.error}
        </p>
      )}
      <Dialog
        open={!!conflict}
        onOpenChange={(value) => {
          if (!value) setConflict(undefined);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{conflict?.title}</DialogTitle>
            <DialogDescription>
              {conflict?.canKeepBoth
                ? "可以将各设备的笔记分段保存，也可以只使用一个版本。此提示可关闭，稍后再处理。"
                : "不同设备修改了同一篇文档，可以选择要使用的版本，也可以稍后处理。"}
            </DialogDescription>
          </DialogHeader>
          {conflict?.canKeepBoth && (
            <Button
              variant="outline"
              disabled={!!operation || running}
              onClick={() =>
                void run("keepBoth", async () => {
                  setWebDAVStatus(
                    await api.keepWebdavNotes(conflict.key, conflict.id),
                  );
                  setConflict(undefined);
                })
              }
            >
              都保留
            </Button>
          )}
          {conflict?.versions.map((version) => (
            <div key={version.id} className="grid gap-2 rounded-md border p-3">
              <p className="text-sm font-medium">
                {version.device} · {new Date(version.updated).toLocaleString()}
              </p>
              <p className="whitespace-pre-wrap text-xs text-muted-foreground">
                {version.deleted
                  ? "此版本已永久删除文档"
                  : version.preview || "文档信息、批注或解析结果有更新"}
              </p>
              <Button
                variant="outline"
                disabled={!!operation || running}
                onClick={() =>
                  void run("resolve", async () => {
                    const next = await api.resolveWebdav(
                      conflict.key,
                      conflict.id,
                      version.id,
                    );
                    setWebDAVStatus(next);
                    setConflict(undefined);
                  })
                }
              >
                只使用此版本
              </Button>
            </div>
          ))}
          <Button variant="ghost" onClick={() => setConflict(undefined)}>
            稍后处理
          </Button>
        </DialogContent>
      </Dialog>
    </section>
  );
}
