<script lang="ts">
  import { forceLinting } from "@codemirror/lint";
  import { EditorState } from "@codemirror/state";
  import { EditorView } from "@codemirror/view";
  import { untrack } from "svelte";
  import { ago, api, gib } from "../lib/api";
  import { addToLane, mainFile, modelFile, modelNames, sources as draftSources } from "../lib/draft";
  import { extensions, type Context } from "../lib/editor";
  import { live } from "../lib/live.svelte";
  import type { Check, ConfigState, GGUF, Published, Route, Schema, Summary, UIState } from "../lib/types";
  import type { Sources } from "../lib/yamlschema";
  import AddModel from "./AddModel.svelte";
  import Merge from "./Merge.svelte";

  // The one config (0022, 9). The draft is the browser's until Publish, kept
  // in localStorage with the version it began from, so a reload loses
  // nothing. Publish signs it on this host and the daemon sends it to the
  // others; the check beside the editor is the daemons' own loader.
  const draftKey = "infermux-config-draft";

  let ui = $state<UIState | null>(null);
  let schema = $state<{ main: Schema; model: Schema } | null>(null);
  let ggufs = $state<GGUF[]>([]);
  let cstate = $state<ConfigState | null>(null);
  let copy = $state<{ dir: string; copy: Summary | null } | null>(null);
  // The running version's files and hash; null files before the first.
  let running = $state<Record<string, string> | null>(null);
  let runningHash = $state("");
  let files = $state<Record<string, string>>({});
  let base = $state("");
  let current = $state(mainFile);
  let checked = $state<Check | null>(null);
  // The last check without an error: the placement table keeps it while
  // the draft is broken.
  let good = $state<Check | null>(null);
  let checkedFiles: Record<string, string> = {};
  let error = $state<string | null>(null);
  let loaded = $state(false);

  function same(a: Record<string, string> | null, b: Record<string, string> | null) {
    const ka = Object.keys(a ?? {});
    return ka.length === Object.keys(b ?? {}).length && ka.every((k) => b![k] === a![k]);
  }
  function plain(f: Record<string, string>): Record<string, string> {
    return JSON.parse(JSON.stringify(f));
  }
  const dirty = $derived(!same(files, running));
  const stale = $derived(loaded && base !== runningHash);

  function readDraft(): { base: string; files: Record<string, string> } | null {
    try {
      const d = JSON.parse(localStorage.getItem(draftKey) ?? "null");
      return d && typeof d.base === "string" && d.files && typeof d.files === "object" ? d : null;
    } catch {
      return null;
    }
  }

  function starter(host: string): Record<string, string> {
    return {
      [mainFile]: `# The first version. Each host's warden.yaml names it under hosts.\nhosts:\n  ${host}:\n    url: https://${host}.<tailnet>.ts.net\n    devices:\n      gpu: {kind: gpu}\n      cpu: {kind: cpu}\nbalance: {}\n`,
    };
  }

  async function refreshRunning(st: ConfigState) {
    const hash = st.current?.hash ?? "";
    if (hash === runningHash && (running || !hash)) return;
    running = (await api.runningConfig())?.files ?? null;
    runningHash = hash;
  }

  async function load() {
    try {
      const [u, sc, g, st, cp] = await Promise.all([api.state(), api.configSchema(), api.ggufs(), api.configState(), api.configCopy()]);
      ui = u;
      schema = sc;
      ggufs = g;
      cstate = st;
      copy = cp;
      await refreshRunning(st);
      const saved = readDraft();
      if (saved) {
        files = saved.files;
        base = saved.base;
      } else {
        files = plain(running ?? starter(st.host || u.warden.host || "this-host"));
        base = runningHash;
      }
      loaded = true;
      error = null;
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  // The daemon's state: another host's Publish, a conflict, the files.
  async function poll() {
    if (!loaded) return;
    try {
      const st = await api.configState();
      const clean = same(files, running) && base === runningHash;
      cstate = st;
      await refreshRunning(st);
      // A draft without edits follows the running version.
      if (clean && base !== runningHash) {
        files = plain(running ?? {});
        base = runningHash;
      }
    } catch {
      // The header shows a daemon that cannot be reached.
    }
  }
  $effect(() => {
    const timer = setInterval(poll, 5000);
    return () => clearInterval(timer);
  });

  let seq = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    if (!loaded) return;
    const snap = plain(files);
    const b = base;
    clearTimeout(timer);
    timer = setTimeout(() => runCheck(snap, b), 300);
  });
  async function runCheck(snap: Record<string, string>, b: string) {
    if (same(snap, untrack(() => running))) localStorage.removeItem(draftKey);
    else localStorage.setItem(draftKey, JSON.stringify({ base: b, files: snap }));
    const n = ++seq;
    try {
      const res = await api.checkConfig(snap);
      if (n !== seq) return;
      checked = res;
      checkedFiles = snap;
      if (!res.error) good = res;
      if (view) forceLinting(view);
    } catch (e) {
      if (n === seq) error = (e as Error).message;
    }
  }

  // The editor: one state per file, so each keeps its undo history.
  let lastSources: Sources = { place: [], device: [], model: [], lane: [], gguf: [] };
  const sources = $derived.by(() => {
    const s = draftSources(files, ggufs.map((g) => g.path));
    if (s) lastSources = s;
    return lastSources;
  });
  function ctxFor(name: string): Context {
    return {
      schema: () => (schema ? (name === mainFile ? schema.main : schema.model) : null),
      sources: () => sources,
      problem: (text) => {
        const p = checked?.error;
        return p && p.file === name && checkedFiles[name] === text ? { line: p.line, message: p.message } : null;
      },
      runtimes: () => (name === mainFile || !ui ? [] : Object.keys(ui.runtimes)),
      kvKernels: () => (name === mainFile ? null : (ui?.kv_kernels ?? null)),
    };
  }
  let host = $state<HTMLDivElement>();
  let view: EditorView | null = null;
  const states = new Map<string, EditorState>();
  function stateFor(name: string) {
    let s = states.get(name);
    const text = files[name] ?? "";
    if (!s || s.doc.toString() !== text) {
      s = EditorState.create({ doc: text, extensions: extensions(ctxFor(name), (t) => (files[name] = t)) });
      states.set(name, s);
    }
    return s;
  }
  $effect(() => {
    if (!host || !loaded) return;
    view = new EditorView({ parent: host, state: untrack(() => stateFor(current)) });
    return () => {
      view?.destroy();
      view = null;
    };
  });
  function open(name: string) {
    if (view) states.set(current, view.state);
    current = name;
    view?.setState(stateFor(name));
  }
  // An edit from outside the editor: a comparison, an added model, a reload.
  $effect(() => {
    const text = files[current];
    if (!view) return;
    if (text === undefined) return untrack(() => open(mainFile));
    if (view.state.doc.toString() !== text) view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
  });

  const fileNames = $derived([...(mainFile in files ? [mainFile] : []), ...modelNames(files).map(modelFile)]);

  function remove(name: string) {
    if (!confirm(`Remove ${name} from the draft? Its places in the lanes stay, and the check points at them.`)) return;
    delete files[name];
    states.delete(name);
  }

  let adding = $state<null | "gguf" | "hf" | "empty">(null);
  function added(name: string, text: string, place: string) {
    files[modelFile(name)] = text;
    if (place) files[mainFile] = addToLane(files[mainFile] ?? "", "balance", name, place);
    adding = null;
    open(modelFile(name));
  }
  const namePattern = $derived(new RegExp(schema?.main.properties?.balance?.propertyNames?.pattern ?? "^[A-Za-z0-9][A-Za-z0-9._-]*$"));

  // Publish.
  let passphrase = $state("");
  // Without -config-passphrase-command, Publish needs the passphrase typed.
  const signable = $derived(!!ui?.config_passphrase || !!passphrase);
  let publishing = $state(false);
  let notice = $state<{ text: string; dir: string } | null>(null);
  let publishError = $state<string | null>(null);
  let merge = $state<{ other: Record<string, string>; label: string; kind: "stale" | "lost" } | null>(null);
  const results: Record<Published["result"], string> = {
    applied: "runs here and goes to the other hosts",
    replaced: "runs; it replaced a version edited apart from it, which every host keeps as the conflict's loser",
    same: "was already running",
    old: "is older than the running version and does not run",
    lost: "is kept as a conflict's loser and does not run",
  };

  function version(s: Summary) {
    return `revision ${s.revision} from ${s.host}, ${ago(s.time)}`;
  }
  function label(s: Summary | null | undefined, what: string) {
    return s ? `${what}, ${version(s)}` : what;
  }

  async function publish(force: boolean) {
    publishing = true;
    notice = publishError = null;
    try {
      const snap = plain(files);
      const res = await api.publish(snap, base, passphrase, force);
      passphrase = "";
      cstate = res.state;
      running = snap;
      runningHash = base = res.published.hash;
      localStorage.removeItem(draftKey);
      merge = null;
      copy = await api.configCopy();
      notice = { text: `Revision ${res.published.revision} ${results[res.result]}.`, dir: copy.dir };
    } catch (e) {
      const m = (e as Error).message;
      if (m.startsWith("409:")) {
        await compareRunning();
        publishError = "Another Publish came first. Compare it with the draft above.";
      } else publishError = m;
    } finally {
      publishing = false;
    }
  }

  async function compareRunning() {
    try {
      const st = await api.configState();
      cstate = st;
      await refreshRunning(st);
      merge = { other: running ?? {}, label: label(st.current, "the running version"), kind: "stale" };
    } catch (e) {
      error = (e as Error).message;
    }
  }
  // The draft takes the running version as its base: what the comparison
  // showed and the user kept is the edit.
  function rebase() {
    base = runningHash;
    merge = null;
    publishError = null;
  }

  async function compareLost() {
    const c = cstate?.conflict;
    if (!c) return;
    try {
      merge = { other: (await api.configVersion(c.lost.hash)).files, label: label(c.lost, "the version that lost"), kind: "lost" };
    } catch (e) {
      error = (e as Error).message;
    }
  }
  async function dismiss() {
    try {
      await api.dismissConflict();
      cstate = await api.configState();
    } catch (e) {
      error = (e as Error).message;
    }
  }
  async function copyRunning() {
    try {
      copy = await api.copyConfig();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  // The placement table: a model per row, a device per column, from the
  // draft as the loader reads it.
  const columns = $derived(
    good
      ? Object.keys(good.devices)
          .sort()
          .flatMap((h) =>
            Object.keys(good!.devices[h])
              .sort()
              .map((d) => ({ host: h, device: d, kind: good!.devices[h][d] })),
          )
      : [],
  );
  const rows = $derived([...new Set([...Object.keys(good?.routes ?? {}), ...modelNames(files)])].sort());
  function cells(model: string) {
    let b = 0;
    let f = 0;
    return (good?.routes[model] ?? []).map((r) => ({ ...r, order: r.failover ? `F${++f}` : `${++b}` }));
  }
  // The llama-swap model a place is on its own host.
  function instance(model: string, r: Route) {
    return r.place === r.host ? model : r.place.slice(r.host.length + 1);
  }
  const missing = $derived((cstate?.files ?? []).filter((f) => f.state !== "present"));
</script>

{#if error}
  <div class="card mb-4 whitespace-pre-wrap text-red-300">{error}</div>
{/if}

{#if cstate?.rejected}
  <div class="card mb-4 border-red-900 text-sm text-red-300">
    {ago(cstate.rejected.at)}, {cstate.rejected.from} offered version {cstate.rejected.hash.slice(0, 12)}, and this host refused it: {cstate.rejected.error}
  </div>
{/if}

{#if cstate?.conflict}
  {@const c = cstate.conflict}
  <div class="card mb-4 flex items-center gap-4 border-amber-900 text-sm text-amber-300">
    <span>
      {ago(c.at)}: revision {c.kept.revision} from {c.kept.host} and revision {c.lost.revision} from {c.lost.host} were edited apart. The later one
      runs; the other is kept here.
    </span>
    <button class="btn ml-auto" onclick={compareLost}>Compare it with the draft</button>
    <button class="btn" onclick={dismiss}>Dismiss on every host</button>
  </div>
{/if}

{#if stale && dirty && !merge}
  <div class="card mb-4 flex items-center gap-4 border-amber-900 text-sm text-amber-300">
    <span>
      This draft began from {base ? base.slice(0, 12) : "no version"}; {label(cstate?.current, "the running version")} came after it.
    </span>
    <button class="btn ml-auto" onclick={compareRunning}>Compare</button>
  </div>
{/if}

{#if merge}
  <Merge {files} other={merge.other} label={merge.label} onedit={(n, t) => (files[n] = t)}>
    {#if merge.kind === "stale"}
      <button class="btn-primary" onclick={rebase} title="The draft as it is now, edited from the running version">Build on the running version</button>
      <button class="btn-danger" disabled={!signable || publishing} onclick={() => publish(true)} title={ui?.config_passphrase ? "" : "Needs the passphrase below"}
        >Publish over it</button
      >
    {/if}
    <button class="btn" onclick={() => (merge = null)}>Close</button>
  </Merge>
{/if}

{#if adding && ui}
  <AddModel
    from={adding}
    {files}
    {ggufs}
    runtimes={Object.keys(ui.runtimes)}
    places={sources.place}
    ggufDir={ui.paths.gguf_dirs[0] ?? "/srv/models"}
    {namePattern}
    onadd={added}
    onclose={() => (adding = null)}
  />
{/if}

<div class="grid grid-cols-[minmax(0,1fr)_28rem] gap-4">
  <div>
    <div class="mb-2 flex flex-wrap items-center gap-1">
      {#each fileNames as f}
        <span class="group flex items-center rounded {current === f ? 'bg-neutral-800' : ''}">
          <button class="px-2 py-0.5 font-mono text-sm {current === f ? 'text-neutral-100' : 'text-neutral-400 hover:text-neutral-200'}" onclick={() => open(f)}
            >{f === mainFile ? f : f.slice("models/".length)}{running?.[f] !== files[f] ? " •" : ""}</button
          >
          {#if f !== mainFile}
            <button class="hidden pr-2 text-neutral-500 group-hover:inline hover:text-red-300" title="Remove from the draft" onclick={() => remove(f)}>×</button>
          {/if}
        </span>
      {/each}
      <span class="ml-auto flex gap-2">
        <button class="btn" onclick={() => (adding = "gguf")}>Model from a GGUF here</button>
        <button class="btn" onclick={() => (adding = "hf")}>Model from Hugging Face</button>
        <button class="btn" onclick={() => (adding = "empty")}>Empty model</button>
      </span>
    </div>
    <div bind:this={host} class="h-[60vh] min-h-96 overflow-hidden rounded border border-neutral-800"></div>
    <div class="mt-1 text-xs text-neutral-500">Ctrl-Space completes; hovering a key shows what it does.</div>
  </div>

  <!-- A path breaks after a slash, and inside a name only if one name is wider than the panel. -->
  {#snippet path(p: string)}{#each p.split("/") as part, i}{#if i > 0}/<wbr />{/if}{part}{/each}{/snippet}
  <div class="space-y-4 wrap-break-word">
    <div class="card">
      <h2 class="mb-2 font-medium">Publish</h2>
      <div class="mb-3 text-sm text-neutral-400">
        {cstate?.current ? `Running ${version(cstate.current)}.` : "No signed config runs yet."}
        <div class="text-neutral-500">
          {#if !dirty}The draft is the running version.{:else}{@const n = Object.keys({ ...files, ...running }).filter((f) => running?.[f] !== files[f]).length}{n}
            {n === 1 ? "file" : "files"} changed.{/if}
        </div>
      </div>
      <div class="flex gap-2">
        {#if !ui?.config_passphrase}
          <input type="password" autocomplete="off" class="flex-1" placeholder="config key passphrase" bind:value={passphrase} />
        {/if}
        <button class="btn-primary" disabled={!dirty || !!checked?.error || !signable || publishing} onclick={() => publish(false)}>Publish</button>
      </div>
      <div class="mt-1 text-xs text-neutral-500">Signs the draft on this host. The daemon runs it and sends it to the other hosts.</div>
      {#if notice}<div class="mt-2 text-sm text-emerald-300">{notice.text} nixcfg's copy in {@render path(notice.dir)} is written; Commit records it.</div>{/if}
      {#if publishError}<div class="mt-2 text-sm whitespace-pre-wrap text-red-300">{publishError}</div>{/if}
    </div>

    <div class="card">
      <h2 class="mb-2 font-medium">Check</h2>
      {#if checked?.error}
        {@const p = checked.error}
        <button class="block text-left text-sm text-red-300 hover:underline" disabled={!p.file} onclick={() => p.file && open(p.file)}>
          {p.file ? `${p.file}${p.line ? `:${p.line}` : ""}: ` : ""}{p.message}
        </button>
      {:else if checked}
        <div class="text-sm text-emerald-300">Every host can run it.</div>
      {:else}
        <div class="text-sm text-neutral-500">Checking.</div>
      {/if}
      {#each checked?.warnings ?? [] as w}<div class="mt-1 text-sm text-amber-300">{w}</div>{/each}
      {#if cstate?.warnings.length}
        <div class="mt-3 label">The running version</div>
        {#each cstate.warnings as w}<div class="mt-1 text-sm text-amber-300">{w}</div>{/each}
      {/if}
    </div>

    <div class="card">
      <h2 class="mb-2 font-medium">nixcfg's copy</h2>
      <div class="font-mono text-xs text-neutral-500">{@render path(copy?.dir ?? "")}</div>
      <div class="mt-1 text-sm text-neutral-400">
        {copy?.copy ? `Holds ${version(copy.copy)}.` : "None yet."}
        {#if copy?.copy && copy.copy.hash === runningHash}<span class="text-emerald-300">The running version.</span>{/if}
      </div>
      {#if runningHash && copy?.copy?.hash !== runningHash}
        <button class="btn mt-2" onclick={copyRunning}>Copy the running version here</button>
        <div class="mt-1 text-xs text-neutral-500">A host starts from this copy when it has no newer version. Commit records it.</div>
      {/if}
    </div>

    {#if missing.length}
      <div class="card">
        <h2 class="mb-2 font-medium">Files this host is fetching</h2>
        {#each missing as f}
          <div class="border-t border-neutral-900 py-1 text-sm">
            <div class="font-mono text-xs">{f.path}</div>
            <div class={f.state === "failed" || f.state === "unfetchable" ? "text-red-300" : "text-neutral-400"}>
              {f.state}{f.total ? `, ${gib(f.done ?? 0)} of ${gib(f.total)}` : ""}{f.from ? ` from ${f.from}` : ""}{f.error ? `: ${f.error}` : ""}
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </div>
</div>

{#if good}
  <div class="card mt-4 overflow-x-auto">
    <h2 class="mb-1 font-medium">Where each model runs</h2>
    <div class="mb-3 text-sm text-neutral-500">
      The draft as the hosts would run it. 1, 2: the balance lane in order, each place up to its limit before the next. F1: the failover lane, used
      only when every balance place is down or refusing. {cstate?.host ? `A dot on ${cstate.host}'s devices is the model running there now.` : ""}
    </div>
    <table class="text-sm">
      <thead class="text-left text-neutral-500">
        <tr>
          <th class="py-1 pr-6 font-normal">Model</th>
          {#each columns as c}<th class="px-3 font-normal"><span class="text-neutral-300">{c.host}</span>/{c.device} <span class="text-xs">{c.kind}</span></th>{/each}
        </tr>
      </thead>
      <tbody>
        {#each rows as m}
          {@const cs = cells(m)}
          <tr class="border-t border-neutral-900">
            <td class="py-1.5 pr-6 font-mono">
              <button class="hover:underline" onclick={() => modelFile(m) in files && open(modelFile(m))}>{m}</button>
              {#if !cs.length}<span class="ml-2 text-xs text-neutral-500">not placed</span>{/if}
            </td>
            {#each columns as c}
              <td class="px-3">
                {#each cs.filter((r) => r.host === c.host && r.device === c.device) as r}
                  {@const state = r.host === cstate?.host ? live.state?.models[instance(m, r)] : undefined}
                  <span
                    class="mr-1 inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-xs {r.failover ? 'bg-neutral-900 text-neutral-400' : 'bg-sky-950 text-sky-200'}"
                    title={state ? `${instance(m, r)}: ${state}` : instance(m, r)}
                  >
                    {r.order}{r.max_inflight ? ` ≤${r.max_inflight}` : ""}{r.only_if_idle ? " idle" : ""}{r.batch_only ? " batch" : ""}
                    {#if state}<span class="h-1.5 w-1.5 rounded-full {state === 'ready' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>{/if}
                  </span>
                {/each}
              </td>
            {/each}
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
