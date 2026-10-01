"""A second runtime must count as ours and yield with the primary router."""

from dataclasses import replace

from warden import agent, router
from warden.config import load
from warden.watch import Watcher

PRIMARY = "http://127.0.0.1:5001"
BONSAI = "http://127.0.0.1:5004"


def test_legacy_config_still_has_one_router(tmp_path):
    path = tmp_path / "warden.toml"
    path.write_text('[agent]\nrouter_url = "http://127.0.0.1:9001"\n')
    assert load(path).router_urls == ("http://127.0.0.1:9001",)


def test_config_loads_and_deduplicates_additional_routers(tmp_path):
    path = tmp_path / "warden.toml"
    path.write_text(
        f'[agent]\nadditional_router_urls = ["{PRIMARY}/", "{BONSAI}", "{BONSAI}/"]\n'
        'our_units = ["llama-cpp.service", "llama-bonsai.service"]\n'
    )
    settings = load(path)
    assert settings.router_urls == (PRIMARY, BONSAI)
    assert "llama-bonsai.service" in settings.our_units


def test_second_router_prevents_low_free_vram_from_being_foreign_contention(monkeypatch):
    settings = replace(load(), additional_router_urls=(BONSAI,), comfyui_url=None, consumers=())
    monkeypatch.setattr(router, "holds_vram", lambda url: url == BONSAI)
    watcher = Watcher(
        settings,
        resources=lambda: {"foreign_gpu_percent": 0, "vram_free_mb": 500, "culprits": []},
    )
    assert watcher.tick().yielded is False


def test_yield_unloads_both_routers_once_even_when_primary_is_down(monkeypatch):
    settings = replace(load(), additional_router_urls=(BONSAI,), comfyui_url=None, consumers=())
    calls = []
    monkeypatch.setattr(
        router,
        "models",
        lambda url, **kw: (
            [] if url == PRIMARY else [{"id": "bonsai", "status": {"value": "loaded"}}]
        ),
    )
    monkeypatch.setattr(
        router, "post_json", lambda url, payload, **kw: calls.append((url, payload))
    )
    watcher = Watcher(
        settings,
        resources=lambda: {"foreign_gpu_percent": 90, "vram_free_mb": 500, "culprits": []},
    )
    watcher.tick()
    watcher.tick()
    assert calls == [(f"{BONSAI}/models/unload", {"model": "bonsai"})]
    assert watcher.last_unload == ["bonsai"]


def test_yield_unloads_loaded_models_on_both_routers(monkeypatch):
    settings = replace(load(), additional_router_urls=(BONSAI,), comfyui_url=None, consumers=())
    calls = []
    monkeypatch.setattr(router, "holds_vram", lambda url: True)
    monkeypatch.setattr(router, "unload_all", lambda url: calls.append(url) or [url])
    watcher = Watcher(
        settings,
        resources=lambda: {"foreign_gpu_percent": 90, "vram_free_mb": 500, "culprits": []},
    )
    watcher.tick()
    assert calls == [PRIMARY, BONSAI]
    assert watcher.last_unload == [PRIMARY, BONSAI]


def test_status_preserves_primary_models_and_reports_second_router(monkeypatch):
    settings = replace(load(), additional_router_urls=(BONSAI,), comfyui_url=None)
    monkeypatch.setattr(agent, "settings", settings)
    monkeypatch.setattr(agent, "_port_open", lambda url: True)
    monkeypatch.setattr(router, "models", lambda url: [{"id": url, "status": {"value": "loaded"}}])
    status = agent.status()
    assert status["services"]["router"]["url"] == PRIMARY
    assert status["services"]["router_2"]["url"] == BONSAI
    assert status["router_models"] == [{"id": PRIMARY, "status": "loaded"}]
    assert status["additional_router_models"][BONSAI] == [{"id": BONSAI, "status": "loaded"}]
