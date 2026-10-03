<script lang="ts">
  import { api } from "../lib/api";

  // This host's llama-swap routing, edited as text (0006, 9).
  let file = $state("");
  let saved = $state("");
  let draft = $state("");
  let error = $state("");

  async function load() {
    try {
      const r = await api.routing();
      file = r.file;
      saved = draft = r.text;
      error = "";
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function save() {
    try {
      await api.saveRouting(draft);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    load();
  });
</script>

<div class="card mt-4">
  <div class="mb-2 flex items-baseline gap-4">
    <h2 class="font-medium">Routing</h2>
    <span class="ml-auto text-sm text-neutral-500">{file}</span>
  </div>
  <div class="mb-2 text-sm text-neutral-500">
    Which models may run at the same time: llama-swap's <code>routing.router</code>, as groups or a matrix. Empty is
    one group, every model swapping with every other. Saving reloads the daemon when it is quiet, which stops the local
    models.
  </div>
  <textarea class="h-48 w-full font-mono text-sm" bind:value={draft} placeholder={"routing:\n  router:\n    use: group\n    settings:\n      groups:\n        ..."}></textarea>
  <div class="mt-2 flex items-center gap-4">
    {#if error}<span class="text-sm text-red-300">{error}</span>{/if}
    <button class="btn-primary ml-auto" disabled={draft === saved} onclick={save}>Save</button>
  </div>
</div>
