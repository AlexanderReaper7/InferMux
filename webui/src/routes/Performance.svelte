<script lang="ts">
  import { api, ms, percent, rate } from "../lib/api";
  import type { HostStats, RequestStat, Spread } from "../lib/types";

  // Every host's last requests, kept in each daemon's memory (0014). A
  // restart empties a host's list.
  let hosts = $state<HostStats[]>([]);
  let error = $state<string | null>(null);
  let shown = $state(50);

  async function load() {
    try {
      hosts = (await api.requests(true)).hosts;
      error = null;
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  });

  function time(iso: string) {
    return new Date(iso).toLocaleTimeString([], { hour12: false });
  }

  function bytes(n: number) {
    if (n < 1000) return `${n} B`;
    if (n < 1e6) return `${(n / 1e3).toFixed(1)} kB`;
    return `${(n / 1e6).toFixed(1)} MB`;
  }

  function closed(r: RequestStat) {
    const s = r.session;
    if (!s?.close_code) return "";
    return `${s.close_code} by ${s.closed_by}`;
  }

  function tokens(r: RequestStat) {
    if (r.prompt_tokens == null) return "";
    return r.cached_tokens ? `${r.prompt_tokens} (${r.cached_tokens} cached)` : String(r.prompt_tokens);
  }
</script>

