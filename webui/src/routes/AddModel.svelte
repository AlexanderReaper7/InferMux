<script lang="ts">
  import { api, basename, gib } from "../lib/api";
  import { modelFile, modelNames, newModel } from "../lib/draft";
  import type { GGUF } from "../lib/types";

  // A model file for a GGUF on this host or on Hugging Face, or an empty one,
  // with the command of a model on the same runtime. The SHA-256 is this
  // host's hash of the file, or Hugging Face's LFS one (0022, 8).
  let {
    from,
    files,
    ggufs,
    runtimes,
    places,
    ggufDir,
    namePattern,
    onadd,
    onclose,
  }: {
    from: "gguf" | "hf" | "empty";
    files: Record<string, string>;
    ggufs: GGUF[];
    runtimes: string[];
    places: string[];
    ggufDir: string;
    namePattern: RegExp;
    onadd: (name: string, text: string, place: string) => void;
    onclose: () => void;
  } = $props();

  // A GGUF the draft names is in use; mmproj files go with a model.
  const unused = $derived(ggufs.filter((g) => !g.path.includes("mmproj") && !Object.values(files).some((t) => t.includes(g.path))));
  let path = $state("");
  let source = $state("");
  let name = $state("");
  let runtime = $state("");
  let place = $state("");
  let busy = $state(false);
  let error = $state<string | null>(null);

  $effect.pre(() => {
    if (!runtime && runtimes.length) runtime = runtimes[0];
  });
  function named(p: string) {
    if (!name) name = basename(p).replace(/\.gguf$/, "");
  }
  const taken = $derived(modelNames(files).includes(name));
  const ready = $derived(namePattern.test(name) && !taken && !!runtime && (from === "empty" || (from === "gguf" ? !!path : /^[^/]+\/[^/]+\/.+\.gguf$/.test(source))));

  async function add() {
    busy = true;
    error = null;
    try {
      let text: string;
      if (from === "empty") {
        text = `cmd: |\n  \${${runtime}}\n    --port \${PORT}\n`;
      } else if (from === "gguf") {
        const { sha256 } = await api.hashGGUF(path);
        text = newModel(files, runtime, path, sha256, null);
      } else {
        const { sha256 } = await api.hfFile(source);
        text = newModel(files, runtime, `${ggufDir}/${basename(source)}`, sha256, source);
      }
      onadd(name, text, place);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

<div class="card mb-4 border-sky-900">
  <div class="mb-3 flex items-baseline gap-4">
    <h2 class="font-medium">
      {from === "gguf" ? "A model for a GGUF on this host" : from === "hf" ? "A model for a GGUF on Hugging Face" : "An empty model"}
    </h2>
    <button class="btn ml-auto" onclick={onclose}>Cancel</button>
  </div>
  <div class="grid grid-cols-[10rem_1fr] items-center gap-x-4 gap-y-2 text-sm">
    {#if from === "gguf"}
      <span class="label">GGUF</span>
      <select bind:value={path} onchange={() => named(path)}>
        <option value="" disabled>{unused.length ? "one the draft does not name" : "every GGUF here is named by a model"}</option>
        {#each unused as g}<option value={g.path}>{g.path} ({gib(g.bytes)})</option>{/each}
      </select>
    {:else if from === "hf"}
      <span class="label">Hugging Face</span>
      <input class="font-mono" placeholder="org/repo/file.gguf" bind:value={source} onchange={() => named(source)} />
      <span></span>
      <span class="text-xs text-neutral-500">
        Each host fetches it to {ggufDir}/{basename(source) || "file.gguf"} when the version runs, from another host that has it or from
        Hugging Face. Only an LFS file has a SHA-256 there.
      </span>
    {/if}
    <span class="label">Name</span>
    <input class="font-mono" bind:value={name} />
    {#if taken}<span></span><span class="text-xs text-amber-300">{modelFile(name)} exists.</span>{/if}
    <span class="label">Runtime</span>
    <select bind:value={runtime}>
      {#each runtimes as r}<option value={r}>{r}</option>{/each}
    </select>
    <span class="label">Balance</span>
    <select bind:value={place}>
      <option value="">not placed yet</option>
      {#each places as p}<option value={p}>{p}</option>{/each}
    </select>
  </div>
  <div class="mt-3 flex items-center gap-4">
    {#if error}<span class="text-sm text-red-300">{error}</span>{/if}
    <button class="btn-primary ml-auto" disabled={!ready || busy} onclick={add}>{busy && from !== "empty" ? "Hashing" : "Add"}</button>
  </div>
</div>
