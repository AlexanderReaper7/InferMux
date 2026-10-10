<script lang="ts">
  import { MergeView } from "@codemirror/merge";
  import { EditorView } from "@codemirror/view";
  import { basicSetup } from "codemirror";
  import { yaml } from "@codemirror/lang-yaml";
  import { oneDark } from "@codemirror/theme-one-dark";
  import { untrack, type Snippet } from "svelte";
  import { black, readOnly } from "../lib/editor";

  // The draft beside another version, file by file. The arrows copy a
  // change from the other version into the draft.
  let {
    files,
    other,
    label,
    onedit,
    children,
  }: {
    files: Record<string, string>;
    other: Record<string, string>;
    label: string;
    onedit: (name: string, text: string) => void;
    children: Snippet;
  } = $props();

  const names = $derived(
    [...new Set([...Object.keys(files), ...Object.keys(other)])].filter((n) => (files[n] ?? "") !== (other[n] ?? "")).sort(),
  );
  let shown = $state<string | null>(null);
  $effect.pre(() => {
    if (shown === null && names.length) shown = names[0];
  });

  let host = $state<HTMLDivElement>();
  $effect(() => {
    const name = shown;
    if (!name || !host) return;
    const view = new MergeView({
      a: {
        doc: untrack(() => files[name] ?? ""),
        extensions: [basicSetup, yaml(), oneDark, black, EditorView.updateListener.of((u) => u.docChanged && onedit(name, u.state.doc.toString()))],
      },
      b: { doc: untrack(() => other[name] ?? ""), extensions: readOnly },
      parent: host,
      revertControls: "b-to-a",
      gutter: true,
    });
    return () => view.destroy();
  });
</script>

<div class="card mb-4 border-sky-900">
  <div class="mb-3 flex flex-wrap items-center gap-2">
    {#each names as n}
      <button class="rounded px-2 py-0.5 font-mono text-sm {shown === n ? 'bg-neutral-800 text-neutral-100' : 'text-neutral-400 hover:text-neutral-200'}" onclick={() => (shown = n)}
        >{n}{files[n] === undefined ? " (not in the draft)" : other[n] === undefined ? " (only in the draft)" : ""}</button
      >
    {:else}
      <span class="text-sm text-neutral-500">The draft and {label} are the same.</span>
    {/each}
    <div class="ml-auto flex gap-2">{@render children()}</div>
  </div>
  {#if names.length}
    <div class="mb-1 grid grid-cols-2 text-xs text-neutral-500">
      <span>Your draft</span><span>{label}</span>
    </div>
    <div bind:this={host} class="max-h-[60vh] overflow-auto rounded border border-neutral-800"></div>
  {/if}
</div>
