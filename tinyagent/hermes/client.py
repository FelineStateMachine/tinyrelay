"""Small JSON-lines client for the tinyagent helper."""

from __future__ import annotations

import asyncio
import base64
import json
import logging
import os
from typing import Any, Awaitable, Callable


class TinyRPC:
    def __init__(self, relay_url: str, key_env: str = "TINY_PRIVATE_KEY", cli_path: str = "tinyagent"):
        self.relay_url = relay_url
        self.key_env = key_env
        self.cli_path = cli_path
        self._proc: asyncio.subprocess.Process | None = None
        self._reader_task: asyncio.Task | None = None
        self._next_id = 0
        self._pending: dict[int, asyncio.Future] = {}
        self._subscriptions: dict[str, Callable[[dict], Awaitable[None]]] = {}
        self._callback_tasks: set[asyncio.Task] = set()
        self._ready: dict[str, asyncio.Event] = {}
        self._write_lock = asyncio.Lock()

    async def connect(self) -> None:
        if self._proc and self._proc.returncode is None:
            return
        env = dict(os.environ)
        # The key is inherited by the helper, never put in arguments or logs.
        self._proc = await asyncio.create_subprocess_exec(
            self.cli_path, "rpc", "--relay", self.relay_url, "--key-env", self.key_env,
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL, env=env, limit=64 * 1024 * 1024,
        )
        self._reader_task = asyncio.create_task(self._read_loop())

    async def close(self) -> None:
        if self._reader_task:
            self._reader_task.cancel()
            await asyncio.gather(self._reader_task, return_exceptions=True)
        for task in self._callback_tasks:
            task.cancel()
        await asyncio.gather(*self._callback_tasks, return_exceptions=True)
        self._callback_tasks.clear()
        if self._proc and self._proc.returncode is None:
            self._proc.terminate()
            await self._proc.wait()
        error = RuntimeError("tiny rpc closed")
        for future in self._pending.values():
            if not future.done():
                future.set_exception(error)
        self._pending.clear()
        self._proc = None

    async def _read_loop(self) -> None:
        assert self._proc and self._proc.stdout
        try:
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
        finally:
            error = RuntimeError("tiny rpc exited")
            for future in self._pending.values():
                if not future.done():
                    future.set_exception(error)
            self._pending.clear()

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
