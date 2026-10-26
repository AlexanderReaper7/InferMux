<script lang="ts">
  import { prompt, reply, type LiveEntry } from "../lib/livetext";

  // This host's running requests and its last finished ones, followed as
  // they happen through /warden/live (0031). A request for another host's
  // model is shown on that host.
  let entries = $state<LiveEntry[]>([]);
  let connected = $state(false);
  let selected = $state<number | null>(null);
  // Follow: the newest request is shown as it arrives.
  let follow = $state(true);
  let raw = $state(false);
  let now = $state(Date.now());

  // The page keeps more finished requests than the daemon's 20, so one being
  // read is not dropped as soon as the next ends.
  const keptHere = 100;

  $effect(() => {
    const source = new EventSource("/daemon/warden/live");
    const on = (type: string, f: (data: any) => void) =>
      source.addEventListener(type, (e) => f(JSON.parse((e as MessageEvent).data)));
    const utf8 = new TextEncoder();
    const find = (id: number) => entries.find((e) => e.id === id);
    source.onopen = () => (connected = true);
    source.onerror = () => (connected = false);
    on("snapshot", (all: LiveEntry[]) => {
      // A reconnect starts again from the daemon's board; what the page
      // kept beyond it stays.
      const known = new Set(all.map((e) => e.id));
      entries = [...entries.filter((e) => !known.has(e.id) && e.done), ...all].sort((a, b) => a.id - b.id);
      connected = true;
    });
    on("start", (e: LiveEntry) => {
      entries.push(e);
      if (follow) selected = e.id;
    });
    on("head", (h: { id: number; status: number; response_type: string; response_kept: boolean }) => {
      const e = find(h.id);
      if (e) Object.assign(e, { status: h.status, response_type: h.response_type, response_kept: h.response_kept });
    });
    on("chunk", (c: { id: number; text: string }) => {
      const e = find(c.id);
      if (e) {
        e.response += c.text;
        // The daemon's count arrives with the end; this one is the kept text's.
        e.response_bytes += utf8.encode(c.text).length;
      }
    });
    on("end", (x: { id: number; status: number; duration_ms: number; response_bytes: number; response_truncated?: boolean }) => {
      const e = find(x.id);
      if (e) Object.assign(e, x, { done: true });
      const done = entries.filter((e) => e.done);
      if (done.length > keptHere) {
        const drop = new Set(done.slice(0, done.length - keptHere).map((e) => e.id));
        entries = entries.filter((e) => !drop.has(e.id) || e.id === selected);
      }
    });
    const timer = setInterval(() => (now = Date.now()), 500);
    return () => {
      source.close();
      clearInterval(timer);
    };
  });

  const newestFirst = $derived([...entries].reverse());
  const running = $derived(entries.filter((e) => !e.done).length);
  const current = $derived(entries.find((e) => e.id === selected) ?? null);
  const asked = $derived(current?.request_kept ? prompt(current.request) : null);
  const answered = $derived(current?.response_kept ? reply(current.response, current.response_type) : null);

  function pick(id: number) {
    selected = id;
    follow = false;
  }

  function time(iso: string) {
    return new Date(iso).toLocaleTimeString([], { hour12: false });
  }

  function seconds(e: LiveEntry) {
    // The browser's clock is not the daemon's; a running request never reads
    // below zero.
    const ms = e.done ? (e.duration_ms ?? 0) : Math.max(0, now - new Date(e.time).getTime());
    return `${(ms / 1000).toFixed(1)} s`;
  }

  function bytes(n: number) {
    if (n < 0) return "? B";
    if (n < 1000) return `${n} B`;
    if (n < 1e6) return `${(n / 1e3).toFixed(1)} kB`;
    return `${(n / 1e6).toFixed(1)} MB`;
  }

  function status(e: LiveEntry) {
    if (!e.done) return e.status ? "streaming" : "waiting";
    return String(e.status);
  }

  function statusColour(e: LiveEntry) {
    if (!e.done) return "text-sky-300";
    return e.status >= 400 ? "text-red-300" : "text-neutral-400";
  }

  // The reply's pane stays at its end while the request runs, unless the
  // user scrolled up to read.
  let pane = $state<HTMLElement | null>(null);
  let pinned = $state(true);
  $effect(() => {
    void current?.response;
    if (pane && pinned) pane.scrollTop = pane.scrollHeight;
  });
</script>

<p class="mb-4 text-sm text-neutral-500">
  This host's requests for its models while they run: the request as it arrived and the reply as the model writes it.
  The last finished ones stay listed. Bodies over 4 MB are cut, and audio, images and uploads are listed by size only.
</p>

