// The JSON of the daemon's /warden/* and infermux-ui's /api/*. Field names
// follow the Go structs' json tags.

export type { Schema } from "./yamlschema";

export interface Verdict {
  yielded: boolean;
  reason: string;
  priority: boolean;
  // a CPU yield for RAM under the floor, which unloads (0022)
  ram_short?: boolean;
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

// The CPU under the one config (0022).
export interface CPUResources {
  foreign_percent: number | null;
  our_percent: number | null;
  window_seconds: number;
  cores: number;
  ram_available_mb: number;
  ram_total_mb: number;
  culprits: string[];
  priority: string[];
  sampled_at: string;
}

export interface CPUPolicy {
  busy_percent: number;
  window_seconds: number;
  resume_quiet_seconds: number;
  min_available_ram_mb: number;
  priority_processes: string[] | null;
}

export interface CPUState {
  verdict: Verdict;
  policy: CPUPolicy;
  resources: CPUResources | null;
  probe_error: string | null;
  pending_unload: boolean;
  last_unload: string[] | null;
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
  devices: string[];
  // an upgraded WebSocket (0018): in flight while data moves
  session?: { moving: boolean; idle_seconds: number };
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
    paused_devices: string[];
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
  // the one config's (0022), null outside it
  cpu: CPUState | null;
  placement: Record<string, string[]> | null;
  // the jobs this host holds (0023), finished ones for ten minutes
  jobs?: { counts: Record<string, number>; paused_reasons: string[] };
}

export interface Consumer {
  name: string;
  url: string;
  announce_path: string;
  timeout_seconds: number;
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
  trusted_hosts: string[] | null;
  policy: Policy;
  // used under the one config only (0022)
  cpu: CPUPolicy;
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

export interface UIState {
  runtimes: Record<string, string>;
  warden: WardenConfig;
  // Per runtime macro, the K-V pairs with a compiled FlashAttention kernel.
  kv_kernels: Record<string, string[]> | null;
  // Publish takes the config key's passphrase from -config-passphrase-command.
  config_passphrase: boolean;
  daemon_port: string;
  swap_port: string;
  paths: { warden_file: string; base_config: string; gguf_dirs: string[]; signed_config: string };
}

export interface GGUF {
  path: string;
  bytes: number;
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
  // a WebSocket session, status 101 (0018, 8)
  session?: SessionStat;
}

export interface SessionStat {
  upgrade_ms: number;
  first_output_ms: number | null;
  active_ms: number;
  idle_ms: number;
  close_code?: number;
  closed_by?: "client" | "backend" | "infermux";
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
  // absent from a host older than 2026-10-07
  sessions?: number;
  first_output_ms?: Spread | null;
}

export interface HostStats {
  host: string;
  requests: RequestStat[] | null;
  models: ModelStat[] | null;
  error?: string;
}

// The one config (0022): a version, as the daemon keeps it and signs it.
export interface Summary {
  hash: string;
  revision: number;
  time: string;
  host: string;
}

// A model's place, in order: balance first, then failover.
export interface Route {
  place: string;
  host: string;
  device: string;
  max_inflight?: number;
  only_if_idle?: boolean;
  batch_only?: boolean;
  failover?: boolean;
}

export interface Problem {
  file: string;
  line: number;
  message: string;
}

// infermux-ui's /api/config/check: a draft as the daemons would run it.
export interface Check {
  error: Problem | null;
  warnings: string[];
  routes: Record<string, Route[]>;
  devices: Record<string, Record<string, string>>;
}

export interface FileStatus {
  path: string;
  state: "present" | "queued" | "fetching" | "failed" | "unfetchable";
  from?: string;
  done?: number;
  total?: number;
  error?: string;
}

// The daemon's /warden/config/state.
export interface ConfigState {
  host: string;
  current: Summary | null;
  // The later of two versions edited apart runs; the other is kept.
  conflict: { at: string; kept: Summary; lost: Summary } | null;
  rejected: { hash: string; from: string; at: string; error: string } | null;
  warnings: string[];
  routes: Record<string, Route[]> | null;
  local: Record<string, { model: string; devices: string[]; files: string[] | null }> | null;
  files: FileStatus[];
}

// A signed version on the wire: the files as text.
export interface Wire {
  manifest: string;
  signature: string;
  files: Record<string, string>;
}

export interface Published {
  result: "same" | "old" | "applied" | "replaced" | "lost";
  state: ConfigState;
  published: Summary;
}
