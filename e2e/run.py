"""End-to-end run of infermux-ui against a real InferMux daemon, in a browser.

Everything is isolated: a temp git repository holds the signed config's copy
and the warden file, the daemon's state and the config key sit beside it, the
models are e2e/fake_server.py, and the daemon and the UI listen on 5101 and
5110. Neither the GPU's models nor the user's configuration are touched. The
warden measures the real card and CPU, so its thresholds are set to 100% to
keep its own verdict at resume. The Config tab's editor is config.py's.

See CLAUDE.md for the command. Exits 1 if any check failed.
"""

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

from playwright.sync_api import sync_playwright

REPO = Path(__file__).resolve().parent.parent
BIN = Path(os.environ.get("INFERMUX_BIN", REPO / "result" / "bin"))
DAEMON, UI = "http://127.0.0.1:5101", "http://127.0.0.1:5110"
# The clients' keys (0006): my-key is the user's, ui-key the UI's own.
PASSPHRASE = "e2e passphrase"
KEYS = {"batch-key": ("batch", None), "my-key": ("interactive", None), "ui-key": ("interactive", None), "narrow-key": ("interactive", ["e2e/beta"])}
OUT = Path(os.environ.get("E2E_OUT", "/tmp/infermux-e2e"))

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else f": {detail}"), flush=True)
    return ok


def wait_for(fn, timeout=10.0, step=0.1):
    end = time.time() + timeout
    while time.time() < end:
        try:
            value = fn()
            if value:
                return value
        except Exception:
            pass
        time.sleep(step)
    return None


def http(method, url, body=None, headers=None):
    """A request as the user's own client: my-key unless headers name another."""
    req = urllib.request.Request(url, method=method, data=None if body is None else json.dumps(body).encode())
    req.add_header("Content-Type", "application/json")
    headers = {"Authorization": "Bearer my-key", **(headers or {})}
    for k, v in headers.items():
        if v is None:
            continue
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def verdict():
    return json.loads(http("GET", DAEMON + "/warden/verdict")[1])


class Stream(threading.Thread):
    """A streaming chat completion, read to the end in the background."""

    def __init__(self, model, tokens, key="my-key"):
        super().__init__(daemon=True)
        self.model, self.tokens, self.key = model, tokens, key
        self.chunks, self.done, self.status, self.ended = 0, False, None, None
        self.first = threading.Event()

    def run(self):
        body = {"model": self.model, "stream": True, "max_tokens": self.tokens, "messages": [{"role": "user", "content": "hi"}]}
        req = urllib.request.Request(DAEMON + "/v1/chat/completions", data=json.dumps(body).encode(), method="POST")
        req.add_header("Content-Type", "application/json")
        if self.key:
            req.add_header("Authorization", "Bearer " + self.key)
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                self.status = resp.status
                for line in resp:
                    if line.startswith(b"data: "):
                        if line.strip() == b"data: [DONE]":
                            self.done = True
                        elif b'"content"' in line:  # a token; the last chunk is the timings
                            self.chunks += 1
                            self.first.set()
        except urllib.error.HTTPError as e:
            self.status = e.code
        except Exception:
            pass
        self.ended = time.time()
        self.first.set()


def tool(*args):
    """infermux-config, with the config key's passphrase."""
    env = dict(os.environ, INFERMUX_CONFIG_PASSPHRASE=PASSPHRASE)
    return subprocess.run([BIN / "infermux-config", *map(str, args)], check=True, env=env, capture_output=True, text=True).stdout


