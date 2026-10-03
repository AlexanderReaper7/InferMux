<script lang="ts">
  import { api, gib } from "../lib/api";
  import type { Download } from "../lib/types";

  // The Hugging Face files fetched since infermux-ui started; a save starts one.
  let downloads = $state<Download[]>([]);
  let error = $state<string | null>(null);

  async function poll() {
    try {
      downloads = await api.downloads();
      error = null;
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    poll();
    const timer = setInterval(poll, 2000);
    return () => clearInterval(timer);
  });

  function percent(d: Download) {
    return d.total > 0 ? Math.floor((100 * d.done) / d.total) : 0;
  }
</script>

{#if downloads.length || error}
  <div class="card mt-4">
    <h2 class="mb-2 font-medium">Hugging Face downloads</h2>
    {#if error}<div class="text-sm text-red-300">{error}</div>{/if}
    {#each downloads as d (d.source)}
      <div class="border-t border-neutral-900 py-2 text-sm">
        <div class="flex items-baseline gap-4">
          <span class="font-mono">{d.source}</span>
          <span class="ml-auto text-neutral-400">
            {#if d.running}
              {gib(d.done)} of {gib(d.total)}, {percent(d)}%
            {:else if d.error}
              <span class="text-red-300">{d.error}</span>
            {:else}
              {d.result}, {gib(d.total)}
            {/if}
          </span>
        </div>
        {#if d.running}
          <div class="mt-1 h-1 rounded bg-neutral-800"><div class="h-1 rounded bg-sky-500" style="width: {percent(d)}%"></div></div>
        {/if}
        <div class="text-xs text-neutral-500">{d.path}</div>
      </div>
    {/each}
  </div>
{/if}
