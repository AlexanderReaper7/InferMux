"""The HTTP face (`warden/agent.py`). Read-only since the Linux port (0002)."""

import pytest
from fastapi import HTTPException

from warden import agent, watch


def test_verdict_without_a_watcher_is_503(monkeypatch):
    """Importing and serving `agent` alone starts no loop. Answering with a
    default verdict would read as "the GPU is free" when nothing has looked."""
    monkeypatch.setattr(watch, "current", None)
    with pytest.raises(HTTPException) as exc:
        agent.verdict()
    assert exc.value.status_code == 503


def test_a_probe_failure_is_503_not_a_crash(monkeypatch):
    def explode():
        raise OSError("NVML Shared Library Not Found")

    monkeypatch.setattr(agent, "_probe", explode)
    with pytest.raises(HTTPException) as exc:
        agent.resources()
    assert exc.value.status_code == 503
    assert "NVML" in exc.value.detail


def test_port_parsing_reads_host_and_port(monkeypatch):
    seen = []

    class Conn:
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(
        agent.socket, "create_connection", lambda addr, timeout: seen.append(addr) or Conn()
    )
    assert agent._port_open("http://127.0.0.1:8188/") is True
    assert seen == [("127.0.0.1", 8188)]
    assert agent._port_open("not a url") is False