def setup(root: Path):
    gguf = root / "gguf"
    gguf.mkdir()
    for name in ("alpha", "beta", "unused"):
        (gguf / f"{name}.gguf").write_bytes(b"GGUF")
    # The config key and the trusted key, outside the repository as the
    # user's age identity is.
    key = root / "config-key.age"
    (root / "trusted.pub").write_text(tool("keygen", "-out", key))
    cfg = root / "cfg"
    signed = cfg / "infermux"
    (signed / "models").mkdir(parents=True)
    (signed / "infermux.yaml").write_text(
        f"""# The e2e one config.
hosts:
  e2e:
    url: {DAEMON}
    devices:
      gpu: {{kind: gpu}}
balance:
  alpha: [e2e/gpu]
  beta: [e2e/gpu]
"""
    )
    (signed / "models" / "alpha.yaml").write_text(
        f"""# Alpha, a fake model.
cmd: |
  ${{fake-server}}
    --port ${{PORT}}
    --model {gguf}/alpha.gguf
    --ctx-size 4096
proxy: http://127.0.0.1:${{PORT}}
"""
    )
    (signed / "models" / "beta.yaml").write_text(
        f"""cmd: |
  ${{fake-server}}
    --port ${{PORT}}
    --model {gguf}/beta.gguf
proxy: http://127.0.0.1:${{PORT}}
"""
    )
    tool("sign", "-key", key, "-host", "e2e", signed)
    (cfg / "warden.yaml").write_text(
        """# The e2e warden.
host: e2e
keys_file: keys.yaml
policy:
  gpu_busy_percent: 100
  min_free_vram_mb: 0
  poll_seconds: 1
cpu:
  busy_percent: 100
  min_available_ram_mb: 0
"""
    )
    base = root / "base.yaml"
    base.write_text(
        f"""macros:
  fake-server: {sys.executable} {REPO / 'e2e' / 'fake_server.py'}
healthCheckTimeout: 30
logToStdout: proxy
# Away from the host's own InferMux, whose model servers start at 5800.
startPort: 15800
"""
    )
    lines = ["# The e2e clients.", "keys:"]
    for name, (cls, allow) in KEYS.items():
        lines += [f"  {name.removesuffix('-key')}:", f"    sha256: {hashlib.sha256(name.encode()).hexdigest()}", f"    class: {cls}"]
        if allow is not None:
            lines.append(f"    allow: {json.dumps(allow)}")
    (cfg / "keys.yaml").write_text("\n".join(lines) + "\n")
    # The Keys tab's sops file, encrypted to an age key outside the repo, and
    # that key encrypted with a passphrase, as the user's is.
    plain = root / "age-plain.txt"
    public = subprocess.run(["age-keygen", "-o", plain], capture_output=True, text=True, check=True).stderr.split()[-1]
    identity = root / "age.txt"
    age_passphrase(plain, identity)
    plain.unlink()
    (cfg / ".sops.yaml").write_text(f"creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age: {public}\n")
    (cfg / "secrets").mkdir()
    kv = root / "kv.json"
    kv.write_text(json.dumps({"fake-server": ["q8_0-q8_0", "f16-f16"]}))
    git = ["git", "-C", str(cfg), "-c", "user.name=e2e", "-c", "user.email=e2e@example"]
    subprocess.run(["git", "init", "-q", str(cfg)], check=True)
    subprocess.run(git + ["add", "-A"], check=True)
    subprocess.run(git + ["commit", "-qm", "start"], check=True)
    return cfg, signed, base, kv, gguf, identity, key


