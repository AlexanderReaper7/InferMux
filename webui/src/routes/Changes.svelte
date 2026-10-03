<script lang="ts">
  import { api } from "../lib/api";
  import type { GitState } from "../lib/types";

  let git = $state<GitState | null>(null);
  let message = $state("");
  let error = $state<string | null>(null);
  let done = $state<string | null>(null);

  async function load() {
    try {
      git = await api.git();
      error = null;
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  async function commit() {
    error = null;
    done = null;
    try {
      const { commit } = await api.commit(message);
      done = `Committed ${commit}. Nothing was pushed.`;
      message = "";
      load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  function lineClass(line: string) {
    if (line.startsWith("+++") || line.startsWith("---")) return "text-neutral-500";
    if (line.startsWith("+")) return "text-emerald-300";
    if (line.startsWith("-")) return "text-red-300";
    if (line.startsWith("@@")) return "text-sky-400";
    if (line.startsWith("diff ")) return "text-neutral-100 font-semibold mt-3";
    return "text-neutral-400";
  }
</script>

{#if error}
  <div class="card mb-4 whitespace-pre-wrap text-red-300">{error}</div>
{/if}
{#if done}
  <div class="card mb-4 text-emerald-300">{done}</div>
{/if}

{#if git}
  <div class="mb-4 flex items-center gap-4 text-sm text-neutral-500">
    <span>{git.repo} on {git.branch || "a detached HEAD"}</span>
    <button class="btn ml-auto" onclick={load}>Refresh</button>
  </div>

  {#if git.changes.length}
    <div class="card mb-4">
      <h2 class="mb-2 font-medium">Uncommitted</h2>
      <ul class="mb-4 font-mono text-sm">
        {#each git.changes as c}<li>{c}</li>{/each}
      </ul>
      <div class="flex gap-2">
        <input class="flex-1" placeholder="Commit message" bind:value={message} />
        <button class="btn-primary" disabled={!message.trim()} onclick={commit}>Commit</button>
      </div>
      <div class="mt-1 text-xs text-neutral-500">Commits InferMux's files only, with your git identity. It never pushes.</div>
    </div>
    <pre class="card overflow-x-auto font-mono text-xs leading-5">{#each git.diff.split("\n") as line}<div class={lineClass(line)}>{line || " "}</div>{/each}</pre>
  {:else}
    <div class="card text-neutral-500">Nothing uncommitted.</div>
  {/if}
{/if}
