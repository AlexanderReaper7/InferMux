"""End-to-end run of the one config (0022, 9): the Config tab against a real
daemon that runs a signed config, in a browser.

Isolated as run.py is: a temp directory holds the config key, the trusted
key, the daemon's state and nixcfg's copy, the models are e2e/fake_server.py
and nothing is started, and the daemon and the UI listen on 5121 and 5130.
A second host's Publish is infermux-config's sign with -host zbox, posted to
the daemon as the other daemon would.

See CLAUDE.md for the command. Exits 1 if any check failed.
"""

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

from playwright.sync_api import sync_playwright

from run import BIN, OUT, REPO, check, results, wait_for

DAEMON, UI = "http://127.0.0.1:5121", "http://127.0.0.1:5130"
PASSPHRASE = "e2e config passphrase"


def get(url):
    with urllib.request.urlopen(url, timeout=10) as r:
        return json.loads(r.read())


def state():
    return get(DAEMON + "/warden/config/state")


def manifest(d):
    return json.loads((d / "manifest.json").read_text())


def tool(*args):
    env = dict(os.environ, INFERMUX_CONFIG_PASSPHRASE=PASSPHRASE)
    return subprocess.run([BIN / "infermux-config", *map(str, args)], check=True, env=env, capture_output=True, text=True).stdout


def setup(root: Path):
    gguf = root / "gguf"
    gguf.mkdir()
    for name in ("alpha", "beta", "unused"):
        (gguf / f"{name}.gguf").write_bytes(f"GGUF {name}".encode())
    key = root / "config-key.age"
    (root / "trusted.pub").write_text(tool("keygen", "-out", key))
    cfg = root / "cfg"
    signed = cfg / "infermux"
    (signed / "models").mkdir(parents=True)
    (signed / "infermux.yaml").write_text(
        f"""# The e2e one config.
hosts:
  here:
    url: {DAEMON}
    devices:
      gpu: {{kind: gpu}}
      cpu: {{kind: cpu}}
balance:
  alpha: [here/gpu]
failover:
  alpha: [here/cpu]
"""
    )
    sha = hashlib.sha256(b"GGUF alpha").hexdigest()
    (signed / "models" / "alpha.yaml").write_text(
        f"""# Alpha, a fake model. It once ran with --cache-type-k q8_0.
cmd: |
  ${{fake-server}}
    --port ${{PORT}}
    --model {gguf}/alpha.gguf
    --ctx-size 4096
files:
  {gguf}/alpha.gguf: {{sha256: {sha}}}
"""
    )
    tool("sign", "-key", key, "-host", "here", signed)
    (cfg / "warden.yaml").write_text("# The e2e warden.\nhost: here\npolicy:\n  gpu_busy_percent: 100\n  min_free_vram_mb: 0\n  poll_seconds: 2\n")
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
    (root / "kv.json").write_text(json.dumps({"fake-server": ["f16-f16", "q8_0-q8_0"]}))
    git = ["git", "-C", str(cfg), "-c", "user.name=e2e", "-c", "user.email=e2e@example"]
    subprocess.run(["git", "init", "-q", str(cfg)], check=True)
    subprocess.run(git + ["add", "-A"], check=True)
    subprocess.run(git + ["commit", "-qm", "start"], check=True)
    return cfg, signed, base, gguf, key


def publish_as_zbox(signed, key, other, edit):
    """Another host's Publish: a copy of the running version, edited, signed
    and posted to this daemon as the other daemon would."""
    shutil.rmtree(other, ignore_errors=True)
    shutil.copytree(signed, other)
    edit(other)
    tool("sign", "-key", key, "-host", "zbox", other)
    names = ["infermux.yaml"] + ["models/" + x.name for x in (other / "models").iterdir()]
    wire = {"manifest": (other / "manifest.json").read_text(), "signature": (other / "manifest.sig").read_text().strip(),
            "files": {f: (other / f).read_text() for f in names}}
    req = urllib.request.Request(DAEMON + "/warden/config", data=json.dumps(wire).encode(), method="POST",
                                 headers={"X-InferMux": "e2e", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return {"error": e.code, "body": e.read().decode()}


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="infermux-e2e-config-"))
    cfg, signed, base, gguf, key = setup(root)
    for d in ("state", "derived"):
        (root / d).mkdir()
    daemon = subprocess.Popen(
        [BIN / "infermux", "-listen", "127.0.0.1:5121", "-config", base, "-config-dir", root / "derived", "-warden-config", cfg / "warden.yaml",
         "-config-trusted-keys", root / "trusted.pub", "-config-state", root / "state", "-config-seed", signed, "-fetch-dirs", gguf],
        stdout=open(OUT / "config-daemon.log", "w"), stderr=subprocess.STDOUT,
    )
    ui = subprocess.Popen(
        [BIN / "infermux-ui", "-listen", "127.0.0.1:5130", "-daemon", DAEMON, "-signed-config", signed, "-config-key", key,
         "-warden-config", cfg / "warden.yaml", "-base-config", base, "-gguf-dirs", gguf, "-kv-kernels", root / "kv.json"],
        stdout=open(OUT / "config-ui.log", "w"), stderr=subprocess.STDOUT,
    )
    try:
        if not wait_for(lambda: get(DAEMON + "/warden/config/state")["current"] and get(UI + "/api/state"), 20):
            print("the stack did not come up; see", OUT)
            return 1
        run(root, cfg, signed, gguf, key)
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