def age_passphrase(src, dst):
    """`age -p` reads the passphrase only from a terminal, so it gets a pty."""
    import pty
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp("age", ["age", "-p", "-o", str(dst), str(src)])
    out = b""
    while True:
        try:
            chunk = os.read(fd, 1024)
        except OSError:
            break
        if not chunk:
            break
        out += chunk
        if chunk.rstrip().endswith(b":"):
            os.write(fd, PASSPHRASE.encode() + b"\n")
    _, status = os.waitpid(pid, 0)
    assert os.waitstatus_to_exitcode(status) == 0, out


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="infermux-e2e-"))
    cfg, signed, base, kv, gguf, identity, key = setup(root)
    for d in ("state", "derived"):
        (root / d).mkdir()
    (root / "ui-key").write_text("ui-key\n")
    env = dict(os.environ, GIT_AUTHOR_NAME="e2e", GIT_AUTHOR_EMAIL="e2e@example", GIT_COMMITTER_NAME="e2e", GIT_COMMITTER_EMAIL="e2e@example")
    daemon_log = open(OUT / "daemon.log", "w")
    daemon = subprocess.Popen(
        [BIN / "infermux", "-listen", "127.0.0.1:5101", "-config", base, "-config-dir", root / "derived", "-warden-config", cfg / "warden.yaml",
         "-config-trusted-keys", root / "trusted.pub", "-config-state", root / "state", "-config-seed", signed, "-off-file", root / "off"],
        stdout=daemon_log, stderr=subprocess.STDOUT, env=env,
    )
    ui = subprocess.Popen(
        [BIN / "infermux-ui", "-listen", "127.0.0.1:5110", "-swap-listen", "127.0.0.1:5111", "-daemon", DAEMON,
         "-signed-config", signed, "-config-key", key, "-warden-config", cfg / "warden.yaml", "-base-config", base, "-gguf-dirs", gguf, "-kv-kernels", kv,
         "-daemon-key-file", root / "ui-key", "-key-secrets", cfg / "secrets" / "infermux.yaml",
         "-age-identity", identity],
        stdout=open(OUT / "ui.log", "w"), stderr=subprocess.STDOUT, env=env,
    )
    try:
        if not wait_for(lambda: http("GET", DAEMON + "/warden/verdict")[0] == 200 and http("GET", UI + "/api/state")[0] == 200, 20):
            print("the stack did not come up; see", OUT)
            return 1
        run(cfg, signed, root / "off")
    finally:
        ui.terminate()
        daemon.terminate()
        for p in (ui, daemon):
            try:
                p.wait(10)
            except subprocess.TimeoutExpired:
                p.kill()
        shutil.rmtree(root, ignore_errors=True)
    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)} passed, {len(failed)} failed. Screenshots and logs in {OUT}")
    for name, _, detail in failed:
        print(f"  FAIL {name}: {detail}")
    return 1 if failed else 0


def reloads():
    return (OUT / "daemon.log").read_text().count("configuration reloaded")


def git(cfg, *args):
    return subprocess.run(["git", "-C", str(cfg), *args], capture_output=True, text=True).stdout


def config_state():
    return json.loads(http("GET", DAEMON + "/warden/config/state")[1])


