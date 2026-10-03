<script lang="ts">
  import { api } from "../lib/api";
  import type { Peer } from "../lib/types";

  let { peers, onsaved }: { peers: Peer[]; onsaved: () => void } = $props();

  // One textarea per peer, a model per line, as the file lists them.
  let drafts = $state<Record<string, string>>({});
  let errors = $state<Record<string, string>>({});
  $effect(() => {
    drafts = Object.fromEntries(peers.map((p) => [p.name, p.models.join("\n")]));
  });

  async function save(p: Peer) {
    const models = drafts[p.name].split("\n").map((m) => m.trim()).filter(Boolean);
    try {
      await api.savePeer(p.name, models);
      errors[p.name] = "";
      onsaved();
    } catch (e) {
      errors[p.name] = (e as Error).message;
    }
  }
</script>

{#each peers as p (p.name)}
  <div class="card mt-4">
    <div class="mb-2 flex items-baseline gap-4">
      <h2 class="font-medium">{p.name}</h2>
      <span class="text-sm text-neutral-500">{p.proxy}</span>
      <span class="ml-auto text-sm text-neutral-500">{p.file}</span>
    </div>
    <div class="mb-2 text-sm text-neutral-500">
      Clients name these {p.name}/&lt;model&gt;. Saving reloads the daemon when it is quiet, which stops the local models.
    </div>
    <textarea class="h-40 w-full font-mono text-sm" bind:value={drafts[p.name]}></textarea>
    <div class="mt-2 flex items-center gap-4">
      {#if errors[p.name]}<span class="text-sm text-red-300">{errors[p.name]}</span>{/if}
      <button
        class="btn-primary ml-auto"
        disabled={drafts[p.name] === p.models.join("\n")}
        onclick={() => save(p)}>Save</button
      >
    </div>
  </div>
{/each}
