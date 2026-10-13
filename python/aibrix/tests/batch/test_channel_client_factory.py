from aibrix import envs
from aibrix.batch.client.channel import create_batch_runtime_http_client
from aibrix.metadata.core.aiohttp_client import AIOHTTPClientWrapper
from aibrix.metadata.core.httpx_client import HTTPXClientWrapper


def test_batch_runtime_http_client_defaults_to_aiohttp(monkeypatch):
    monkeypatch.setattr(envs, "BATCH_RUNTIME_HTTP_CLIENT", "aiohttp_client")

    client = create_batch_runtime_http_client("test-client")

    assert isinstance(client, AIOHTTPClientWrapper)


def test_batch_runtime_http_client_can_select_httpx(monkeypatch):
    monkeypatch.setattr(envs, "BATCH_RUNTIME_HTTP_CLIENT", "httpx_client")

    client = create_batch_runtime_http_client("test-client")

    assert isinstance(client, HTTPXClientWrapper)


def test_batch_runtime_http_client_rejects_unknown_implementation(monkeypatch):
    monkeypatch.setattr(envs, "BATCH_RUNTIME_HTTP_CLIENT", "unknown")

    try:
        create_batch_runtime_http_client("test-client")
    except ValueError as exc:
        assert "AIBRIX_BATCH_RUNTIME_HTTP_CLIENT" in str(exc)
    else:
        raise AssertionError("expected ValueError")
