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
  // The GGUF and mmproj on Hugging Face, as org/repo/file.gguf; null for local files.
  hf: { model: string; mmproj?: string } | null;
}

export interface Consumer {
  name: string;
  url: string;
  announce_path: string;
  timeout_seconds: number;
}

export interface Remote {
  name: string;
  url: string;
  key_file: string;
}

export interface Key {
  sha256?: string;
  class: "interactive" | "batch";
  // Absent: every model. Empty: none.
  allow?: string[] | null;
}

export interface KeysState {
  file: string;
  secrets: string;
  keys: Record<string, Key>;
}

export interface WardenConfig {
  comfyui_url: string;
  comfyui_unit: string;
  our_units: string[] | null;
  desktop_processes: string[] | null;
  priority_processes: string[] | null;
  host: string;
  keys_file: string;
  remotes: Remote[] | null;
  trusted_hosts: string[] | null;
  policy: Policy;
  consumers: Consumer[] | null;
  stuck: Stuck;
}

// What is done about an agent on a local model that repeats itself (0015).
export interface Stuck {
  annotate_after: number;
  escalate_after: number;
  escalate_to: string;
  refuse_after: number;
}

// A cloud API in llama-swap's peers: (0006, 6). The UI edits only the list.
export interface Peer {
  name: string;
  proxy: string;
  models: string[];
  file: string;
}

export interface UIState {
  models: Model[];
  peers: Peer[];
  runtimes: Record<string, string>;
  warden: WardenConfig;
  // Per runtime macro, the K-V pairs with a compiled FlashAttention kernel.
  kv_kernels: Record<string, string[]> | null;
  daemon_port: string;
  swap_port: string;
  // Whether a model may name its files on Hugging Face (-hf-dir is set).
  hf: boolean;
  paths: { models_dir: string; warden_file: string; base_config: string; gguf_dirs: string[] };
}

export interface Download {
  source: string;
  path: string;
  total: number;
  done: number;
  running: boolean;
  error?: string;
  result?: "downloaded" | "up to date";
  updated: string;
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
  // Commit needs the GPG key's passphrase: a host with no pinentry (0008).
  sign_passphrase: boolean;
}

export interface BuildState {
  configured: boolean;
  installable: string;
  running: boolean;
  started: string | null;
  ended: string | null;
  ok: boolean | null;
  stale: boolean;
  error: string;
  output: string;
  log: string[] | null;
}

// The daemon's /warden/requests (internal/stats): each host's last requests
// for the models it serves, timed as the client sees them, and their summary
// per model. A null number was not reported.
export interface Spread {
  median: number;
  p95: number;
  n: number;
}

export interface RequestStat {
  id: number;
  time: string;
  model: string;
  client: string;
  path: string;
  status: number;
  stream: boolean;
  duration_ms: number;
  ttft_ms: number | null;
  prefill_ms: number | null;
  prompt_tokens: number | null;
  cached_tokens: number | null;
  output_tokens: number | null;
  prefill_per_second: number | null;
  decode_per_second: number | null;
  rates_from?: "llama-server" | "client";
  draft_tokens: number | null;
  draft_accepted: number | null;
  // absent from a host older than 2026-10-06
  request_bytes?: number;
  response_bytes?: number;
}

export interface ModelStat {
  model: string;
  requests: number;
  failed: number;
  ttft_ms: Spread | null;
  prefill_ms: Spread | null;
  wait_ms: Spread | null;
  prefill_per_second: Spread | null;
  decode_per_second: Spread | null;
  cache_share: number | null;
  draft_acceptance: number | null;
}

export interface HostStats {
  host: string;
  requests: RequestStat[] | null;
  models: ModelStat[] | null;
  error?: string;
}