<div class="grid grid-cols-[minmax(0,32rem)_minmax(0,1fr)] gap-4">
  <section class="card h-[calc(100vh-12rem)] overflow-y-auto p-0">
    <div class="flex items-center gap-3 border-b border-neutral-800 px-4 py-2 text-sm">
      <span class={connected ? "text-emerald-300" : "text-red-300"}>{connected ? "following" : "reconnecting"}</span>
      <span class="text-neutral-500">{running} running</span>
      <label class="ml-auto flex items-center gap-2 text-neutral-400">
        <input type="checkbox" bind:checked={follow} /> show the newest
      </label>
    </div>
    {#if !entries.length}
      <div class="px-4 py-3 text-sm text-neutral-500">No requests since the page opened or the daemon started.</div>
    {/if}
    {#each newestFirst as e (e.id)}
      <button
        onclick={() => pick(e.id)}
        class="block w-full border-b border-neutral-900 px-4 py-2 text-left text-sm hover:bg-neutral-900 {e.id === selected
          ? 'bg-neutral-900'
          : ''}"
      >
        <div class="flex items-center gap-3">
          <span class="font-mono text-neutral-500">{time(e.time)}</span>
          <span class="truncate text-neutral-100">{e.model}</span>
          <span class="ml-auto font-mono {statusColour(e)}">{status(e)}</span>
        </div>
        <div class="flex gap-3 text-xs text-neutral-500">
          <span>{e.client || "no key"}</span>
          <span class="truncate">{e.path}</span>
          <span class="ml-auto font-mono">{seconds(e)} · {bytes(e.response_bytes)}</span>
        </div>
      </button>
    {/each}
  </section>

  {#if current}
    <section class="flex h-[calc(100vh-12rem)] min-w-0 flex-col gap-4">
      <div class="card flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
        <span class="font-medium text-neutral-100">{current.model}</span>
        <span class="text-neutral-400">{current.client || "no key"}</span>
        <span class="font-mono text-neutral-400">{current.path}</span>
        <span class="font-mono {statusColour(current)}">{status(current)}</span>
        <span class="font-mono text-neutral-400">{seconds(current)}</span>
        <label class="ml-auto flex items-center gap-2 text-neutral-400">
          <input type="checkbox" bind:checked={raw} /> raw
        </label>
      </div>

      <div class="grid min-h-0 flex-1 grid-cols-2 gap-4">
        <div class="card min-h-0 overflow-y-auto">
          <h2 class="label mb-2">
            Request · {bytes(current.request_bytes)}{current.request_truncated ? " · cut at 4 MB" : ""}
          </h2>
          {#if !current.request_kept}
            <div class="text-sm text-neutral-500">{current.request_type || "a body"} that is not text, not kept.</div>
          {:else if raw || !asked}
            <pre class="text-sm break-words whitespace-pre-wrap text-neutral-200">{current.request}</pre>
          {:else}
            {#each asked.messages as m}
              <div class="mb-3">
                <div class="label">{m.role}</div>
                <div class="text-sm break-words whitespace-pre-wrap text-neutral-200">{m.text}</div>
              </div>
            {/each}
            <details class="mt-2 text-sm">
              <summary class="cursor-pointer text-neutral-500">parameters</summary>
              <pre class="mt-1 break-words whitespace-pre-wrap text-neutral-400">{JSON.stringify(asked.rest, null, 2)}</pre>
            </details>
          {/if}
        </div>

        <div
          class="card min-h-0 overflow-y-auto"
          bind:this={pane}
          onscroll={(ev) => {
            const el = ev.currentTarget;
            pinned = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
        >
          <h2 class="label mb-2">
            Reply · {bytes(current.response_bytes)}{current.response_truncated ? " · cut at 4 MB" : ""}
          </h2>
          {#if !current.status && !current.done}
            <div class="text-sm text-neutral-500">Waiting for the model: a load, a swap or the queue.</div>
          {:else if !current.response_kept}
            <div class="text-sm text-neutral-500">{current.response_type || "a body"} that is not text, not kept.</div>
          {:else if raw || !answered?.parsed}
            <pre class="text-sm break-words whitespace-pre-wrap text-neutral-200">{current.response}</pre>
          {:else}
            {#if answered.reasoning}
              <div class="label">reasoning</div>
              <div class="mb-3 text-sm break-words whitespace-pre-wrap text-neutral-500">{answered.reasoning}</div>
            {/if}
            {#if answered.text}
              <div class="mb-3 text-sm break-words whitespace-pre-wrap text-neutral-100">{answered.text}</div>
            {/if}
            {#each answered.tools as t}
              <div class="label">tool call</div>
              <div class="mb-3 font-mono text-sm break-words whitespace-pre-wrap text-amber-200">{t.name}({t.arguments})</div>
            {/each}
          {/if}
        </div>
      </div>
    </section>
  {:else}
    <section class="card text-sm text-neutral-500">Pick a request.</section>
  {/if}
</div>
