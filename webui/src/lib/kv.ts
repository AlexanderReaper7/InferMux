import type { Model } from "./types";

// The K-V cache pair a model asks llama.cpp for. An unset type is f16,
// llama.cpp's own default.
export function kvPair(m: Model): string {
  if (m.raw) return cmdPair(m.cmd);
  const value = (...names: string[]) => m.flags.find((f) => names.includes(f.name))?.value ?? "f16";
  return `${value("--cache-type-k", "-ctk")}-${value("--cache-type-v", "-ctv")}`;
}

function cmdPair(cmd: string): string {
  const k = cmd.match(/(?:--cache-type-k|-ctk)\s+(\S+)/);
  const v = cmd.match(/(?:--cache-type-v|-ctv)\s+(\S+)/);
  return `${k?.[1] ?? "f16"}-${v?.[1] ?? "f16"}`;
}

function cmdRuntime(cmd: string): string {
  return cmd.match(/^\s*\$\{([^}]+)\}/)?.[1] ?? "";
}

// The runtime's compiled pairs, when the module said what they are.
function runtimeOf(m: Model): string {
  return m.raw ? cmdRuntime(m.cmd) : m.runtime;
}

// A sentence when the model's pair has no kernel in its runtime, else null.
// Such a cache is converted to f16 on every decode step: 61 instead of 80 t/s
// on the 9B at 32k context (nixcfg's decisions, 2026-09-25).
export function kvWarning(m: Model, kernels: Record<string, string[]> | null): string | null {
  return missing(runtimeOf(m), kvPair(m), kernels);
}

function missing(runtime: string, pair: string, kernels: Record<string, string[]> | null): string | null {
  const compiled = kernels?.[runtime];
  if (!compiled || compiled.includes(pair)) return null;
  return `${runtime} has no FlashAttention kernel for ${pair}: the cache is converted to f16 on every decode step, about a quarter slower at 32k context, until a rebuild compiles it and a switch puts it in use.`;
}

// The same warning for a model file of the one config (0022, 9): cmd's
// value and its source text, which starts at offset in the file. The range is
// the cache type flag, or the runtime's line when neither is set. A
// placement's flags can change the pair per device; those are not checked.
export function kvDiagnostic(
  cmd: string,
  source: string,
  offset: number,
  kernels: Record<string, string[]> | null,
): { from: number; to: number; message: string } | null {
  const message = missing(cmdRuntime(cmd), cmdPair(cmd), kernels);
  if (!message) return null;
  const at = [/(?:--cache-type-k|-ctk)\s+\S+/, /(?:--cache-type-v|-ctv)\s+\S+/, /\$\{[^\n]*/].map((re) => re.exec(source)).find((m) => m);
  return at ? { from: offset + at.index, to: offset + at.index + at[0].length, message } : null;
}
