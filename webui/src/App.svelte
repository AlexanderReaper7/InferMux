<script lang="ts">
  import { live } from "./lib/live.svelte";
  import Status from "./routes/Status.svelte";
  import Models from "./routes/Models.svelte";
  import Settings from "./routes/Settings.svelte";
  import Changes from "./routes/Changes.svelte";

  const tabs = [
    { id: "status", label: "Status" },
    { id: "models", label: "Models" },
    { id: "settings", label: "Warden" },
    { id: "changes", label: "Changes" },
  ] as const;
  type Tab = (typeof tabs)[number]["id"];

  function fromHash(): Tab {
    const id = location.hash.slice(1);
    return (tabs.find((t) => t.id === id)?.id ?? "status") as Tab;
  }
  let tab = $state<Tab>(fromHash());
  $effect(() => {
    const onHash = () => (tab = fromHash());
    addEventListener("hashchange", onHash);
    return () => removeEventListener("hashchange", onHash);
  });
</script>

<div class="mx-auto max-w-7xl p-6">
  <header class="mb-6 flex items-center gap-6 border-b border-neutral-800 pb-4">
    <h1 class="text-xl font-semibold tracking-tight text-neutral-100">InferMux</h1>
    <nav class="flex gap-1">
      {#each tabs as t}
        <a
          href={"#" + t.id}
          class="rounded px-3 py-1 text-sm {tab === t.id ? 'bg-neutral-800 text-neutral-100' : 'text-neutral-400 hover:text-neutral-200'}"
          >{t.label}</a
        >
      {/each}
    </nav>
    <div class="ml-auto flex items-center gap-4 text-sm">
      {#if live.error}
        <span class="text-red-400">daemon unreachable</span>
      {:else if live.state}
        <span
          class="rounded-full px-3 py-0.5 font-medium {live.state.verdict.yielded
            ? 'bg-amber-950 text-amber-300'
            : 'bg-emerald-950 text-emerald-300'}"
        >
          {live.state.action}{live.state.manual ? " (by hand)" : ""}
        </span>
      {/if}
      <a class="text-neutral-400 hover:text-neutral-200" href={`${location.protocol}//${location.hostname}:5001/ui/`} target="_blank" rel="noreferrer"
        >llama-swap ↗</a
      >
    </div>
  </header>

  {#if tab === "status"}
    <Status />
  {:else if tab === "models"}
    <Models />
  {:else if tab === "settings"}
    <Settings />
  {:else}
    <Changes />
  {/if}
</div>
