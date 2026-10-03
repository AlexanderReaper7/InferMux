<script lang="ts">
  import { api } from "../lib/api";
  import type { Policy, WardenConfig } from "../lib/types";

  let cfg = $state<WardenConfig | null>(null);
  let path = $state("");
  let error = $state<string | null>(null);
  let saved = $state(false);
  let lists = $state({ our_units: "", desktop_processes: "", batch_api_keys: "", trusted_hosts: "" });

  async function load() {
    try {
      const ui = await api.state();
      cfg = ui.warden;
      cfg.consumers ??= [];
      path = ui.paths.warden_file;
      lists = {
        our_units: (cfg.our_units ?? []).join("\n"),
        desktop_processes: (cfg.desktop_processes ?? []).join("\n"),
        batch_api_keys: (cfg.batch_api_keys ?? []).join("\n"),
        trusted_hosts: (cfg.trusted_hosts ?? []).join("\n"),
      };
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  const lines = (s: string) =>
    s
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);

  async function save() {
    if (!cfg) return;
    error = null;
    saved = false;
    const body: WardenConfig = {
      ...$state.snapshot(cfg),
      our_units: lines(lists.our_units),
      desktop_processes: lines(lists.desktop_processes),
      batch_api_keys: lines(lists.batch_api_keys),
      trusted_hosts: lines(lists.trusted_hosts),
    };
    try {
      await api.saveWarden(body);
      saved = true;
      load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  const numbers: { key: keyof Policy; label: string; help: string }[] = [
    { key: "gpu_busy_percent", label: "Busy at, % foreign GPU", help: "Foreign utilization that counts as contention." },
    { key: "min_free_vram_mb", label: "Min free VRAM, MB", help: "Read only while no model is loaded." },
    { key: "resume_quiet_seconds", label: "Resume after quiet, s", help: "Also the Retry-After a refused batch request gets." },
    { key: "interactive_recent_seconds", label: "Interactive recent, s", help: "No unload this long after your last request." },
    { key: "poll_seconds", label: "Poll every, s", help: "The yield latency." },
    { key: "comfyui_idle_seconds", label: "ComfyUI idle, s", help: "Empty queue this long before /free." },
  ];
</script>

{#if error}
  <div class="card mb-4 whitespace-pre-wrap text-red-300">{error}</div>
{/if}

{#if cfg}
  <div class="mb-4 text-sm text-neutral-500">{path}. The daemon reloads these at once.</div>
  <div class="grid gap-4 lg:grid-cols-2">
    <section class="card">
      <h2 class="mb-3 font-medium">Policy</h2>
      <label class="mb-4 flex items-center gap-2">
        <input type="checkbox" bind:checked={cfg.policy.enabled} /> Enabled
        <span class="text-xs text-neutral-500">Disabled lifts a pause and announces nothing.</span>
      </label>
      <div class="grid gap-3 sm:grid-cols-2">
        {#each numbers as n}
          <label class="flex flex-col gap-1">
            <span class="label">{n.label}</span>
            <input type="number" bind:value={cfg.policy[n.key] as number} />
            <span class="text-xs text-neutral-500">{n.help}</span>
          </label>
        {/each}
      </div>
    </section>

    <section class="card">
      <h2 class="mb-3 font-medium">Whose work is whose</h2>
      <div class="grid gap-3 sm:grid-cols-3">
        <label class="flex flex-col gap-1">
          <span class="label">Our units</span>
          <textarea class="h-28 font-mono text-sm" bind:value={lists.our_units}></textarea>
          <span class="text-xs text-neutral-500">systemd units whose GPU work is ours. InferMux's own always is.</span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="label">Desktop processes</span>
          <textarea class="h-28 font-mono text-sm" bind:value={lists.desktop_processes}></textarea>
          <span class="text-xs text-neutral-500">Never contention.</span>
        </label>
        <label class="flex flex-col gap-1">
          <span class="label">Batch API keys</span>
          <textarea class="h-28 font-mono text-sm" bind:value={lists.batch_api_keys}></textarea>
          <span class="text-xs text-neutral-500">Requests with one are batch; all others are yours.</span>
        </label>
      </div>
      <label class="mt-4 flex flex-col gap-1">
        <span class="label">Trusted hosts</span>
        <textarea class="h-16 font-mono text-sm" bind:value={lists.trusted_hosts}></textarea>
        <span class="text-xs text-neutral-500"
          >Names besides loopback that the UI and the daemon answer browsers on, such as the tailnet name tailscale serve
          uses.</span
        >
      </label>
      <label class="mt-4 flex flex-col gap-1">
        <span class="label">ComfyUI URL, empty for none</span>
        <input bind:value={cfg.comfyui_url} />
      </label>
    </section>

    <section class="card lg:col-span-2">
      <h2 class="mb-3 font-medium">Consumers</h2>
      <table class="w-full text-sm">
        <thead class="text-left text-neutral-500">
          <tr><th class="py-1 font-normal">Name</th><th class="font-normal">URL</th><th class="font-normal">Announce path</th><th class="font-normal">Timeout s</th><th></th></tr>
        </thead>
        <tbody>
          {#each cfg.consumers ?? [] as c, i}
            <tr>
              <td class="py-1 pr-2"><input class="w-full" bind:value={c.name} /></td>
              <td class="pr-2"><input class="w-full" bind:value={c.url} /></td>
              <td class="pr-2"><input class="w-full" bind:value={c.announce_path} /></td>
              <td class="pr-2"><input class="w-20" type="number" bind:value={c.timeout_seconds} /></td>
              <td><button class="text-neutral-500 hover:text-red-400" onclick={() => cfg!.consumers!.splice(i, 1)}>✕</button></td>
            </tr>
          {/each}
        </tbody>
      </table>
      <button
        class="btn mt-2"
        onclick={() => cfg!.consumers!.push({ name: "", url: "", announce_path: "/api/pipeline/announce", timeout_seconds: 10 })}
        >Add consumer</button
      >
    </section>
  </div>

  <div class="mt-4 flex items-center gap-4">
    <button class="btn-primary" onclick={save}>Save</button>
    {#if saved}<span class="text-sm text-emerald-300">Saved.</span>{/if}
  </div>
{/if}
