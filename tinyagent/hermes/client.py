"""Small JSON-lines client for the tinyagent helper."""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import os
from typing import Any, Awaitable, Callable

# The helper receives only what it needs: a lookup path, a home for TLS and
# temp files, its signing key and the proxy settings of the gateway.
_HELPER_ENV = ("PATH", "HOME", "TMPDIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
               "http_proxy", "https_proxy", "no_proxy")


def scoped_secret(name: str, default: str = "", *, strict: bool = False) -> str:
    """Resolve a TINY_* setting through Hermes's profile secret scope.

    A multiplexed gateway serves several profiles from one process and keeps each
    profile's ``.env`` in a context-local scope rather than in ``os.environ``; reading
    through ``get_secret`` hands a profile its own key. Outside Hermes, or on a Hermes
    without the scope API, the process environment is the source. ``strict`` lets the
    fail-closed error of an unscoped read in multiplex mode propagate; passive probes
    use the default instead.
    """
    try:
        from agent.secret_scope import get_secret
    except ImportError:
        return os.environ.get(name, default)
    try:
        value = get_secret(name, default)
    except RuntimeError:
        if strict:
            raise
        return default
    return default if value is None else str(value)


class TinyRPC:
    def __init__(self, relay_url: str, key_env: str = "TINY_PRIVATE_KEY", cli_path: str = "tinyagent",
                 on_exit: Callable[["TinyRPC"], Any] | None = None):
        self.relay_url = relay_url
        self.key_env = key_env
        self.cli_path = cli_path
        self.on_exit = on_exit
        self._proc: asyncio.subprocess.Process | None = None
        self._reader_task: asyncio.Task | None = None
        self._next_id = 0
        self._pending: dict[int, asyncio.Future] = {}
        self._subscriptions: dict[str, Callable[[dict], Awaitable[None]]] = {}
        self._callback_tasks: set[asyncio.Task] = set()
        self._ready: dict[str, asyncio.Event] = {}
        self._write_lock = asyncio.Lock()
        self._closing = False
        self._exited = False

    def helper_env(self) -> dict[str, str]:
        """Return the scoped environment handed to the helper process.

        The signing key is resolved through the active profile scope and reaches the
        helper only through its environment variable."""
        env = {name: value for name, value in os.environ.items() if name in _HELPER_ENV}
        key = scoped_secret(self.key_env, "", strict=True)
        if key:
            env[self.key_env] = key
        return env

    @property
    def alive(self) -> bool:
        return self._proc is not None and self._proc.returncode is None and not self._exited

    async def connect(self) -> None:
        if self._proc is not None:
            if self.alive:
                return
            # A dead helper is not respawned in place: its subscriptions are gone,
            # so the owner must rebuild the session through on_exit.
            raise RuntimeError("tiny rpc exited")
        # The key is inherited by the helper, never put in arguments or logs.
        self._proc = await asyncio.create_subprocess_exec(
            self.cli_path, "rpc", "--relay", self.relay_url, "--key-env", self.key_env,
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL, env=self.helper_env(), limit=64 * 1024 * 1024,
        )
        self._reader_task = asyncio.create_task(self._read_loop())

    async def close(self) -> None:
        self._closing = True
        if self._reader_task:
            self._reader_task.cancel()
            await asyncio.gather(self._reader_task, return_exceptions=True)
        if not self._exited:
            # A deliberate close ends in-flight event handling; turns started by a helper
            # that died on its own run to completion and are redelivered if they fail.
            for task in self._callback_tasks:
                task.cancel()
            await asyncio.gather(*self._callback_tasks, return_exceptions=True)
        self._callback_tasks.clear()
        if self._proc and self._proc.returncode is None:
            self._proc.terminate()
            await self._proc.wait()
        self._fail_pending(RuntimeError("tiny rpc closed"))
        self._proc = None

    def _fail_pending(self, error: Exception) -> None:
        for future in self._pending.values():
            if not future.done():
                future.set_exception(error)
        self._pending.clear()

    async def _read_loop(self) -> None:
        assert self._proc and self._proc.stdout
        died = False
        try:
            try:
                await self._pump()
            except asyncio.CancelledError:
                raise
            except Exception:
                logging.getLogger(__name__).exception("Tiny helper stream failed")
            # EOF or a broken pipe: the helper exited (or was closed) rather than being cancelled.
            died = not self._closing
        finally:
            self._fail_pending(RuntimeError("tiny rpc exited"))
            if died and not self._exited:
                # The helper went away on its own; tell the owner exactly once.
                self._exited = True
                if self.on_exit is not None:
                    try:
                        result = self.on_exit(self)
                        if asyncio.iscoroutine(result):
                            await result
                    except Exception:
                        logging.getLogger(__name__).exception("Tiny helper exit handler failed")

    async def _pump(self) -> None:
        assert self._proc and self._proc.stdout
        async for line in self._proc.stdout:
            try:
                msg = json.loads(line)
            except (ValueError, UnicodeDecodeError):
                continue
            if msg.get("event") == "event":
                callback = self._subscriptions.get(str(msg.get("subscription", "")))
                if callback:
                    task = asyncio.create_task(callback(msg.get("data") or {}))
                    self._callback_tasks.add(task)
                    task.add_done_callback(self._callback_done)
                continue
            if msg.get("event") == "connected":
                ready = self._ready.get(str(msg.get("subscription", "")))
                if ready:
                    ready.set()
                continue
            request_id = msg.get("id")
            if isinstance(request_id, int) and request_id in self._pending:
                future = self._pending.pop(request_id)
                if future.done():
                    continue
                if "error" in msg:
                    future.set_exception(RuntimeError(str((msg["error"] or {}).get("message", "rpc error"))))
                else:
                    future.set_result(msg.get("result"))

    def _callback_done(self, task):
        self._callback_tasks.discard(task)
        if not task.cancelled() and task.exception():
            logging.getLogger(__name__).error("Tiny event handling failed", exc_info=task.exception())

    async def call(self, method: str, params: dict[str, Any] | None = None, timeout: float = 30) -> Any:
        await self.connect()
        if not self._proc or not self._proc.stdin:
            raise RuntimeError("tiny rpc is not connected")
        self._next_id += 1
        request_id = self._next_id
        loop = asyncio.get_running_loop()
        future = loop.create_future()
        self._pending[request_id] = future
        payload = json.dumps({"id": request_id, "method": method, "params": params or {}}, separators=(",", ":"))
        try:
            async with self._write_lock:
                self._proc.stdin.write((payload + "\n").encode())
                await self._proc.stdin.drain()
            return await asyncio.wait_for(future, timeout)
        finally:
            self._pending.pop(request_id, None)

    async def publish(self, event: dict) -> dict:
        return await self.call("publish", {"event": event})

    async def query(self, filter: dict) -> list[dict]:
        return await self.call("query", {"filter": filter})

    async def subscribe(self, subscription: str, filter: dict, callback: Callable[[dict], Awaitable[None]]) -> Any:
        self._subscriptions[subscription] = callback
        self._ready[subscription] = asyncio.Event()
        result = await self.call("subscribe", {"subscription": subscription, "filter": filter})
        await asyncio.wait_for(self._ready[subscription].wait(), 30)
        return result

    async def unsubscribe(self, subscription: str) -> Any:
        self._subscriptions.pop(subscription, None)
        self._ready.pop(subscription, None)
        return await self.call("unsubscribe", {"subscription": subscription})

    async def upload(self, room: str, filename: str, media_type: str, data: bytes) -> dict:
        return await self.call("upload", {"room": room, "filename": filename, "type": media_type,
                                           "data": base64.b64encode(data).decode()})

    async def download(self, path: str) -> tuple[bytes, str]:
        result = await self.call("download", {"path": path})
        return base64.b64decode(result["data"]), result.get("type", "application/octet-stream")
