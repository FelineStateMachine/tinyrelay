import hashlib
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
from media import incoming_media  # noqa: E402


class FakeRPC:
    def __init__(self, data: bytes):
        self.data = data
        self.paths = []

    async def download(self, path):
        self.paths.append(path)
        return self.data, "text/plain"


@pytest.mark.asyncio
async def test_incoming_media_caches_same_relay_content(tmp_path, monkeypatch):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    data = b"tinyagent attachment\n"
    digest = hashlib.sha256(data).hexdigest()
    rpc = FakeRPC(data)
    urls, types = await incoming_media(
        rpc,
        {"tags": [["imeta", f"url http://tiny:7447/media/{digest}", "m text/plain", "filename note.txt", f"x {digest}", f"size {len(data)}"]]},
        "http://tiny:7447",
    )
    assert len(urls) == 1 and Path(urls[0]).is_file()
    assert types == ["text/plain"]
    assert rpc.paths == [f"/media/{digest}"]


@pytest.mark.asyncio
@pytest.mark.parametrize("url", ["https://example.com/media/" + "a" * 64, "http://tiny:7447/files/a"])
async def test_incoming_media_rejects_external_or_non_media_url(url, tmp_path, monkeypatch):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    rpc = FakeRPC(b"bad")
    urls, _ = await incoming_media(rpc, {"tags": [["imeta", "url " + url]]}, "http://tiny:7447")
    assert urls == [] and rpc.paths == []


@pytest.mark.asyncio
async def test_incoming_media_rejects_false_hash_and_size(tmp_path, monkeypatch):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    data = b"actual"
    digest = hashlib.sha256(data).hexdigest()
    rpc = FakeRPC(data)
    urls, _ = await incoming_media(
        rpc,
        {"tags": [["imeta", f"url /media/{digest}", f"x {'0' * 64}", "size 999"]]},
        "http://tiny:7447",
    )
    assert urls == []


def test_tenant_media_paths_remain_relative_to_helper_base():
    from media import _media_path
    digest = 'a' * 64
    assert _media_path(f'https://tiny/r/team/media/{digest}', 'https://tiny/r/team') == (f'/media/{digest}', digest)
    assert _media_path(f'https://tiny/r/other/media/{digest}', 'https://tiny/r/team') is None
    assert _media_path(f'https://tiny/media/{digest}?token=no', 'https://tiny') is None
    assert _media_path(f'/media/{digest}#fragment', 'https://tiny') is None
