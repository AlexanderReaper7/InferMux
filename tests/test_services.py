"""The two HTTP services the warden acts on: the llama.cpp router and ComfyUI.

`urlopen` and `post_json` are stubbed, so these pin the reading of each answer
and which requests go out, not the network.
"""

import json
from io import BytesIO

import pytest

from warden import comfyui, router


def _answer(monkeypatch, module, payload):
    class Response(BytesIO):
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(
        f"warden.{module}.urllib.request.urlopen",
        lambda *a, **kw: Response(json.dumps(payload).encode()),
    )


def _refuse(monkeypatch, module):
    def refuse(*a, **kw):
        raise ConnectionRefusedError()

    monkeypatch.setattr(f"warden.{module}.urllib.request.urlopen", refuse)


@pytest.fixture
def posts(monkeypatch):
    sent: list[tuple[str, dict]] = []

    def post(url, payload, *, timeout=10.0):
        sent.append((url, payload))
        return {}

    monkeypatch.setattr("warden.router.post_json", post)
    monkeypatch.setattr("warden.comfyui.post_json", post)
    return sent


# --- the router ------------------------------------------------------------------

ROUTER = "http://127.0.0.1:5001"


def test_a_loading_model_counts_as_loaded(monkeypatch):
    """A model halfway into VRAM occupies it exactly as much as a resident one.
    Reading `loading` as free is what once let a governor pause and unload the
    model it was in the middle of loading."""
    _answer(monkeypatch, "router", {"data": [{"id": "main", "status": {"value": "loading"}}]})
    assert router.holds_vram(ROUTER) is True


def test_an_unloaded_router_holds_nothing(monkeypatch):
    _answer(monkeypatch, "router", {"data": [{"id": "main", "status": {"value": "unloaded"}}]})
    assert router.holds_vram(ROUTER) is False


def test_a_silent_router_holds_nothing(monkeypatch):
    """Same no-opinion direction as the rest of this: a router that is down is
    not holding a model, and guessing otherwise would suppress the VRAM half of
    the policy precisely when it applies."""
    _refuse(monkeypatch, "router")
    assert router.holds_vram(ROUTER) is False


def test_unload_asks_only_for_the_models_in_memory(monkeypatch, posts):
    _answer(
        monkeypatch,
        "router",
        {
            "data": [
                {"id": "main", "status": {"value": "loaded"}},
                {"id": "coder", "status": {"value": "unloaded"}},
                {"id": "draft", "status": {"value": "loading"}},
            ]
        },
    )
    assert router.unload_all(ROUTER) == ["main", "draft"]
    assert posts == [
        (f"{ROUTER}/models/unload", {"model": "main"}),
        (f"{ROUTER}/models/unload", {"model": "draft"}),
    ]


def test_a_refused_unload_does_not_stop_the_others(monkeypatch):
    _answer(
        monkeypatch,
        "router",
        {
            "data": [
                {"id": "a", "status": {"value": "loaded"}},
                {"id": "b", "status": {"value": "loaded"}},
            ]
        },
    )

    def post(url, payload, *, timeout=10.0):
        if payload["model"] == "a":
            raise OSError("400 model is not running")
        return {}

    monkeypatch.setattr("warden.router.post_json", post)
    assert router.unload_all(ROUTER) == ["b"]


# --- ComfyUI ---------------------------------------------------------------------

COMFY = "http://127.0.0.1:8188"


def test_queue_depth_counts_running_and_pending(monkeypatch):
    _answer(
        monkeypatch,
        "comfyui",
        {"queue_running": [[0, "a"]], "queue_pending": [[1, "b"], [2, "c"]]},
    )
    assert comfyui.queue_depth(COMFY) == 3


def test_an_empty_queue_is_zero(monkeypatch):
    _answer(monkeypatch, "comfyui", {"queue_running": [], "queue_pending": []})
    assert comfyui.queue_depth(COMFY) == 0


def test_a_silent_comfyui_is_no_opinion(monkeypatch):
    """None, not 0: a ComfyUI too busy to answer may be the busiest thing on
    the card."""
    _refuse(monkeypatch, "comfyui")
    assert comfyui.queue_depth(COMFY) is None


def test_free_unloads_models_and_the_cache(posts):
    comfyui.free(COMFY)
    assert posts == [(f"{COMFY}/free", {"unload_models": True, "free_memory": True})]
