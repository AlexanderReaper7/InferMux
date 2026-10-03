"""End-to-end run of infermux-ui against a real InferMux daemon, in a browser.

Everything is isolated: a temp git repository holds the models and the warden
file, the models are e2e/fake_server.py, and the daemon and the UI listen on
5101 and 5110. Neither the GPU's models nor the user's configuration are
touched. The warden measures the real card, so its threshold is set to 100%
to keep its own verdict at resume.

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
                        else:
                            self.chunks += 1
                            self.first.set()
        except urllib.error.HTTPError as e:
            self.status = e.code
        except Exception:
            pass
        self.ended = time.time()
        self.first.set()


def setup(root: Path):
    gguf = root / "gguf"
    gguf.mkdir()
    for name in ("alpha", "beta", "unused"):
        (gguf / f"{name}.gguf").write_bytes(b"GGUF")
    cfg = root / "cfg"
    (cfg / "models").mkdir(parents=True)
    (cfg / "models" / "alpha.yaml").write_text(
        f"""# Alpha, the table-form model.
models:
  alpha:
    cmd: |
      ${{fake-server}}
        --port ${{PORT}}
        --model {gguf}/alpha.gguf
        --ctx-size 4096
        --cache-type-k q8_0
        --cache-type-v q8_0
    proxy: http://127.0.0.1:${{PORT}}
    env:
      - KEEP=me # a key the UI does not edit
"""
    )
    (cfg / "models" / "beta.yaml").write_text(
        f"""models:
  beta:
    cmd: |
      ${{fake-server}}
        # a comment makes it raw text
        --port ${{PORT}}
        --model {gguf}/beta.gguf
    proxy: http://127.0.0.1:${{PORT}}
"""
    )
    (cfg / "warden.yaml").write_text(
        """# The e2e warden.
host: e2e
keys_file: keys.yaml
policy:
  gpu_busy_percent: 100
  min_free_vram_mb: 0
  poll_seconds: 1
"""
    )
    base = root / "base.yaml"
    base.write_text(
        f"""macros:
  fake-server: {sys.executable} {REPO / 'e2e' / 'fake_server.py'}
healthCheckTimeout: 30
logToStdout: proxy
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
    return cfg, base, kv, gguf, identity


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
    cfg, base, kv, gguf, identity = setup(root)
    models = cfg / "models"
    (root / "ui-key").write_text("ui-key\n")
    env = dict(os.environ, GIT_AUTHOR_NAME="e2e", GIT_AUTHOR_EMAIL="e2e@example", GIT_COMMITTER_NAME="e2e", GIT_COMMITTER_EMAIL="e2e@example")
    daemon_log = open(OUT / "daemon.log", "w")
    daemon = subprocess.Popen(
        [BIN / "infermux", "-listen", "127.0.0.1:5101", "-config", base, "-config-dir", models, "-warden-config", cfg / "warden.yaml"],
        stdout=daemon_log, stderr=subprocess.STDOUT, env=env,
    )
    ui = subprocess.Popen(
        [BIN / "infermux-ui", "-listen", "127.0.0.1:5110", "-daemon", DAEMON, "-models-dir", models,
         "-warden-config", cfg / "warden.yaml", "-base-config", base, "-gguf-dirs", gguf, "-kv-kernels", kv,
         "-daemon-key-file", root / "ui-key", "-key-secrets", cfg / "secrets" / "infermux.yaml",
         "-age-identity", identity],
        stdout=open(OUT / "ui.log", "w"), stderr=subprocess.STDOUT, env=env,
    )
    try:
        if not wait_for(lambda: http("GET", DAEMON + "/warden/verdict")[0] == 200 and http("GET", UI + "/api/state")[0] == 200, 20):
            print("the stack did not come up; see", OUT)
            return 1
        run(cfg, models, gguf)
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


