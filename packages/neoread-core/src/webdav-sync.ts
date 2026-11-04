export interface WebDAVConfig {
  libraryConfigured?: boolean;
  readonly remoteId?: string;
  account: string;
  folder: string;
  deviceName: string;
  intervalMinutes: number;
  enabled: boolean;
  passwordSet: boolean;
}
export type WebDAVConfigInput = WebDAVConfig & { password?: string };
export interface WebDAVLibrary {
  id: string;
  folder: string;
}
export interface WebDAVVariant {
  id: string;
  device: string;
  updated: string;
  deleted: boolean;
  preview: string;
}
export interface WebDAVConflict {
  key: string;
  id: string;
  title: string;
  versions: WebDAVVariant[];
  canKeepBoth: boolean;
}
export interface WebDAVStatus {
  runId?: string;
  manual: boolean;
  dirty: boolean;
  failed: boolean;
  phase:
    | "idle"
    | "scanning"
    | "downloading"
    | "uploading"
    | "ready"
    | "applying"
    | "complete"
    | "conflict"
    | "cancelled"
    | "failed";
  lastSync?: string;
  error?: string;
  conflicts: WebDAVConflict[];
  deferred: number;
  transferred: number;
  totalBytes?: number;
  changed: string[];
  revision: number;
  applyToken?: string;
}
export const webdavIntervals = [5, 10, 15, 30, 60] as const;
export const webdavBusy = (phase: WebDAVStatus["phase"]) =>
  ["scanning", "downloading", "uploading", "ready", "applying"].includes(phase);
