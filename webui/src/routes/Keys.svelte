<script lang="ts">
  import { api } from "../lib/api";
  import type { Key, KeysState } from "../lib/types";

  // One row per key. allow is edited as lines; "limited" with no lines is
  // allow: [], a key that may use nothing.
  type Row = { name: string; class: Key["class"]; limited: boolean; allow: string; shown: string | null };

  let keys = $state<KeysState | null>(null);
  let rows = $state<Row[]>([]);
  let error = $state<string | null>(null);
  let made = $state<{ name: string; key: string } | null>(null);
  let draft = $state<Row>({ name: "", class: "interactive", limited: false, allow: "", shown: null });

  const lines = (s: string) =>
    s
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);
  const toKey = (r: Row): Key => ({ class: r.class, allow: r.limited ? lines(r.allow) : null });

  async function load() {
    try {
      keys = await api.keys();
      rows = Object.entries(keys.keys)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([name, k]) => ({
          name,
          class: k.class,
          limited: k.allow != null,
          allow: (k.allow ?? []).join("\n"),
          shown: null,
        }));
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  async function act(f: () => Promise<unknown>) {
    error = null;
    try {
      await f();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  const create = () =>
    act(async () => {
      const { key } = await api.createKey(draft.name, toKey(draft));
      made = { name: draft.name, key };
      draft = { name: "", class: "interactive", limited: false, allow: "", shown: null };
      await load();
    });
  const save = (r: Row) => act(() => api.saveKey(r.name, toKey(r)).then(load));
  const remove = (r: Row) =>
    act(async () => {
      if (!confirm(`Revoke ${r.name}? Its client is refused once the daemons reload keys.yaml.`)) return;
      await api.deleteKey(r.name);
      await load();
    });
  const reveal = (r: Row) =>
    act(async () => {
      r.shown = r.shown ? null : (await api.revealKey(r.name)).key;
    });
</script>

{#if error}
  <div class="card mb-4 whitespace-pre-wrap text-red-300">{error}</div>
{/if}

{#if made}
  <div class="card mb-4 border-emerald-800">
    <div class="mb-2 text-sm text-emerald-300">
      {made.name} is made. Give it to its client; it can be read again here with Show.
    </div>
    <div class="flex items-center gap-2">
      <code class="select-all rounded bg-neutral-900 px-2 py-1 font-mono text-sm">{made.key}</code>
      <button class="btn" onclick={() => navigator.clipboard.writeText(made!.key)}>Copy</button>
      <button class="btn" onclick={() => (made = null)}>Done</button>
    </div>
  </div>
{/if}

{#if keys}
  <div class="mb-4 text-sm text-neutral-500">
    Hashes in {keys.file}, read by both hosts. Plaintext in {keys.secrets || "nowhere: infermux-ui has no -key-secrets"}. Commit both from Changes.
  </div>

  <section class="card mb-4">
    <h2 class="mb-3 font-medium">Keys</h2>
    <table class="w-full text-sm">
      <thead class="text-left text-neutral-500">
        <tr>
          <th class="py-1 font-normal">Name</th>
          <th class="font-normal">Class</th>
          <th class="font-normal">Models</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {#each rows as r}
          <tr class="align-top">
            <td class="py-2 pr-2 font-mono">
              {r.name}
              {#if r.shown}
                <div><code class="select-all text-xs text-amber-300">{r.shown}</code></div>
              {/if}
            </td>
            <td class="py-2 pr-2">
              <select bind:value={r.class}>
                <option value="interactive">interactive</option>
                <option value="batch">batch</option>
              </select>
            </td>
            <td class="py-2 pr-2">
              <label class="flex items-center gap-2">
                <input type="checkbox" bind:checked={r.limited} /> only these
              </label>
              {#if r.limited}
                <textarea class="mt-1 h-16 w-full font-mono text-xs" bind:value={r.allow} placeholder="reaperboi/*"></textarea>
              {:else}
                <span class="text-xs text-neutral-500">every model</span>
              {/if}
            </td>
            <td class="whitespace-nowrap py-2">
              <button class="btn" onclick={() => save(r)}>Save</button>
              <button class="btn" onclick={() => reveal(r)}>{r.shown ? "Hide" : "Show"}</button>
              <button class="btn text-red-300" onclick={() => remove(r)}>Revoke</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </section>

  <section class="card">
    <h2 class="mb-3 font-medium">New key</h2>
    <div class="grid gap-3 sm:grid-cols-3">
      <label class="flex flex-col gap-1">
        <span class="label">Name</span>
        <input bind:value={draft.name} placeholder="episteme-batch" />
        <span class="text-xs text-neutral-500">Letters, digits, '.', '_' and '-'.</span>
      </label>
      <label class="flex flex-col gap-1">
        <span class="label">Class</span>
        <select bind:value={draft.class}>
          <option value="interactive">interactive: never killed</option>
          <option value="batch">batch: refused and cancelled when the GPU is wanted</option>
        </select>
      </label>
      <label class="flex flex-col gap-1">
        <span class="label"><input type="checkbox" bind:checked={draft.limited} /> Only these models</span>
        {#if draft.limited}
          <textarea class="h-16 font-mono text-xs" bind:value={draft.allow} placeholder={"zbox/*\nopenrouter/*"}></textarea>
        {/if}
        <span class="text-xs text-neutral-500">Patterns on &lt;host&gt;/&lt;model&gt; or &lt;peer&gt;/&lt;model&gt;.</span>
      </label>
    </div>
    <button class="btn-primary mt-3" disabled={!draft.name} onclick={create}>Make key</button>
  </section>
{/if}
