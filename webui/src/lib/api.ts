import type { BuildState, Download, GGUF, GitState, Key, KeysState, Model, UIState, VerdictState, WardenConfig } from "./types";

// Every request carries X-InferMux: infermux-ui refuses a write without it,
// and a page from another origin cannot add it.
async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "X-InferMux": "webui", ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  if (!res.ok) {
    const detail = (data as { detail?: string } | null)?.detail ?? text ?? res.statusText;
    throw new Error(`${res.status}: ${detail}`);
  }
  return data as T;
}

export const api = {
  state: () => call<UIState>("GET", "/api/state"),
  createModel: (m: Model) => call<Model>("POST", "/api/models", m),
  saveModel: (original: string, m: Model) => call<Model>("PUT", `/api/models/${encodeURIComponent(original)}`, m),
  deleteModel: (name: string) => call("DELETE", `/api/models/${encodeURIComponent(name)}`),
  savePeer: (name: string, models: string[]) => call("PUT", `/api/peers/${encodeURIComponent(name)}`, { models }),
  ggufs: () => call<GGUF[]>("GET", "/api/gguf"),
  downloads: () => call<Download[]>("GET", "/api/downloads"),
  saveWarden: (cfg: WardenConfig) => call<WardenConfig>("PUT", "/api/warden", cfg),
  keys: () => call<KeysState>("GET", "/api/keys"),
  createKey: (name: string, k: Key, passphrase: string) =>
    call<{ key: string }>("POST", `/api/keys/${encodeURIComponent(name)}`, { ...k, passphrase }),
  saveKey: (name: string, k: Key) => call("PUT", `/api/keys/${encodeURIComponent(name)}`, k),
  deleteKey: (name: string, passphrase: string) => call("DELETE", `/api/keys/${encodeURIComponent(name)}`, { passphrase }),
  revealKey: (name: string, passphrase: string) =>
    call<{ key: string }>("POST", `/api/keys/${encodeURIComponent(name)}/reveal`, { passphrase }),
  git: () => call<GitState>("GET", "/api/git"),
  build: () => call<BuildState>("GET", "/api/build"),
  startBuild: () => call<BuildState>("POST", "/api/build"),
  commit: (message: string, passphrase: string) =>
    call<{ commit: string }>("POST", "/api/git/commit", { message, passphrase }),

  verdict: () => call<VerdictState>("GET", "/daemon/warden/verdict"),
  manual: (action: "pause" | "resume" | "auto") => call("POST", "/daemon/warden/manual", { action }),
  forgive: () => call<{ was_owed: boolean }>("POST", "/daemon/warden/forgive"),
  unloadAll: () => call<{ unloaded: string[] }>("POST", "/daemon/warden/unload"),
  cancelBatch: () => call<{ cancelled: number }>("POST", "/daemon/warden/cancel-batch"),
  freeComfyUI: () => call("POST", "/daemon/warden/comfyui/free"),
};

export function ago(iso: string | null | undefined): string {
  if (!iso) return "never";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 90) return `${Math.round(s)} s ago`;
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  return `${(s / 3600).toFixed(1)} h ago`;
}

export function gib(bytes: number): string {
  return `${(bytes / 1024 ** 3).toFixed(2)} GiB`;
}

export function basename(path: string): string {
  return path.split("/").pop() ?? path;
}
