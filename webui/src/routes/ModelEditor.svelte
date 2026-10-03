<script lang="ts">
  import { untrack } from "svelte";
  import { api, basename, gib } from "../lib/api";
  import type { GGUF, Model } from "../lib/types";

  let {
    model,
    original,
    runtimes,
    ggufs,
    onclose,
  }: {
    model: Model;
    original: string; // "" for a new model
    runtimes: Record<string, string>;
    ggufs: GGUF[];
    onclose: (saved: boolean) => void;
  } = $props();

  // An edited copy; the list is only reloaded after a save.
  let m = $state<Model>(untrack(() => structuredClone($state.snapshot(model)) as Model));
  let aliases = $state(m.aliases.join(", "));
  let ttlText = $state(m.ttl === null ? "" : String(m.ttl));
  let error = $state<string | null>(null);
  let saving = $state(false);

  function addFlag() {
    m.flags.push({ name: "--", value: "" });
  }

  async function save() {
    saving = true;
    error = null;
    const body: Model = {
      ...$state.snapshot(m),
      aliases: aliases
        .split(",")
        .map((a) => a.trim())
        .filter(Boolean),
      ttl: ttlText.trim() === "" ? null : Number(ttlText),
    };
    try {
      if (original) await api.saveModel(original, body);
      else await api.createModel(body);
      onclose(true);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      saving = false;
    }
  }

  async function remove() {
    if (!confirm(`Delete ${original}? Its file changes on disk; commit or revert it under Changes.`)) return;
    try {
      await api.deleteModel(original);
      onclose(true);
    } catch (e) {
      error = (e as Error).message;
    }
  }
</script>

<div class="card">
  <div class="mb-4 flex items-baseline gap-3">
    <h2 class="text-lg font-medium">{original ? `Edit ${original}` : "New model"}</h2>
    {#if m.file}<span class="text-sm text-neutral-500">{m.file}</span>{/if}
    <button class="btn ml-auto" onclick={() => onclose(false)}>Close</button>
  </div>

  {#if m.comment}
    <pre class="mb-4 rounded bg-neutral-900 p-3 text-xs whitespace-pre-wrap text-neutral-400">{m.comment}</pre>
  {/if}

  <div class="grid gap-4 md:grid-cols-2">
    <label class="flex flex-col gap-1">
      <span class="label">Name, the model ID clients ask for</span>
      <input bind:value={m.name} />
    </label>
    <label class="flex flex-col gap-1">
      <span class="label">Aliases, comma-separated</span>
      <input bind:value={aliases} />
    </label>
    <label class="flex flex-col gap-1">
      <span class="label">Description</span>
      <input bind:value={m.description} />
    </label>
    <div class="flex gap-4">
      <label class="flex flex-1 flex-col gap-1">
        <span class="label">TTL seconds, empty for the global</span>
        <input bind:value={ttlText} inputmode="numeric" />
      </label>
      <label class="flex items-end gap-2 pb-1 text-sm">
        <input type="checkbox" bind:checked={m.unlisted} /> unlisted
      </label>
    </div>
  </div>

  <div class="mt-6 mb-2 flex items-center gap-4">
    <h3 class="font-medium">Command</h3>
    <label class="flex items-center gap-2 text-sm text-neutral-400">
      <input type="checkbox" bind:checked={m.raw} /> edit as text
    </label>
  </div>

  {#if m.raw}
    <textarea class="h-48 w-full font-mono text-sm" bind:value={m.cmd}></textarea>
    <div class="mt-1 text-xs text-neutral-500">
      Runtimes: {Object.keys(runtimes).map((r) => "${" + r + "}").join(", ")}. A command in the form
      runtime, --port ${"{PORT}"}, --model, flags opens as a table next time.
    </div>
  {:else}
    <div class="grid gap-4 md:grid-cols-2">
      <label class="flex flex-col gap-1">
        <span class="label">Runtime</span>
        <select bind:value={m.runtime}>
          {#each Object.entries(runtimes) as [name, value]}
            <option value={name}>{name} ({basename(value.split(" ")[0])})</option>
          {/each}
        </select>
      </label>
      <label class="flex flex-col gap-1">
        <span class="label">GGUF</span>
        <select bind:value={m.gguf}>
          {#if m.gguf && !ggufs.some((g) => g.path === m.gguf)}
            <option value={m.gguf}>{m.gguf} (not found)</option>
          {/if}
          {#each ggufs as g}
            <option value={g.path}>{g.path} ({gib(g.bytes)}){g.used_by.length ? ` used by ${g.used_by.join(", ")}` : ""}</option>
          {/each}
        </select>
      </label>
    </div>

    <table class="mt-4 w-full text-sm">
      <thead class="text-left text-neutral-500">
        <tr><th class="w-1/3 py-1 font-normal">Flag</th><th class="font-normal">Value</th><th class="w-24 font-normal">Switch</th><th class="w-10"></th></tr>
      </thead>
      <tbody>
        {#each m.flags as flag, i}
          <tr>
            <td class="py-1 pr-2"><input class="w-full font-mono" bind:value={flag.name} /></td>
            <td class="pr-2">
              {#if flag.value !== null}
                <input class="w-full font-mono" bind:value={flag.value} />
              {:else}
                <span class="text-neutral-600">no value</span>
              {/if}
            </td>
            <td>
              <input
                type="checkbox"
                checked={flag.value === null}
                onchange={(e) => (flag.value = (e.target as HTMLInputElement).checked ? null : "")}
              />
            </td>
            <td><button class="text-neutral-500 hover:text-red-400" title="Remove" onclick={() => m.flags.splice(i, 1)}>✕</button></td>
          </tr>
        {/each}
      </tbody>
    </table>
    <button class="btn mt-2" onclick={addFlag}>Add flag</button>
    <div class="mt-1 text-xs text-neutral-500">
      --port and --model come from the runtime and GGUF fields. The flags go to the runtime as written, for example
      --ctx-size 262144, -fa on, --cache-type-k q4_0.
    </div>
  {/if}

  {#if error}
    <div class="mt-4 rounded border border-red-900 px-3 py-2 text-sm whitespace-pre-wrap text-red-300">{error}</div>
  {/if}

  <div class="mt-6 flex gap-2">
    <button class="btn-primary" disabled={saving} onclick={save}>{saving ? "Saving…" : "Save"}</button>
    {#if original}
      <button class="btn-danger ml-auto" onclick={remove}>Delete</button>
    {/if}
  </div>
  <div class="mt-2 text-xs text-neutral-500">
    Saving validates with llama-swap's own loader and writes the file. InferMux reloads once no request of yours is in
    flight; a reload stops the running model.
  </div>
</div>
