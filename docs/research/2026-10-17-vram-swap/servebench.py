"""servebench: times one llama-server started by hand on a spare port.
Throwaway measurement tool for docs/research/2026-10-17-vram-swap.md.

  servebench.py ready <port> <t0>          seconds from t0 (time.time()) to /health 200
  servebench.py embed <port> single <n>    n one-sentence queries, p50/max ms
  servebench.py embed <port> batch <n>     n rounds of 8 code chunks of ~1000 tokens, s per round
  servebench.py chat <port> <n>            n completions of 128 tokens, pp and tg t/s from the server's timings
"""

import glob
import http.client
import json
import statistics
import sys
import time


def post(port, path, body, timeout=600):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    c.request("POST", path, json.dumps(body), {"Content-Type": "application/json"})
    r = c.getresponse()
    data = r.read()
    if r.status != 200:
        raise RuntimeError(f"{path}: {r.status} {data[:200]!r}")
    return json.loads(data)


def chunks():
    # Go source from this repository, cut to about 1000 tokens each.
    text = ""
    for f in sorted(glob.glob("/home/alexander/Projects/.worktrees/InferMux/research-vram-swap/internal/warden/*.go")):
        text += open(f).read()
    return [text[i * 3500 : (i + 1) * 3500] for i in range(8)]


def main():
    cmd, port = sys.argv[1], int(sys.argv[2])
    if cmd == "ready":
        t0 = float(sys.argv[3])
        while True:
            try:
                c = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
                c.request("GET", "/health")
                if c.getresponse().status == 200:
                    break
            except OSError:
                pass
            time.sleep(0.02)
        print(f"ready {time.time() - t0:.2f} s")
    elif cmd == "embed":
        mode, n = sys.argv[3], int(sys.argv[4])
        if mode == "single":
            ts = []
            for i in range(n):
                t = time.monotonic()
                post(port, "/v1/embeddings", {"input": f"where does the warden decide to yield the card {i}"})
                ts.append((time.monotonic() - t) * 1000)
            print(f"single n={n} p50 {statistics.median(ts):.0f} ms max {max(ts):.0f} ms first {ts[0]:.0f} ms")
        else:
            cs = chunks()
            ts = []
            for i in range(n):
                t = time.monotonic()
                for c in cs:
                    r = post(port, "/v1/embeddings", {"input": c})
                ts.append(time.monotonic() - t)
            tok = r.get("usage", {}).get("prompt_tokens", 0)
            print(f"batch 8 chunks (last {tok} tokens) n={n}: " + ", ".join(f"{t:.2f}" for t in ts) + " s")
    elif cmd == "chat":
        n = int(sys.argv[3])
        prompt = chunks()[0][:2000] + "\n\nSummarise what this code does."
        for i in range(n):
            t = time.monotonic()
            r = post(port, "/completion", {"prompt": prompt, "n_predict": 128, "temperature": 0, "cache_prompt": False})
            tm = r["timings"]
            print(f"chat pp {tm['prompt_n']} tok {tm['prompt_per_second']:.0f} t/s, tg {tm['predicted_n']} tok {tm['predicted_per_second']:.1f} t/s, wall {time.monotonic() - t:.2f} s")


if __name__ == "__main__":
    main()
