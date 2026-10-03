<script lang="ts">
  import { onDestroy } from "svelte";
  import { ago, api } from "../lib/api";
  import type { BuildState } from "../lib/types";

  let st = $state<BuildState | null>(null);
  let error = $state<string | null>(null);
  let timer: ReturnType<typeof setTimeout> | undefined;

  async function poll() {
    try {
      st = await api.build();
      error = null;
    } catch (e) {
      error = (e as Error).message;
    }
    if (st?.running) timer = setTimeout(poll, 2000);
  }
  poll();
  onDestroy(() => clearTimeout(timer));

  async function start() {
    error = null;
    try {
      st = await api.startBuild();
      clearTimeout(timer);
      timer = setTimeout(poll, 2000);
    } catch (e) {
      error = (e as Error).message;
    }
  }
</script>

{#if st?.configured}
  <div class="mt-3 flex items-center gap-3">
    <button class="btn" disabled={st.running} onclick={start}>{st.running ? "Building…" : "Build now"}</button>
    <span class="text-xs text-neutral-500">
      {#if st.running}
        started {ago(st.started)}
      {:else if st.stale}
        The last build, {ago(st.started)}, predates a model change. Build again for the files as they are.
      {:else if st.ok === true}
        <span class="text-emerald-300">built {ago(st.ended)}.</span> Switch to put it in use: commit, push, then nixos-rebuild switch.
      {:else if st.ok === false}
        <span class="text-red-300">failed {ago(st.ended)}: {st.error}</span>
      {:else}
        Builds <span class="font-mono">{st.installable}</span> ahead of the switch, as you. Nothing is switched.
      {/if}
    </span>
  </div>
  {#if st.log?.length && (st.running || (st.ok === false && !st.stale))}
    <pre class="mt-2 max-h-64 overflow-auto rounded bg-neutral-950 p-2 font-mono text-xs text-neutral-400">{st.log.join("\n")}</pre>
  {/if}
{/if}
{#if error}
  <div class="mt-2 text-xs text-red-300">{error}</div>
{/if}
