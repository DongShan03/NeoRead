import { Check, RefreshCw, RefreshCwOff, X } from "lucide-react";
import { Button } from "@neoread/ui/components/button";
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@neoread/ui/components/tooltip";
import { webdavBusy } from "@neoread/core";
import { startWebDAVSync, useWebDAVSync } from "./webdav-sync";

export function WebDAVSyncButton() {
  const { config, status, starting } = useWebDAVSync();
  if (!config?.account || !config.passwordSet) return null;
  const enabled = config.enabled;
  const SyncIcon = enabled ? RefreshCw : RefreshCwOff;
  const busy = starting || webdavBusy(status.phase);
  const pending =
    status.dirty || status.conflicts.length > 0 || !status.lastSync;
  const marker = status.failed ? "failed" : pending ? "pending" : "synced";
  const label = busy
    ? "正在同步当前书库"
    : !enabled
      ? "此书库未开启同步，点击设置"
      : status.failed
        ? "同步失败，点击重试"
        : pending
          ? "当前书库未同步，点击同步"
          : "当前书库已同步，点击同步";
  const title =
    enabled && status.conflicts.length
      ? `${label}；${status.conflicts.length} 份内容有冲突，可在同步设置中稍后处理`
      : label;
  const transferred = starting ? 0 : status.transferred;
  const totalBytes = starting ? undefined : status.totalBytes;
  const progress = `已传输 ${(transferred / 1_000_000).toFixed(1)} MB / ${
    totalBytes === undefined
      ? "总量统计中"
      : `共 ${(totalBytes / 1_000_000).toFixed(1)} MB`
  }`;
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            size="icon-sm"
            variant="ghost"
            className="shrink-0"
            aria-label={busy ? `${label}；${progress}` : label}
            aria-busy={busy}
            disabled={busy}
            focusableWhenDisabled
            onClick={() => {
              if (!enabled)
                window.dispatchEvent(new Event("neoread:open-sync-settings"));
              else void startWebDAVSync().catch(() => {});
            }}
          />
        }
      >
        <span
          className="relative block size-[1.2rem]"
          data-sync-state={busy ? "syncing" : !enabled ? "disabled" : marker}
        >
          <SyncIcon
            aria-hidden="true"
            className={`absolute inset-0 !size-full ${enabled && status.conflicts.length ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground"} ${busy ? "animate-spin motion-reduce:animate-none" : ""}`}
            style={{ animationDuration: "1.2s" }}
          />
          {enabled &&
            !busy &&
            (marker === "synced" ? (
              <Check
                aria-hidden="true"
                strokeWidth={3}
                className="absolute left-1/2 top-1/2 !size-[0.6rem] -translate-x-1/2 -translate-y-1/2 text-emerald-600 dark:text-emerald-400"
              />
            ) : marker === "failed" ? (
              <X
                aria-hidden="true"
                strokeWidth={3}
                className="absolute left-1/2 top-1/2 !size-[0.6rem] -translate-x-1/2 -translate-y-1/2 text-destructive"
              />
            ) : (
              <svg
                aria-hidden="true"
                viewBox="0 0 12 12"
                className="absolute left-1/2 top-1/2 !size-[0.6rem] -translate-x-1/2 -translate-y-1/2 text-amber-600 dark:text-amber-400"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
              >
                <path d="M6 2.3v4.5" />
                <circle
                  cx="6"
                  cy="9.3"
                  r=".9"
                  fill="currentColor"
                  stroke="none"
                />
              </svg>
            ))}
        </span>
      </TooltipTrigger>
      <TooltipContent side="bottom" className="flex-col items-start gap-1">
        <span>{title}</span>
        {busy && <span className="tabular-nums">{progress}</span>}
      </TooltipContent>
    </Tooltip>
  );
}
