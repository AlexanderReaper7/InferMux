import type { Model } from "./types";

// The K-V cache pair a model asks llama.cpp for. An unset type is f16,
// llama.cpp's own default.
export function kvPair(m: Model): string {
  if (m.raw) {
    const k = m.cmd.match(/(?:--cache-type-k|-ctk)\s+(\S+)/);
    const v = m.cmd.match(/(?:--cache-type-v|-ctv)\s+(\S+)/);
    return `${k?.[1] ?? "f16"}-${v?.[1] ?? "f16"}`;
  }
  const value = (...names: string[]) => m.flags.find((f) => names.includes(f.name))?.value ?? "f16";
  return `${value("--cache-type-k", "-ctk")}-${value("--cache-type-v", "-ctv")}`;
}

// The runtime's compiled pairs, when the module said what they are.
function runtimeOf(m: Model): string {
  if (!m.raw) return m.runtime;
  return m.cmd.match(/^\s*\$\{([^}]+)\}/)?.[1] ?? "";
}

// A sentence when the model's pair has no kernel in its runtime, else null.
// Such a cache is converted to f16 on every decode step: 61 instead of 80 t/s
// on the 9B at 32k context (nixcfg's decisions, 2026-09-25).
export function kvWarning(m: Model, kernels: Record<string, string[]> | null): string | null {
  const compiled = kernels?.[runtimeOf(m)];
  if (!compiled) return null;
  const pair = kvPair(m);
  if (compiled.includes(pair)) return null;
  return `${runtimeOf(m)} has no FlashAttention kernel for ${pair}: the cache is converted to f16 on every decode step, about a quarter slower at 32k context, until a rebuild compiles it and a switch puts it in use.`;
}
