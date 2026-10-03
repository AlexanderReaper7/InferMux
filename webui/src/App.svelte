<script lang="ts">
  import { live } from "./lib/live.svelte";
  import Status from "./routes/Status.svelte";
  import Models from "./routes/Models.svelte";
  import Settings from "./routes/Settings.svelte";
  import Changes from "./routes/Changes.svelte";
  import Keys from "./routes/Keys.svelte";
  import { api } from "./lib/api";

  const tabs = [
    { id: "status", label: "Status" },
    { id: "models", label: "Models" },
    { id: "settings", label: "Warden" },
    { id: "keys", label: "Keys" },
    { id: "changes", label: "Changes" },
    { id: "llama-swap", label: "llama-swap" },
  ] as const;
  type Tab = (typeof tabs)[number]["id"];

  function fromHash(): Tab {
    const id = location.hash.slice(1);
    return (tabs.find((t) => t.id === id)?.id ?? "status") as Tab;
  }
  let tab = $state<Tab>(fromHash());

  // llama-swap's own UI, from the daemon's origin. Its fetches are absolute
  // paths on that origin, so it is framed rather than proxied (0005).
  let daemonPort = $state("5001");
  api.state().then((s) => (daemonPort = s.daemon_port || daemonPort), () => {});
  const swapUI = $derived(`${location.protocol}//${location.hostname}:${daemonPort}/ui/`);
  // Mounted on first visit and kept, so a tab switch does not reload it and
  // lose a playground conversation.
  let swapOpened = $state(false);
  $effect(() => {
    if (tab === "llama-swap") swapOpened = true;
  });
  $effect(() => {
    const onHash = () => (tab = fromHash());
    addEventListener("hashchange", onHash);
    return () => removeEventListener("hashchange", onHash);
  });
</script>

<div class="mx-auto p-6 {tab === 'llama-swap' ? '' : 'max-w-7xl'}">
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
    </div>
  </header>

  {#if tab === "status"}
    <Status />
  {:else if tab === "models"}
    <Models />
  {:else if tab === "settings"}
    <Settings />
  {:else if tab === "keys"}
    <Keys />
  {:else if tab === "changes"}
    <Changes />
  {/if}
  {#if swapOpened}
    <iframe
      title="llama-swap"
      src={swapUI}
      class="h-[calc(100vh-7rem)] w-full rounded border border-neutral-800 {tab === 'llama-swap' ? '' : 'hidden'}"
    ></iframe>
  {/if}
</div>