{#snippet spread(s: Spread | null, fmt: (v: number) => string)}
  {#if s}{fmt(s.median)} <span class="text-neutral-500">/ {fmt(s.p95)}</span>{/if}
{/snippet}

{#if error}
  <div class="card mb-4 text-red-300">{error}</div>
{/if}

<p class="mb-4 text-sm text-neutral-500">
  Each host's last 500 requests for the models it serves, in its memory since it started. Summaries are median / 95th
  percentile over the successful ones. TTFT is from arrival to the first generated token, a load or swap included; wait
  is TTFT minus llama-server's prefill. A cloud model's decode rate is measured here, from the first token to the end.
  An audio reply's TTFT is its first chunk.
</p>
<p class="mb-4 text-sm text-neutral-500">
  A WebSocket session is one row, added when it ends (0018). First output is from the client's first data frame to the
  backend's first transcript or audio; a session's TTFT, from arrival, includes the time before the user started, so it
  stays out of the model's TTFT. Active is the 10 s after each data frame, idle the rest. In and out are every byte after
  the upgrade.
</p>

{#each hosts as h (h.host)}
  <section class="card mb-4">
    <h2 class="mb-3 font-medium">{h.host}</h2>
    {#if h.error}
      <div class="text-sm text-red-300">{h.error}</div>
    {:else if !h.requests?.length}
      <div class="text-sm text-neutral-500">No requests since it started.</div>
    {:else}
      <table class="mb-4 w-full text-sm">
        <thead class="text-left text-neutral-500">
          <tr>
            <th class="py-1 font-normal">Model</th>
            <th class="text-right font-normal">Requests</th>
            <th class="text-right font-normal">Sessions</th>
            <th class="text-right font-normal">TTFT</th>
            <th class="text-right font-normal">First output</th>
            <th class="text-right font-normal">Wait</th>
            <th class="text-right font-normal">Prefill</th>
            <th class="text-right font-normal">Prefill tok/s</th>
            <th class="text-right font-normal">Decode tok/s</th>
            <th class="text-right font-normal">Cached</th>
            <th class="text-right font-normal">Drafts accepted</th>
          </tr>
        </thead>
        <tbody>
          {#each h.models ?? [] as m (m.model)}
            <tr class="border-t border-neutral-900">
              <td class="py-1">{m.model}</td>
              <td class="text-right">{m.requests}{#if m.failed}<span class="text-red-400"> ({m.failed} failed)</span>{/if}</td>
              <td class="text-right">{m.sessions || ""}</td>
              <td class="text-right">{@render spread(m.ttft_ms, ms)}</td>
              <td class="text-right">{@render spread(m.first_output_ms ?? null, ms)}</td>
              <td class="text-right">{@render spread(m.wait_ms, ms)}</td>
              <td class="text-right">{@render spread(m.prefill_ms, ms)}</td>
              <td class="text-right">{@render spread(m.prefill_per_second, rate)}</td>
              <td class="text-right">{@render spread(m.decode_per_second, rate)}</td>
              <td class="text-right">{percent(m.cache_share)}</td>
              <td class="text-right">{percent(m.draft_acceptance)}</td>
            </tr>
          {/each}
        </tbody>
      </table>

      {@const sessions = h.requests.filter((r) => r.session)}
      {@const requests = h.requests.filter((r) => !r.session)}
      {#if sessions.length}
        <h3 class="mb-1 text-sm text-neutral-400">Sessions</h3>
        <table class="mb-4 w-full text-sm">
          <thead class="text-left text-neutral-500">
            <tr>
              <th class="py-1 font-normal">Time</th>
              <th class="font-normal">Model</th>
              <th class="font-normal">Client</th>
              <th class="font-normal">Path</th>
              <th class="text-right font-normal" title="arrival to the 101">Upgrade</th>
              <th class="text-right font-normal" title="the client's first data frame to the backend's first output">First output</th>
              <th class="text-right font-normal" title="arrival to the first output">TTFT</th>
              <th class="text-right font-normal">Active</th>
              <th class="text-right font-normal">Idle</th>
              <th class="text-right font-normal" title="every byte after the upgrade, client to backend">In</th>
              <th class="text-right font-normal" title="every byte after the upgrade, backend to client">Out</th>
              <th class="text-right font-normal">Total</th>
              <th class="text-right font-normal" title="the first close frame, either way">Closed</th>
            </tr>
          </thead>
          <tbody>
            {#each sessions.slice(0, shown) as r (r.id)}
              <tr class="border-t border-neutral-900">
                <td class="py-1 text-neutral-400">{time(r.time)}</td>
                <td>{r.model}</td>
                <td class="text-neutral-400">{r.client}</td>
                <td class="text-neutral-500">{r.path}</td>
                <td class="text-right">{ms(r.session?.upgrade_ms)}</td>
                <td class="text-right">{ms(r.session?.first_output_ms)}</td>
                <td class="text-right">{ms(r.ttft_ms)}</td>
                <td class="text-right">{ms(r.session?.active_ms)}</td>
                <td class="text-right text-neutral-400">{ms(r.session?.idle_ms)}</td>
                <td class="text-right text-neutral-400">{bytes(r.request_bytes ?? 0)}</td>
                <td class="text-right text-neutral-400">{bytes(r.response_bytes ?? 0)}</td>
                <td class="text-right text-neutral-400">{ms(r.duration_ms)}</td>
                <td class="text-right {r.session?.close_code === 1000 || !r.session?.close_code ? 'text-neutral-400' : 'text-amber-300'}"
                  >{closed(r)}</td
                >
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}

      <table class="w-full text-sm">
        <thead class="text-left text-neutral-500">
          <tr>
            <th class="py-1 font-normal">Time</th>
            <th class="font-normal">Model</th>
            <th class="font-normal">Client</th>
            <th class="font-normal">Path</th>
            <th class="text-right font-normal">Status</th>
            <th class="text-right font-normal">Prompt</th>
            <th class="text-right font-normal">Output</th>
            <th class="text-right font-normal">TTFT</th>
            <th class="text-right font-normal">Prefill</th>
            <th class="text-right font-normal">Prefill tok/s</th>
            <th class="text-right font-normal">Decode tok/s</th>
            <th class="text-right font-normal">Drafts</th>
            <th class="text-right font-normal" title="the request's body, uncompressed">In</th>
            <th class="text-right font-normal" title="the reply's body, uncompressed">Out</th>
            <th class="text-right font-normal">Total</th>
          </tr>
        </thead>
        <tbody>
          {#each requests.slice(0, shown) as r (r.id)}
            <tr class="border-t border-neutral-900">
              <td class="py-1 text-neutral-400">{time(r.time)}</td>
              <td>{r.model}</td>
              <td class="text-neutral-400">{r.client}</td>
              <td class="text-neutral-500">{r.path}{r.stream ? "" : " (whole)"}</td>
              <td class="text-right {r.status === 200 ? 'text-neutral-400' : 'text-red-400'}">{r.status}</td>
              <td class="text-right">{tokens(r)}</td>
              <td class="text-right">{r.output_tokens ?? ""}</td>
              <td class="text-right">{ms(r.ttft_ms)}</td>
              <td class="text-right">{ms(r.prefill_ms)}</td>
              <td class="text-right">{rate(r.prefill_per_second)}</td>
              <td class="text-right" title={r.rates_from === "client" ? "measured here, after the first token" : ""}
                >{rate(r.decode_per_second)}{r.rates_from === "client" ? "*" : ""}</td
              >
              <td class="text-right">{r.draft_tokens ? `${r.draft_accepted}/${r.draft_tokens}` : ""}</td>
              <td class="text-right text-neutral-400">{bytes(r.request_bytes ?? 0)}</td>
              <td class="text-right text-neutral-400">{bytes(r.response_bytes ?? 0)}</td>
              <td class="text-right text-neutral-400">{ms(r.duration_ms)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
      {#if Math.max(requests.length, sessions.length) > shown}
        <button class="btn mt-2" onclick={() => (shown = Infinity)}>Show all {h.requests.length}</button>
      {/if}
    {/if}
  </section>
{/each}