def run(root, cfg, signed, gguf, key):
    with sync_playwright() as pw:
        browser = pw.chromium.launch()
        pg = browser.new_page(viewport={"width": 2560, "height": 1300}, color_scheme="dark")
        errors = []
        pg.on("pageerror", lambda e: errors.append(str(e)))
        pg.goto(UI + "/#config")
        pg.wait_for_selector("text=Every host can run it.", timeout=15000)
        editor = pg.locator(".cm-content").first

        def popup():
            # CodeMirror ignores Enter for interactionDelay (75 ms) after it opens.
            pg.wait_for_selector(".cm-tooltip-autocomplete", timeout=5000)
            pg.wait_for_timeout(150)
            return pg.locator(".cm-tooltip-autocomplete li").all_inner_texts()

        def publish(button="Publish"):
            # The field is cleared after each Publish.
            pg.locator("input[type=password]").fill(PASSPHRASE)
            pg.locator("button", has_text=button).first.click()

        def line(n):
            """The cursor at the end of line n."""
            editor.click()
            pg.keyboard.press("Control+Home")
            for _ in range(n - 1):
                pg.keyboard.press("ArrowDown")
            pg.keyboard.press("End")

        tabs = pg.locator("nav a").all_inner_texts()
        check("the Config tab replaces Models", "Config" in tabs and "Models" not in tabs, tabs)
        bg = pg.evaluate("getComputedStyle(document.querySelector('.cm-editor')).backgroundColor")
        check("the editor is black", bg == "rgb(10, 10, 10)", bg)

        # --- Completion -------------------------------------------------------------------
        editor.click()
        pg.keyboard.press("Control+End")
        pg.keyboard.type("groups:\n  embedders: [")
        opts = popup()
        check("a group's member completes to the models", opts == ["alpha"], opts)
        pg.keyboard.press("Enter")
        pg.keyboard.type("]")
        pg.keyboard.press("Escape")
        line(9)  # "  alpha: [here/gpu]", the last of balance
        pg.keyboard.type("\n")
        pg.keyboard.press("Control+Space")
        opts = popup()
        check("a lane's keys complete to the unplaced models and groups", opts == ["embedders"], opts)
        pg.keyboard.press("Enter")
        pg.keyboard.type("[")
        opts = popup()
        check("an accepted key opens its values, a place's host/device", sorted(opts) == ["here/cpu", "here/gpu"], opts)
        pg.keyboard.type("here/c")
        pg.wait_for_timeout(150)
        pg.keyboard.press("Enter")
        pg.keyboard.type("]")
        pg.keyboard.press("Escape")
        text = editor.inner_text()
        check("the typed lane and group", "embedders: [here/cpu]" in text and "embedders: [alpha]" in text, text)
        pg.wait_for_selector("text=Every host can run it.", timeout=5000)
        rows = pg.locator("table tbody tr").all_inner_texts()
        check("the table follows the draft", [r.split()[0] for r in rows] == ["alpha"], rows)

        # --- Hover ------------------------------------------------------------------------
        pg.locator(".cm-line", has_text="failover:").first.locator("text=failover").first.hover()
        pg.wait_for_selector(".cm-schema-doc", timeout=5000)
        tip = pg.locator(".cm-schema-doc").inner_text()
        check("hovering failover: describes it", "every balance place is down or refusing" in tip, tip)
        pg.mouse.move(5, 5)

        # --- A broken draft: the daemons' loader, on its line ----------------------------
        line(9)  # gpu becomes tpu
        pg.keyboard.press("ArrowLeft")
        for _ in range(3):
            pg.keyboard.press("Backspace")
        pg.keyboard.type("tpu")
        pg.keyboard.press("Escape")
        pg.wait_for_selector("text=has no device", timeout=5000)
        err = pg.locator(".card", has_text="Check").inner_text()
        check("a place on a missing device is the check's error", 'no device "tpu"' in err, err)
        check("Publish is off while the draft is broken", pg.locator("button", has_text="Publish").first.is_disabled())
        for _ in range(3):
            pg.keyboard.press("Backspace")
        pg.keyboard.type("gpu")
        pg.keyboard.press("Escape")
        check("the line is back", "alpha: [here/gpu]" in editor.inner_text(), editor.inner_text())
        pg.wait_for_selector("text=Every host can run it.", timeout=5000)

        # --- A model for a GGUF on this host, placed on the GPU --------------------------
        pg.locator("button", has_text="Model from a GGUF here").click()
        sel = pg.locator("select").first
        paths = sel.locator("option").all_inner_texts()
        check("the GGUFs no model names", len(paths) == 3 and "beta.gguf" in paths[1] and "unused.gguf" in paths[2], paths)
        sel.select_option(str(gguf / "beta.gguf"))
        pg.locator("select").nth(2).select_option("here/gpu")
        pg.locator("button", has_text="Add").last.click()
        pg.wait_for_selector("text=beta.yaml", timeout=5000)
        beta = editor.inner_text()
        sha = hashlib.sha256(b"GGUF beta").hexdigest()
        check("the new model has alpha's command and beta's hash",
              "The command is alpha's" in beta and "beta.gguf" in beta and sha in beta and "--ctx-size 4096" in beta, beta)
        pg.locator("button", has_text="infermux.yaml").click()
        check("beta is in the balance lane", "beta: [here/gpu]" in editor.inner_text(), editor.inner_text())
        pg.wait_for_selector("text=Every host can run it.", timeout=5000)
        pg.wait_for_timeout(500)
        pg.screenshot(path=str(OUT / "config-draft.png"), full_page=True)

        pg.reload()
        pg.wait_for_selector("text=beta.yaml", timeout=10000)
        check("a reload keeps the draft", "beta: [here/gpu]" in editor.inner_text())

        # --- Publish ----------------------------------------------------------------------
        pg.locator("input[type=password]").fill("not the passphrase")
        pg.locator("button", has_text="Publish").first.click()
        pg.wait_for_selector("text=does not open the config key", timeout=10000)
        check("a wrong passphrase publishes nothing", state()["current"]["revision"] == 1)
        publish()
        pg.wait_for_selector("text=Revision 2 runs here", timeout=15000)
        st = state()
        check("the daemon runs revision 2", st["current"]["revision"] == 2 and "beta" in st["routes"], st)
        m = manifest(signed)
        check("nixcfg's copy is revision 2 with beta", m["revision"] == 2 and (signed / "models" / "beta.yaml").exists(), m)
        check("the draft is the running version", pg.locator("text=The draft is the running version.").count() == 1)
        status, body = 0, ""
        req = urllib.request.Request(DAEMON + "/v1/chat/completions", method="POST", headers={"Content-Type": "application/json"},
                                     data=json.dumps({"model": "beta", "max_tokens": 3, "messages": [{"role": "user", "content": "hi"}]}).encode())
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                status, body = r.status, r.read().decode()
        except urllib.error.HTTPError as e:
            status, body = e.code, e.read().decode()
        check("the published model answers", status == 200, f"{status} {body[:200]}")

        # --- Another host publishes while this draft has an edit -------------------------
        editor.click()
        pg.keyboard.press("Control+End")
        pg.keyboard.type("\n# an edit here\n")
        pg.wait_for_timeout(600)
        res = publish_as_zbox(signed, key, root / "other", lambda d: (d / "models" / "alpha.yaml").write_text(
            (d / "models" / "alpha.yaml").read_text().replace("--ctx-size 4096", "--ctx-size 8192")))
        check("zbox's revision 3 runs", res.get("result") == "applied", res)
        rev3 = state()["current"]["hash"]
        pg.wait_for_selector("text=came after it", timeout=10000)
        check("the stale draft is shown", True)
        publish()
        pg.wait_for_selector("text=Another Publish came first", timeout=10000)
        names = pg.locator(".card.border-sky-900 button.font-mono").all_inner_texts()
        check("the comparison lists the files that differ", names == ["infermux.yaml", "models/alpha.yaml"], names)
        pg.screenshot(path=str(OUT / "config-stale.png"), full_page=True)
        # Zbox's change to alpha, taken into the draft with the merge's arrow.
        pg.locator(".card.border-sky-900 button.font-mono", has_text="models/alpha.yaml").click()
        pg.wait_for_selector(".cm-merge-revert button", timeout=5000)
        pg.locator(".cm-merge-revert button").first.click()
        pg.wait_for_timeout(300)
        pg.locator("button", has_text="Build on the running version").click()
        publish()
        pg.wait_for_selector("text=Revision 4 runs here", timeout=15000)
        st = state()
        m = manifest(signed)
        check("revision 4's parent is zbox's 3", m["revision"] == 4 and m["parent"] == rev3 and st["current"]["revision"] == 4, (m, rev3))
        check("revision 4 has zbox's ctx-size and this host's comment",
              "--ctx-size 8192" in (signed / "models" / "alpha.yaml").read_text() and "# an edit here" in (signed / "infermux.yaml").read_text())
        check("no conflict", st["conflict"] is None, st["conflict"])

        # --- A forced Publish over a version it did not see: a conflict -------------------
        editor.click()
        pg.keyboard.press("Control+End")
        pg.keyboard.type("# forced\n")
        pg.wait_for_timeout(600)
        res = publish_as_zbox(signed, key, root / "other", lambda d: (d / "infermux.yaml").write_text((d / "infermux.yaml").read_text() + "# from zbox\n"))
        check("zbox's revision 5 runs", res.get("result") == "applied", res)
        time.sleep(1.1)  # last write wins: the forced version must be the later one
        publish()
        pg.wait_for_selector("text=Another Publish came first", timeout=10000)
        pg.locator("button", has_text="Publish over it").click()
        pg.wait_for_selector("text=replaced a version edited apart", timeout=15000)
        st = state()
        check("the daemon records the conflict", st["conflict"] and st["conflict"]["lost"]["host"] == "zbox", st["conflict"])
        check("nixcfg's copy is the forced version", "# forced" in (signed / "infermux.yaml").read_text())
        pg.wait_for_selector("text=were edited apart", timeout=10000)
        pg.locator("button", has_text="Compare it with the draft").click()
        pg.wait_for_selector("text=the version that lost", timeout=5000)
        check("the comparison shows the lost edit", "# from zbox" in pg.locator(".cm-merge-b").inner_text())
        pg.screenshot(path=str(OUT / "config-conflict.png"), full_page=True)
        pg.locator("button", has_text="Close").click()
        pg.locator("button", has_text="Dismiss on every host").click()
        check("dismissed", wait_for(lambda: state()["conflict"] is None, 5))

        # --- Changes commits nixcfg's copy -----------------------------------------------
        pg.goto(UI + "/#changes")
        pg.wait_for_selector("text=Uncommitted", timeout=10000)
        ch = pg.locator(".card", has_text="Uncommitted").inner_text()
        check("Changes lists nixcfg's copy", "infermux/manifest.json" in ch and "infermux/models/beta.yaml" in ch, ch)

        # --- A model file's help: runtimes, and the K-V warning on its flag ----------------
        pg.goto(UI + "/#config")
        pg.wait_for_selector("text=Every host can run it.", timeout=15000)
        pg.locator("button", has_text="alpha.yaml").click()
        line(6)  # "    --ctx-size 8192"
        pg.keyboard.type("\n--cache-type-k q4_0")
        pg.keyboard.press("Escape")
        pg.wait_for_selector(".cm-lintRange-warning", timeout=5000)
        marked = pg.locator(".cm-lintRange-warning").all_inner_texts()
        check("a cache pair with no kernel is a warning on its flag, not the comment's", marked == ["--cache-type-k q4_0"], marked)
        pg.keyboard.type("\n${")
        opts = popup()
        check("${ offers the runtimes and llama-swap's macros", opts == ["${fake-server}", "${MODEL_ID}", "${PORT}"], opts)
        pg.keyboard.press("Escape")
        pg.screenshot(path=str(OUT / "config-model.png"), full_page=True)

        check("no page errors", not errors, errors)
        browser.close()


if __name__ == "__main__":
    sys.exit(main())
