<script lang="ts">
  import { api, basename, gib } from "../lib/api";
  import { live } from "../lib/live.svelte";
  import type { GGUF, Model, UIState } from "../lib/types";
  import ModelEditor from "./ModelEditor.svelte";

  let ui = $state<UIState | null>(null);
  let ggufs = $state<GGUF[]>([]);
  let error = $state<string | null>(null);
  let editing = $state<{ model: Model; original: string } | null>(null);

  async function load() {
    try {
      [ui, ggufs] = await Promise.all([api.state(), api.ggufs()]);
      error = null;
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  function flag(m: Model, name: string) {
    const f = m.flags.find((f) => f.name === name);
    return f ? (f.value ?? "on") : "";
  }

  function blank(path = ""): Model {
    const runtime = Object.keys(ui?.runtimes ?? {})[0] ?? "";
    const name = path ? basename(path).replace(/\.gguf$/, "") : "";
    return { name, file: "", runtime, gguf: path, flags: [], raw: false, cmd: "", ttl: null, aliases: [], description: "", unlisted: false, comment: "" };
  }

  // A new model starts from the flags of an existing one on the same runtime.
  function copyFlags(m: Model) {
    const like = ui?.models.find((o) => !o.raw && o.runtime === m.runtime);
    if (like) m.flags = structuredClone($state.snapshot(like.flags));
    return m;
  }

  const unused = $derived(ggufs.filter((g) => !g.used_by.length && !g.path.includes("mmproj")));
</script>

{#if error}
  <div class="card mb-4 text-red-300">{error}</div>
{/if}

{#if editing && ui}
  <ModelEditor
    model={editing.model}
    original={editing.original}
    runtimes={ui.runtimes}
    {ggufs}
    onclose={(saved) => {
      editing = null;
      if (saved) load();
    }}
  />
{:else if ui}
  <div class="mb-4 flex items-center gap-4">
    <div class="text-sm text-neutral-500">{ui.paths.models_dir}</div>
    <button class="btn-primary ml-auto" onclick={() => (editing = { model: copyFlags(blank()), original: "" })}>New model</button>
  </div>

  <div class="card">
    <table class="w-full text-sm">
      <thead class="text-left text-neutral-500">
        <tr>
          <th class="py-1 font-normal">Model</th><th class="font-normal">Runtime</th><th class="font-normal">GGUF</th>
          <th class="font-normal">Context</th><th class="font-normal">KV cache</th><th class="font-normal">State</th>
        </tr>
      </thead>
      <tbody>
        {#each ui.models as m (m.name)}
          {@const state = live.state?.models[m.name]}
          <tr class="cursor-pointer border-t border-neutral-900 hover:bg-neutral-900" onclick={() => (editing = { model: m, original: m.name })}>
            <td class="py-2">
              <div>{m.name}</div>
              {#if m.description}<div class="text-xs text-neutral-500">{m.description}</div>{/if}
            </td>
            <td class="text-neutral-400">{m.raw ? "text" : m.runtime}</td>
            <td class="text-neutral-400">{m.raw ? "" : basename(m.gguf)}</td>
            <td>{flag(m, "--ctx-size") || flag(m, "-c")}</td>
            <td>{flag(m, "--cache-type-k") || flag(m, "-ctk")}</td>
            <td class={state ? "text-sky-300" : "text-neutral-600"}>{state ?? "stopped"}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  {#if unused.length}
    <div class="card mt-4">
      <h2 class="mb-2 font-medium">GGUFs no model uses</h2>
      {#each unused as g}
        <div class="flex items-center justify-between border-t border-neutral-900 py-1 text-sm">
          <span>{g.path} <span class="text-neutral-500">{gib(g.bytes)}</span></span>
          <button class="btn" onclick={() => (editing = { model: copyFlags(blank(g.path)), original: "" })}>Add as a model</button>
        </div>
      {/each}
    </div>
  {/if}
{/if}