def run(cfg, signed, off):
    with sync_playwright() as pw:
        browser = pw.chromium.launch()
        # No key: llama-swap's UI comes through infermux-ui's second listener,
        # which adds the UI's, so a Basic prompt would fail the tab's check.
        page = browser.new_page(viewport={"width": 2560, "height": 1300}, color_scheme="dark")
        errors = []
        page.on("pageerror", lambda e: errors.append(f"pageerror: {e}"))
        page.on("console", lambda m: m.type == "error" and errors.append(f"console: {m.text}"))
        page.on("dialog", lambda d: d.accept())

        def visible(locator, timeout=5):
            """Waits for the locator to show; is_visible() does not wait."""
            try:
                locator.first.wait_for(state="visible", timeout=timeout * 1000)
                return True
            except Exception:
                return False

        def shot(name):
            page.screenshot(path=str(OUT / f"{name}.png"))

        def open_tab(tab):
            # A fresh load: going to the same #hash would not leave an open editor.
            page.goto(f"{UI}/?t={time.time()}#{tab}")
            page.wait_for_load_state("networkidle")

        def publish(edit):
            """A Publish from this UI, as the Config tab sends it: the running
            version's files, edited, signed with the config key."""
            files = {"infermux.yaml": (signed / "infermux.yaml").read_text()}
            files.update({"models/" + f.name: f.read_text() for f in (signed / "models").iterdir()})
            edit(files)
            body = {"files": files, "base": config_state()["current"]["hash"], "passphrase": PASSPHRASE, "force": False}
            return page.evaluate(
                """body => fetch('/api/config/publish', {method: 'POST', headers: {'X-InferMux': '1', 'Content-Type': 'application/json'},
                    body: JSON.stringify(body)}).then(async r => [r.status, await r.text()])""",
                body,
            )

        # --- Keys at the daemon ---------------------------------------------------
        chat = {"model": "alpha", "max_tokens": 1, "messages": []}
        status, _ = http("POST", DAEMON + "/v1/chat/completions", chat, {"Authorization": None})
        check("no key gets 401", status == 401, status)
        status, _ = http("GET", DAEMON + "/warden/verdict", None, {"Authorization": "Bearer guess"})
        check("an unknown key gets 401, the warden's API too", status == 401, status)
        status, _ = http("GET", DAEMON + "/health", None, {"Authorization": None})
        check("the health check needs no key", status == 200, status)
        status, body = http("POST", DAEMON + "/v1/chat/completions", chat, {"Authorization": "Bearer narrow-key"})
        check("a model outside the key's allow list gets 403", status == 403 and "e2e/alpha" in body, f"{status} {body[:100]}")
        status, body = http("POST", DAEMON + "/v1/chat/completions", dict(chat, model="beta"), {"x-api-key": "narrow-key", "Authorization": None})
        check("one inside it is served, the key read from x-api-key", status == 200, f"{status} {body[:100]}")

        # --- Status -----------------------------------------------------------
        open_tab("status")
        page.get_by_text("RESUME", exact=True).wait_for(timeout=10000)
        check("status shows the verdict", visible(page.get_by_text("RESUME", exact=True)))
        check("status lists GPU processes", visible(page.get_by_role("columnheader", name="Process")))
        shot("status")

        # --- A safety stop: the local models off (0025) ------------------------
        off.write_text("too hot: e2e\n")
        page.get_by_text("Local models off").wait_for(timeout=10000)
        check("status shows the local models off with the file's text", visible(page.get_by_text("too hot: e2e")))
        status, body = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 1, "messages": []})
        check("a local request is refused while they are off", status == 503, f"{status} {body[:200]}")
        shot("status-off")
        off.unlink()
        page.get_by_text("Local models off").wait_for(state="hidden", timeout=10000)
        check("the notice goes with the file", not visible(page.get_by_text("Local models off")))

        # --- The signed config runs ---------------------------------------------
        st = config_state()
        check("the daemon runs the signed config from the seed", st["current"] and st["current"]["revision"] == 1, st)
        status, body = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 1, "messages": []})
        check("its model starts and answers", status == 200, f"{status} {body[:200]}")
        running = json.loads(http("GET", DAEMON + "/running")[1])["running"]
        check("on the GPU device it was placed on", running and running[0]["model"] == "alpha", running)

        # --- Performance: the request just made, and a streamed one ------------
        status, _ = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 3, "stream": True, "messages": []})
        recorded = json.loads(http("GET", DAEMON + "/warden/requests")[1])["hosts"][0]
        alpha = [r for r in recorded["requests"] if r["model"] == "e2e/alpha"]
        streamed = alpha[0]
        check("a streamed request is recorded with its key and first token",
              status == 200 and streamed["model"] == "e2e/alpha" and streamed["client"] == "my" and streamed["stream"] and streamed["ttft_ms"] is not None, streamed)
        check("llama-server's timings come through llama-swap",
              streamed["prefill_ms"] == 30 and streamed["prompt_tokens"] == 16 and streamed["cached_tokens"] == 4 and streamed["decode_per_second"] == 10, streamed)
        check("the request that was not streamed has its timings and no first token",
              len(alpha) == 2 and alpha[1]["ttft_ms"] is None and alpha[1]["prefill_ms"] == 30, recorded["requests"])
        open_tab("performance")
        check("the performance tab summarises the model", visible(page.locator("tr", has_text="e2e/alpha").first))
        check("and lists every request, beta's too", page.locator("tr", has_text="/v1/chat/completions").count() == 3)
        alpha_row = page.locator("tr", has_text="e2e/alpha").first
        check("the summary has the median decode rate", "10.0" in alpha_row.inner_text(), alpha_row.inner_text())
        shot("performance")

        # --- Active: a reply read while the model is still writing it (0031) ----
        open_tab("active")
        check("the active tab follows the daemon", visible(page.get_by_text("following"), 5))
        writing = Stream("alpha", 40)
        writing.start()
        reply_pane = page.locator("section", has_text="Reply ·").last
        check("the new request is shown with its key and messages",
              visible(page.locator("button", has_text="e2e/alpha").first, 10) and visible(page.locator("section", has_text="Request ·").get_by_text("hi", exact=True), 5))
        check("its reply shows tokens while it streams",
              visible(reply_pane.get_by_text(re.compile(r"w0 w1 ")), 10) and visible(page.get_by_text("streaming"), 2) and writing.ended is None,
              reply_pane.inner_text() if reply_pane.count() else "no reply pane")
        shot("active-streaming")
        writing.join(30)
        check("and ends with the whole reply and its status",
              visible(reply_pane.get_by_text(re.compile(r"w39 ")), 10) and visible(page.locator("button", has_text="e2e/alpha").first.get_by_text("200"), 5),
              reply_pane.inner_text())
        shot("active-done")

        # --- The reload waits for the user's reply ------------------------------------
        reply = Stream("alpha", 60)
        reply.start()
        check("an interactive reply starts", reply.first.wait(30) and reply.chunks > 0, reply.status)
        before = reloads()
        open_tab("status")
        status, body = publish(lambda f: f.update({"models/alpha.yaml": f["models/alpha.yaml"].replace("--ctx-size 4096", "--ctx-size 8192")}))
        check("a Publish mid-reply is accepted", status == 200 and config_state()["current"]["revision"] == 2, f"{status} {body[:200]}")
        waiting = page.get_by_text("Waiting for your requests to finish")
        check("status shows the reload waiting", visible(waiting, 5), verdict().get("waiting_for_quiet"))
        shot("status-waiting")
        check("no reload while the reply runs", reloads() == before and not reply.done)
        reply.join(60)
        check("the reply finished whole", reply.done and reply.chunks == 60, f"{reply.chunks} chunks, done {reply.done}")
        check("the reload ran after it", wait_for(lambda: reloads() > before, 10))
        status, _ = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 1, "messages": []})
        running = json.loads(http("GET", DAEMON + "/running")[1])["running"]
        check("the daemon runs the published flag", status == 200 and running and re.search(r"--ctx-size\s+8192", running[0]["cmd"]), running)
        check("nixcfg's copy is the published version", "--ctx-size 8192" in (signed / "models" / "alpha.yaml").read_text())

        # --- Manual verdict, and what batch gets ------------------------------------
        page.get_by_role("button", name="Pause", exact=True).click()
        check("pause by hand", visible(page.get_by_text("pause (by hand)")))
        status, body = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 1, "messages": []}, {"Authorization": "Bearer batch-key"})
        check("a batch request gets 503 while paused", status == 503, f"{status} {body[:100]}")
        status, _ = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 1, "messages": []})
        check("an interactive request gets 200 while paused", status == 200, status)
        shot("status-paused")
        page.get_by_role("button", name="Automatic").click()
        check("automatic hands back", visible(page.get_by_text("resume", exact=True)))

        # --- Cancel batch, unload ---------------------------------------------------------
        batch, mine = Stream("alpha", 100, key="batch-key"), Stream("alpha", 30)
        batch.start()
        mine.start()
        batch.first.wait(30)
        mine.first.wait(30)
        page.get_by_role("button", name="Cancel batch requests").click()
        batch.join(10)
        check("cancel batch ends the batch stream early", batch.ended and not batch.done and batch.chunks < 100, f"{batch.chunks} chunks")
        # The user's click always wins, and cuts the reply on the GPU (0024, 4).
        page.get_by_role("button", name="Unload all").click()
        mine.join(10)
        check("unload all cuts the interactive stream mid-reply", mine.ended and not mine.done and mine.chunks < 30, f"{mine.chunks} chunks")
        check("and stops the model", wait_for(lambda: json.loads(http("GET", DAEMON + "/running")[1])["running"] == [], 10))

        # --- A job is paused mid-reply and goes on where it stopped (0023) ------------
        jobs_headers = {"Authorization": "Bearer batch-key", "X-InferMux": "1"}
        status, body = http("POST", DAEMON + "/warden/jobs", {"path": "/v1/chat/completions", "body": {"model": "alpha", "max_tokens": 40, "messages": [{"role": "user", "content": "count"}]}}, jobs_headers)
        check("a batch client hands in a job", status == 201, f"{status} {body[:200]}")
        job_id = json.loads(body)["id"] if status == 201 else ""

        def job():
            return json.loads(http("GET", DAEMON + "/warden/jobs/" + job_id, None, jobs_headers)[1])

        check("the job runs", wait_for(lambda: job()["state"] == "running" and job()["last_seq"] >= 8, 30), job())
        open_tab("status")
        page.get_by_role("button", name="Pause", exact=True).click()
        check("pause by hand pauses the job", wait_for(lambda: job()["state"] == "paused", 10), job())
        check("status shows the paused job and why", visible(page.get_by_text("Jobs: 1 paused")) and visible(page.get_by_text("Paused: batch requests wait while the GPU is yielded")), verdict().get("jobs"))
        shot("status-job-paused")
        page.get_by_role("button", name="Automatic").click()
        check("the job finishes after the resume", wait_for(lambda: job()["state"] == "done", 30), job())
        status, events = http("GET", DAEMON + "/warden/jobs/" + job_id + "/events", None, jobs_headers)
        heard = "".join(c.get("delta", {}).get("content") or "" for line in events.splitlines() if line.startswith("data: ")
                        for c in json.loads(line[6:]).get("choices", []))
        check("the client heard one reply, each word once", heard == "".join(f"w{i} " for i in range(40)), heard)
        check("in two attempts", job()["attempts"] >= 2, job())

        # --- Warden settings ----------------------------------------------------------
        open_tab("settings")
        busy_input = page.locator("label", has_text="Busy at, % foreign GPU").locator("input")
        busy_input.fill("99")
        cpu_card = page.locator(".card", has=page.locator("h2:text-is('CPU')"))
        cpu_card.locator("label", has_text="Busy at").locator("input").fill("90")
        cpu_card.locator("label", has_text="Priority processes").locator("textarea").fill("blender")
        page.locator("label", has_text="Trusted hosts").locator("textarea").fill("box.example.ts.net")
        page.get_by_role("button", name="Save", exact=True).click()
        check("warden settings save", visible(page.get_by_text("Saved.")))
        wtext = (cfg / "warden.yaml").read_text()
        check("the warden file has them, comment kept", "gpu_busy_percent: 99" in wtext and "box.example.ts.net" in wtext and wtext.startswith("# The e2e warden."), wtext)
        check("the daemon applies them without a restart", wait_for(lambda: verdict()["policy"]["gpu_busy_percent"] == 99, 10))
        check("the CPU's numbers save too", re.search(r"^cpu:\n(\s+.*\n)*?\s+busy_percent: 90\n", wtext, re.M) is not None and re.search(r"^\s+- blender$", wtext, re.M) is not None, wtext)
        page.locator("label", has_text="Poll every").locator("input").fill("0")
        page.get_by_role("button", name="Save", exact=True).click()
        check("a policy number no one could mean is refused", visible(page.locator(".card.text-red-300", has_text="poll_seconds")))
        check("and the file keeps the last good one", (cfg / "warden.yaml").read_text() == wtext)
        shot("settings")

        # --- Keys -------------------------------------------------------------------
        open_tab("keys")
        check("keys lists the file's keys", visible(page.locator("td", has_text="narrow")))
        page.get_by_placeholder("episteme-batch").fill("phone")
        page.get_by_role("button", name="Make key").click()
        made = page.locator("code.select-all").first
        check("a new key is shown once made", visible(made))
        key = made.inner_text().strip() if made.count() else ""
        ktext = (cfg / "keys.yaml").read_text()
        secrets = (cfg / "secrets" / "infermux.yaml").read_text()
        check("keys.yaml has its hash, not the key, comment kept",
              hashlib.sha256(key.encode()).hexdigest() in ktext and key not in ktext and ktext.startswith("# The e2e clients."), ktext)
        check("the sops file has it encrypted", key and key not in secrets and "sops:" in secrets, secrets[:200])
        check("the daemon accepts it without a restart",
              wait_for(lambda: http("POST", DAEMON + "/v1/chat/completions", chat, {"Authorization": "Bearer " + key})[0] == 200, 10))
        shot("keys-made")
        page.get_by_role("button", name="Done").click()
        phone = page.locator("tr", has_text="phone")
        phone.get_by_role("button", name="Show").click()
        check("show without the passphrase asks for it", visible(page.get_by_text("passphrase of the age identity")))
        page.locator("label", has_text="Passphrase").locator("input").fill("wrong")
        phone.get_by_role("button", name="Show").click()
        check("a wrong passphrase does not open it", visible(page.get_by_text("passphrase does not open")))
        page.locator("label", has_text="Passphrase").locator("input").fill(PASSPHRASE)
        phone.get_by_role("button", name="Show").click()
        check("show reads it back from sops", visible(phone.get_by_text(key)) if key else False)
        phone.get_by_role("button", name="Revoke").click()
        check("revoke takes it out of keys.yaml", wait_for(lambda: "phone:" not in (cfg / "keys.yaml").read_text(), 5))
        check("and the daemon refuses it",
              wait_for(lambda: http("POST", DAEMON + "/v1/chat/completions", chat, {"Authorization": "Bearer " + key})[0] == 401, 10))
        shot("keys")

        # --- Changes ---------------------------------------------------------------
        open_tab("changes")
        check("changes lists the edited files", visible(page.get_by_text("infermux/models/alpha.yaml")) and visible(page.get_by_text("warden.yaml")))
        commits = git(cfg, "rev-list", "--count", "HEAD").strip()
        page.get_by_placeholder("Commit message").fill("e2e edits")
        page.get_by_role("button", name="Commit").click()
        check("commit reports its hash", visible(page.get_by_text("Nothing was pushed."), 10))
        check("one commit was made", git(cfg, "rev-list", "--count", "HEAD").strip() == str(int(commits) + 1))
        check("nothing of ours is left uncommitted", git(cfg, "status", "--porcelain").strip() == "", git(cfg, "status", "--porcelain"))
        check("the commit has the message", git(cfg, "log", "-1", "--format=%s").strip() == "e2e edits")
        shot("changes")

        # --- llama-swap's own UI --------------------------------------------------------
        open_tab("llama-swap")
        frame = page.frame_locator('iframe[title="llama-swap"]')
        check("the llama-swap tab shows its UI without a key", visible(frame.get_by_text("Activity"), 15))
        check("the tab frames infermux-ui's llama-swap listener", ":5111/ui/" in (page.locator('iframe[title="llama-swap"]').get_attribute("src") or ""))
        check("its model list has ours", visible(frame.get_by_text(re.compile("^alph")), 5))
        shot("llama-swap")
        open_tab("status")
        page.evaluate("location.hash = '#llama-swap'")
        check("the frame is kept across tabs", page.locator('iframe[title="llama-swap"]').count() == 1)
        check("there is no header link any more", page.get_by_role("link", name=re.compile("llama-swap ↗")).count() == 0)

        # --- The guard, from a real browser ---------------------------------------------
        open_tab("status")
        code = page.evaluate("fetch('/api/warden', {method: 'PUT', body: '{}'}).then(r => r.status)")
        check("a same-origin write without the header is refused", code == 403, code)
        page.goto(DAEMON + "/ui/")
        outcome = page.evaluate(
            f"""fetch('{UI}/api/warden', {{method: 'PUT', headers: {{'X-InferMux': '1'}}, body: '{{}}'}})
                .then(r => 'status ' + r.status, e => 'blocked: ' + e.message)"""
        )
        check("another origin cannot send the header (preflight refused)", outcome.startswith("blocked"), outcome)
        outcome = page.evaluate(
            f"""fetch('{UI}/daemon/warden/manual', {{method: 'POST', mode: 'no-cors', body: '{{"action":"pause"}}'}})
                .then(r => 'sent', e => 'blocked: ' + e.message)"""
        )
        page.wait_for_timeout(500)
        check("a simple cross-origin POST does not pause", verdict()["manual"] is None, f"{outcome}, manual {verdict()['manual']}")

        noise = [e for e in errors if "net::ERR_FAILED" not in e and "CORS" not in e and "Failed to load resource" not in e]
        check("no errors in the console", not noise, noise[:5])
        browser.close()


if __name__ == "__main__":
    sys.exit(main())
