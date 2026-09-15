"""TinyRPC process handling against a fake helper; no Hermes or network needed."""

import asyncio
import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))

import client as module
from client import TinyRPC


class FakeStdin:
    def __init__(self, process):
        self.process = process
        self.lines = []

    def write(self, data):
        self.lines.append(json.loads(data))
        self.process.on_request(self.lines[-1])

    async def drain(self):
        pass


class FakeProcess:
    """Answers every request immediately and dies on demand."""

    def __init__(self):
        self.stdout = asyncio.StreamReader()
        self.stdin = FakeStdin(self)
        self.returncode = None
        self.terminated = False

    def on_request(self, request):
        if request["method"] == "hang":
            return
        reply = {"id": request["id"], "result": {"method": request["method"]}}
        self.stdout.feed_data((json.dumps(reply) + "\n").encode())
        if request["method"] == "subscribe":
            self.stdout.feed_data(json.dumps({"event": "connected", "subscription": request["params"]["subscription"]}).encode() + b"\n")

    def die(self):
        self.returncode = 1
        self.stdout.feed_eof()

    def terminate(self):
        self.terminated = True
        self.returncode = -15
        self.stdout.feed_eof()

    async def wait(self):
        return self.returncode


@pytest.fixture
def spawn(monkeypatch):
    spawned = []

    async def create(*args, **kwargs):
        process = FakeProcess()
        spawned.append((args, kwargs, process))
        return process

    monkeypatch.setattr(module.asyncio, "create_subprocess_exec", create)
    return spawned


def test_helper_gets_a_scoped_environment(spawn, monkeypatch):
    monkeypatch.setenv("TINY_PRIVATE_KEY", "secret")
    monkeypatch.setenv("HTTPS_PROXY", "http://proxy")
    monkeypatch.setenv("no_proxy", "localhost")
    monkeypatch.setenv("OPENAI_API_KEY", "leak")
    monkeypatch.setenv("HERMES_HOME", "/somewhere")

    async def run():
        rpc = TinyRPC("http://relay", cli_path="/bin/tinyagent")
        await rpc.connect()
        await rpc.close()

    asyncio.run(run())
    args, kwargs, _process = spawn[0]
    assert args[:2] == ("/bin/tinyagent", "rpc")
    env = kwargs["env"]
    assert env["TINY_PRIVATE_KEY"] == "secret"
    assert env["HTTPS_PROXY"] == "http://proxy" and env["no_proxy"] == "localhost"
    assert "OPENAI_API_KEY" not in env and "HERMES_HOME" not in env
    assert set(env) <= {"PATH", "HOME", "TMPDIR", "TINY_PRIVATE_KEY", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
                        "http_proxy", "https_proxy", "no_proxy"}


def test_helper_death_fails_pending_and_reports_once(spawn):
    exits = []

    async def run():
        rpc = TinyRPC("http://relay", on_exit=exits.append)
        assert (await rpc.call("identity")) == {"method": "identity"}
        process = spawn[0][2]
        hanging = asyncio.ensure_future(rpc.call("hang"))
        await asyncio.sleep(0)
        process.die()
        with pytest.raises(RuntimeError, match="tiny rpc exited"):
            await hanging
        await asyncio.sleep(0)
        assert exits == [rpc]
        assert not rpc.alive
        # No silent respawn: the owner rebuilds the session instead.
        with pytest.raises(RuntimeError, match="tiny rpc exited"):
            await rpc.call("identity")
        assert len(spawn) == 1
        await rpc.close()
        assert exits == [rpc]

    asyncio.run(run())


def test_close_does_not_report_an_exit(spawn):
    exits = []

    async def run():
        rpc = TinyRPC("http://relay", on_exit=exits.append)
        await rpc.connect()
        await rpc.close()
        await asyncio.sleep(0)

    asyncio.run(run())
    assert exits == []
    assert spawn[0][2].terminated


def test_async_exit_handler_is_awaited(spawn):
    seen = []

    async def run():
        done = asyncio.Event()

        async def on_exit(rpc):
            seen.append(rpc)
            done.set()

        rpc = TinyRPC("http://relay", on_exit=on_exit)
        await rpc.connect()
        spawn[0][2].die()
        await asyncio.wait_for(done.wait(), 1)

    asyncio.run(run())
    assert len(seen) == 1


def test_dead_helper_keeps_in_flight_callbacks(spawn):
    async def run():
        finished = asyncio.Event()

        async def callback(event):
            await asyncio.sleep(0.01)
            finished.set()

        rpc = TinyRPC("http://relay")
        await rpc.subscribe("s", {"kinds": [9]}, callback)
        process = spawn[0][2]
        process.stdout.feed_data(json.dumps({"event": "event", "subscription": "s", "data": {"id": "x"}}).encode() + b"\n")
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        process.die()
        await asyncio.sleep(0)
        await rpc.close()
        await asyncio.wait_for(finished.wait(), 1)

    asyncio.run(run())