def run(cfg, models, gguf):
    with sync_playwright() as pw:
        browser = pw.chromium.launch()
        # The browser's key for llama-swap's UI, given at the Basic prompt.
        page = browser.new_page(viewport={"width": 2560, "height": 1300}, color_scheme="dark", http_credentials={"username": "me", "password": "my-key"})
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

        def flag_row(name):
            """The flag table row whose name input holds name."""
            index = page.evaluate(
                """name => [...document.querySelectorAll('tbody tr')].findIndex(
                    tr => tr.querySelector('input')?.value === name)""",
                name,
            )
            return page.locator("tbody tr").nth(index) if index >= 0 else None

        def edit(model):
            open_tab("models")
            page.locator("tr", has_text=model).first.click()
            page.get_by_role("heading", name=f"Edit {model}").wait_for()

        def save():
            page.get_by_role("button", name="Save", exact=True).click()

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

        # --- Models: the list ---------------------------------------------------
        open_tab("models")
        alpha_row = page.locator("tr", has_text="alpha").first
        alpha_row.wait_for()
        check("models lists the table-form model with its runtime", "fake-server" in alpha_row.inner_text())
        check("models lists the raw model as text", "text" in page.locator("tr", has_text="beta").first.inner_text())
        check("the KV column shows the pair", "q8_0-q8_0" in alpha_row.inner_text())
        check("an unused GGUF is offered", visible(page.get_by_text(f"{gguf}/unused.gguf")))
        shot("models")

        # --- Edit a flag, the file and the daemon follow ----------------------
        edit("alpha")
        check("the file's comment is shown", visible(page.get_by_text("Alpha, the table-form model.")))
        before = reloads()
        flag_row("--ctx-size").locator("input").nth(1).fill("8192")
        save()
        text = wait_for(lambda: "--ctx-size 8192" in (models / "alpha.yaml").read_text() and (models / "alpha.yaml").read_text())
        check("a flag edit lands in the file", text, (models / "alpha.yaml").read_text())
        check("keys the UI does not edit survive", text and "KEEP=me # a key the UI does not edit" in text, text)
        check("the comment survives", text and text.startswith("# Alpha, the table-form model."), text)
        check("the daemon reloads after a save", wait_for(lambda: reloads() > before, 10), f"{reloads()} reloads")

        status, body = http("POST", DAEMON + "/v1/chat/completions", {"model": "alpha", "max_tokens": 1, "messages": []})
        check("the edited model starts and answers", status == 200, f"{status} {body[:200]}")
        running = json.loads(http("GET", DAEMON + "/running")[1])["running"]
        check("the daemon runs it with the new flag", running and re.search(r"--ctx-size\s+8192", running[0]["cmd"]), running)

        # --- Add a flag, a switch, and remove one --------------------------------
        edit("alpha")
        page.get_by_role("button", name="Add flag").click()
        last = page.locator("tbody tr").last
        last.locator("input").nth(0).fill("--jinja")
        last.locator('input[type="checkbox"]').check()
        flag_row("--cache-type-v").get_by_title("Remove").click()
        save()
        text = wait_for(lambda: "--jinja" in (models / "alpha.yaml").read_text() and (models / "alpha.yaml").read_text())
        check("an added switch is written without a value", text and re.search(r"--jinja\n", text), text)
        check("a removed flag is gone", text and "--cache-type-v" not in text, text)

        # --- The KV warning ----------------------------------------------------
        edit("alpha")
        # --cache-type-v was removed above, so the pair is now q8_0-f16.
        check("the editor warns about q8_0-f16, which has no kernel", visible(page.get_by_text("has no FlashAttention kernel for q8_0-f16")))
        flag_row("--cache-type-k").locator("input").nth(1).fill("f16")
        check("the warning clears as the pair becomes f16-f16", wait_for(lambda: not page.get_by_text("has no FlashAttention kernel").is_visible(), 3))
        flag_row("--cache-type-k").locator("input").nth(1).fill("q4_1")
        check("the warning comes back for q4_1-f16", visible(page.get_by_text("has no FlashAttention kernel for q4_1-f16")))
        shot("editor-kv-warning")
        save()
        open_tab("models")
        check("the list marks the model", visible(page.get_by_text("q4_1-f16 ⚠")))
        check("the banner names the model", visible(page.locator(".card", has_text="no FlashAttention kernel for q4_1-f16")))
        check("no build button without -prebuild", page.get_by_role("button", name="Build now").count() == 0)

        # --- Refusals leave the file alone ------------------------------------------
        edit("alpha")
        before_text = (models / "alpha.yaml").read_text()
        page.locator("label", has_text="Name, the model ID").locator("input").fill("has space")
        save()
        err = page.locator(".border-red-900").first
        err.wait_for(timeout=5000)
        check("a bad name shows the refusal", "a model name is" in err.inner_text(), err.inner_text())
        page.locator("label", has_text="Name, the model ID").locator("input").fill("alpha")
        page.get_by_role("button", name="Add flag").click()
        last = page.locator("tbody tr").last
        last.locator("input").nth(0).fill("--prompt")
        last.locator("input").nth(1).fill("--looks-like-a-flag")
        save()
        refusal = page.locator(".border-red-900", has_text="would read as a flag")
        check("a value that would read as a flag is refused", visible(refusal), page.locator(".border-red-900").all_inner_texts())
        check("refused saves leave the file alone", (models / "alpha.yaml").read_text() == before_text)

        # --- Raw text --------------------------------------------------------------
        edit("beta")
        check("a commented cmd opens as text", visible(page.locator("textarea")))
        area = page.locator("textarea").first
        area.fill(area.input_value().replace("--model", "--threads 2\n  --model"))
        save()
        check("a raw edit lands", wait_for(lambda: "--threads 2" in (models / "beta.yaml").read_text()), (models / "beta.yaml").read_text())
        check("a raw edit keeps its comment", "# a comment makes it raw text" in (models / "beta.yaml").read_text())

        # --- New model from an unused GGUF, rename, delete -----------------------------
        open_tab("models")
        page.locator("div", has_text=f"{gguf}/unused.gguf").get_by_role("button", name="Add as a model").last.click()
        page.get_by_role("heading", name="New model").wait_for()
        check("the new model is named after its file", page.locator("label", has_text="Name, the model ID").locator("input").input_value() == "unused")
        check("the new model copies flags from one on its runtime", page.locator("tbody tr").count() > 0)
        save()
        check("a new model gets its own file", wait_for(lambda: (models / "unused.yaml").exists()))
        edit("unused")
        page.locator("label", has_text="Name, the model ID").locator("input").fill("gamma")
        save()
        check("a rename moves the file", wait_for(lambda: (models / "gamma.yaml").exists() and not (models / "unused.yaml").exists()))
        edit("gamma")
        page.get_by_role("button", name="Delete").click()
        check("delete removes the file", wait_for(lambda: not (models / "gamma.yaml").exists()))
        open_tab("models")
        check("the list no longer has it", page.locator("tr", has_text="gamma").count() == 0)

        # --- The reload waits for the user's reply ------------------------------------
        reply = Stream("alpha", 60)
        reply.start()
        check("an interactive reply starts", reply.first.wait(30) and reply.chunks > 0, reply.status)
        before = reloads()
        edit("alpha")
        page.locator("label", has_text="Description").locator("input").fill("edited mid-reply")
        save()
        open_tab("status")
        waiting = page.get_by_text("Waiting for your requests to finish")
        check("status shows the reload waiting", visible(waiting, 5), verdict().get("waiting_for_quiet"))
        shot("status-waiting")
        check("no reload while the reply runs", reloads() == before and not reply.done)
        reply.join(60)
        check("the reply finished whole", reply.done and reply.chunks == 60, f"{reply.chunks} chunks, done {reply.done}")
        check("the reload ran after it", wait_for(lambda: reloads() > before, 10))

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
        unload = page.get_by_role("button", name="Unload all")
        unload.click()
        check("unload all is refused while a reply runs", visible(page.locator(".border-red-900", has_text="interactive request")))
        mine.join(30)
        check("the interactive stream was untouched", mine.done and mine.chunks == 30, f"{mine.chunks} chunks")
        page.wait_for_timeout(2500)
        unload.click()
        check("unload all after it stops the model", wait_for(lambda: json.loads(http("GET", DAEMON + "/running")[1])["running"] == [], 10))

        # --- Warden settings ----------------------------------------------------------
        open_tab("settings")
        busy_input = page.locator("label", has_text="Busy at").locator("input")
        busy_input.fill("99")
        page.locator("label", has_text="Trusted hosts").locator("textarea").fill("box.example.ts.net")
        page.get_by_role("button", name="Save", exact=True).click()
        check("warden settings save", visible(page.get_by_text("Saved.")))
        wtext = (cfg / "warden.yaml").read_text()
        check("the warden file has them, comment kept", "gpu_busy_percent: 99" in wtext and "box.example.ts.net" in wtext and wtext.startswith("# The e2e warden."), wtext)
        check("the daemon applies them without a restart", wait_for(lambda: verdict()["policy"]["gpu_busy_percent"] == 99, 10))
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
        check("changes lists the edited files", visible(page.get_by_text("models/alpha.yaml")))
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
        check("the llama-swap tab shows its UI", visible(frame.get_by_text("Activity"), 15))
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
