<script lang="ts">
  import { api, ago } from "../lib/api";
  import { live, refresh } from "../lib/live.svelte";

  let busy = $state(false);
  let message = $state<string | null>(null);
  let failed = $state(false);

  async function act(label: string, run: () => Promise<unknown>) {
    busy = true;
    message = null;
    try {
      const out = await run();
      failed = false;
      message = `${label}: ${JSON.stringify(out)}`;
    } catch (e) {
      failed = true;
      message = `${label}: ${(e as Error).message}`;
    } finally {
      busy = false;
      refresh();
    }
  }

  function whose(p: { ours: boolean; desktop: boolean }) {
    if (p.ours) return { label: "ours", cls: "text-sky-400" };
    if (p.desktop) return { label: "desktop", cls: "text-neutral-500" };
    return { label: "foreign", cls: "text-amber-400" };
  }

  const s = $derived(live.state);
  const res = $derived(s?.resources ?? null);
</script>

{#if live.error && !s}
  <div class="card text-red-300">The daemon does not answer: {live.error}</div>
{:else if !s}
  <div class="text-neutral-500">Loading…</div>
{:else}
  <section
    class="mb-6 rounded-lg border p-5 {s.verdict.yielded
      ? 'border-amber-800 bg-amber-950/40'
      : 'border-emerald-900 bg-emerald-950/30'}"
  >
    <div class="flex flex-wrap items-start gap-6">
      <div>
        <div class="text-3xl font-semibold {s.verdict.yielded ? 'text-amber-300' : 'text-emerald-300'}">
          {s.action.toUpperCase()}
        </div>
        <div class="mt-1 text-neutral-300">{s.verdict.reason}</div>
        <div class="mt-1 text-sm text-neutral-500">
          since {ago(s.verdict.since)}{s.verdict.contended_at ? `, contention last seen ${ago(s.verdict.contended_at)}` : ""}
        </div>
        {#if s.manual}
          <div class="mt-2 text-sm text-neutral-300">
            Set by hand {ago(s.manual.at)}. Holds until the warden's own decision moves from
            <b>{s.manual.own_action}</b>; it now says <b>{s.own.yielded ? "pause" : "resume"}</b>:
            {s.own.reason}.
          </div>
        {/if}
        {#if !s.enabled}
          <div class="mt-2 text-sm text-neutral-400">The warden is disabled. Enable it under Warden.</div>
        {/if}
      </div>
      <div class="ml-auto flex flex-col items-end gap-2">
        <div class="flex gap-2">
          <button class="btn" disabled={busy || !s.enabled} onclick={() => act("Pause", () => api.manual("pause"))}
            >Pause</button
          >
          <button class="btn" disabled={busy || !s.enabled} onclick={() => act("Resume", () => api.manual("resume"))}
            >Resume</button
          >
          <button class="btn" disabled={busy || !s.manual} onclick={() => act("Automatic", () => api.manual("auto"))}
            >Automatic</button
          >
        </div>
        <div class="text-xs text-neutral-500">A manual verdict ends when the warden's own decision changes.</div>
      </div>
    </div>
    {#if s.waiting_for_quiet.length}
      <div class="mt-3 text-sm text-sky-300">
        Waiting for your requests to finish: {s.waiting_for_quiet.join(", ")}
      </div>
    {/if}
    {#if s.probe_error}
      <div class="mt-3 text-sm text-red-300">Probe failed, verdict unchanged: {s.probe_error}</div>
    {/if}
  </section>

  {#if message}
    <div class="mb-4 rounded border px-3 py-2 text-sm {failed ? 'border-red-900 text-red-300' : 'border-neutral-800 text-neutral-400'}">
      {message}
    </div>
  {/if}

  <div class="grid gap-4 lg:grid-cols-3">
    <section class="card lg:col-span-2">
      <div class="mb-3 flex items-baseline gap-4">
        <h2 class="font-medium">GPU</h2>
        {#if res}
          <span class="text-sm text-neutral-500">sampled {ago(res.sampled_at)}</span>
        {/if}
      </div>
      {#if res}
        <div class="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
          <div>
            <div class="label">Foreign</div>
            <div class="text-2xl {(res.foreign_gpu_percent ?? 0) >= s.policy.gpu_busy_percent ? 'text-amber-300' : ''}">
              {res.foreign_gpu_percent ?? "–"}%
            </div>
            <div class="text-xs text-neutral-500">busy at {s.policy.gpu_busy_percent}%</div>
          </div>
          <div>
            <div class="label">Ours</div>
            <div class="text-2xl text-sky-300">{res.our_gpu_percent}%</div>
            <div class="text-xs text-neutral-500">{res.our_vram_mb} MB</div>
          </div>
          <div>
            <div class="label">Desktop</div>
            <div class="text-2xl text-neutral-400">{res.desktop_gpu_percent}%</div>
          </div>
          <div>
            <div class="label">VRAM</div>
            <div class="text-2xl">{res.vram_used_mb} <span class="text-sm text-neutral-500">/ {res.vram_total_mb} MB</span></div>
            <div class="text-xs text-neutral-500">
              {res.vram_free_mb === null ? "free not read while a model is loaded" : `${res.vram_free_mb} MB free`}
            </div>
          </div>
        </div>
        <table class="w-full text-sm">
          <thead class="text-left text-neutral-500">
            <tr><th class="py-1 font-normal">Process</th><th class="font-normal">Unit</th><th class="font-normal">Whose</th><th class="text-right font-normal">GPU</th><th class="text-right font-normal">VRAM</th></tr>
          </thead>
          <tbody>
            {#each res.processes as p (p.pid)}
              {@const w = whose(p)}
              <tr class="border-t border-neutral-900">
                <td class="py-1">{p.name} <span class="text-neutral-600">{p.pid}</span></td>
                <td class="text-neutral-500">{p.unit}</td>
                <td class={w.cls}>{w.label}</td>
                <td class="text-right">{p.percent}%</td>
                <td class="text-right">{p.vram_mb} MB</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {:else}
        <div class="text-neutral-500">No measurement yet.</div>
      {/if}
    </section>

    <section class="card">
      <h2 class="mb-3 font-medium">Models</h2>
      {#if Object.keys(s.models).length}
        <ul class="mb-3 text-sm">
          {#each Object.entries(s.models) as [id, state]}
            <li class="flex justify-between border-t border-neutral-900 py-1"><span>{id}</span><span class="text-sky-300">{state}</span></li>
          {/each}
        </ul>
      {:else}
        <div class="mb-3 text-sm text-neutral-500">None loaded.</div>
      {/if}
      {#if s.pending_unload}
        <div class="mb-3 text-sm text-amber-300">An unload is owed and waits for your session to go quiet.</div>
      {/if}
      <div class="flex flex-wrap gap-2">
        <button class="btn" disabled={busy || !Object.keys(s.models).length} onclick={() => act("Unload", api.unloadAll)}>Unload all</button>
        <button class="btn" disabled={busy || !s.pending_unload} onclick={() => act("Forgive", api.forgive)}>Forgive owed unload</button>
      </div>
      {#if s.last_unload?.length}
        <div class="mt-3 text-xs text-neutral-500">Last unloaded: {s.last_unload.join(", ")}</div>
      {/if}
    </section>

    <section class="card">
      <h2 class="mb-3 font-medium">Requests</h2>
      {#if s.traffic.in_flight.length}
        <ul class="mb-3 text-sm">
          {#each s.traffic.in_flight as f}
            <li class="flex justify-between border-t border-neutral-900 py-1">
              <span class={f.class === "batch" ? "text-amber-300" : "text-sky-300"}>{f.class}</span>
              <span class="text-neutral-400">{f.path}</span>
              <span>{f.age_seconds} s</span>
            </li>
          {/each}
        </ul>
      {:else}
        <div class="mb-3 text-sm text-neutral-500">Nothing in flight.</div>
      {/if}
      <div class="mb-3 text-sm text-neutral-400">
        Last interactive {ago(s.traffic.last_interactive)}. Batch refused {s.traffic.refused_batch}, cancelled {s.traffic.cancelled_batch}.
      </div>
      <button class="btn" disabled={busy || !s.traffic.in_flight.some((f) => f.class === "batch")} onclick={() => act("Cancel batch", api.cancelBatch)}
        >Cancel batch requests</button
      >
    </section>

    <section class="card">
      <h2 class="mb-3 font-medium">Consumers</h2>
      {#each Object.entries(s.consumers) as [name, c]}
        <div class="border-t border-neutral-900 py-1 text-sm">
          <div class="flex justify-between">
            <span>{name}</span>
            <span class={c.action === "pause" ? "text-amber-300" : "text-emerald-300"}>{c.action ?? "not told yet"}</span>
          </div>
          <div class="text-xs text-neutral-500">{c.url}{c.age_seconds !== null ? `, told ${Math.round(c.age_seconds)} s ago` : ""}</div>
          {#if c.error}<div class="text-xs text-red-300">{c.error}</div>{/if}
        </div>
      {:else}
        <div class="text-sm text-neutral-500">None configured.</div>
      {/each}
    </section>

    <section class="card">
      <h2 class="mb-3 font-medium">ComfyUI</h2>
      {#if s.comfyui}
        <div class="text-sm text-neutral-400">{s.comfyui.url}</div>
        <div class="mb-3 text-sm">
          {res?.comfyui_jobs ? `${res.comfyui_jobs} job(s) queued` : "queue empty"}, last busy {ago(s.comfyui.busy_at)},
          {s.comfyui.freed ? "models freed" : "models may be held"}
        </div>
        <button class="btn" disabled={busy} onclick={() => act("Free ComfyUI", api.freeComfyUI)}>Free its models now</button>
      {:else}
        <div class="text-sm text-neutral-500">Not configured.</div>
      {/if}
    </section>
  </div>
{/if}
