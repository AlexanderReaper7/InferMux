// The JSON of the daemon's /warden/* and infermux-ui's /api/*. Field names
// follow the Go structs' json tags.

export interface Verdict {
  yielded: boolean;
  reason: string;
  since: string | null;
  contended_at: string | null;
  sampled_at: string | null;
}

export interface Manual {
  action: "pause" | "resume";
  own_action: "pause" | "resume";
  at: string;
}

export interface Process {
  pid: number;
  name: string;
  unit: string;
  percent: number;
  vram_mb: number;
  ours: boolean;
  desktop: boolean;
}

export interface Resources {
  foreign_gpu_percent: number | null;
  our_gpu_percent: number;
  desktop_gpu_percent: number;
  our_vram_mb: number;
  vram_used_mb: number;
  vram_total_mb: number;
  vram_free_mb: number | null;
  gpu_percent: number | null;
  culprits: string[];
  processes: Process[];
  sampled_at: string;
  comfyui_jobs: number | null;
}

export interface Policy {
  gpu_busy_percent: number;
  min_free_vram_mb: number;
  resume_quiet_seconds: number;
  poll_seconds: number;
  comfyui_idle_seconds: number;
  interactive_recent_seconds: number;
  enabled: boolean;
}

export interface ConsumerState {
  url: string;
  action: string | null;
  age_seconds: number | null;
  error: string | null;
}

export interface Flight {
  class: "interactive" | "batch";
  path: string;
  age_seconds: number;
}

export interface VerdictState {
  enabled: boolean;
  verdict: Verdict;
  action: "pause" | "resume";
  own: Verdict;
  manual: Manual | null;
  waiting_for_quiet: string[];
  policy: Policy;
  consumers: Record<string, ConsumerState>;
  traffic: {
    paused: boolean;
    in_flight: Flight[];
    last_interactive: string | null;
    refused_batch: number;
    cancelled_batch: number;
  };
  models: Record<string, string>;
  pending_unload: boolean;
  last_unload: string[] | null;
  comfyui: { url: string; busy_at: string; freed: boolean } | null;
  probe_error: string | null;
  resources: Resources | null;
}

export interface Flag {
  name: string;
  value: string | null;
}

export interface Model {
  name: string;
  file: string;
  runtime: string;
  gguf: string;
  flags: Flag[];
  raw: boolean;
  cmd: string;
  ttl: number | null;
  aliases: string[];
  description: string;
  unlisted: boolean;
  comment: string;
}

export interface Consumer {
  name: string;
  url: string;
  announce_path: string;
  timeout_seconds: number;
}

export interface WardenConfig {
  comfyui_url: string;
  our_units: string[] | null;
  desktop_processes: string[] | null;
  batch_api_keys: string[] | null;
  trusted_hosts: string[] | null;
  policy: Policy;
  consumers: Consumer[] | null;
}

export interface UIState {
  models: Model[];
  runtimes: Record<string, string>;
  warden: WardenConfig;
  // Per runtime macro, the K-V pairs with a compiled FlashAttention kernel.
  kv_kernels: Record<string, string[]> | null;
  paths: { models_dir: string; warden_file: string; base_config: string; gguf_dirs: string[] };
}

export interface GGUF {
  path: string;
  bytes: number;
  used_by: string[];
}

export interface GitState {
  repo: string;
  branch: string;
  changes: string[];
  diff: string;
}

export interface BuildState {
  configured: boolean;
  installable: string;
  running: boolean;
  started: string | null;
  ended: string | null;
  ok: boolean | null;
  error: string;
  output: string;
  log: string[] | null;
}
