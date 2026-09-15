"""Native Tinyrelay adapter for Hermes Agent.

Tiny is a Nostr relay connection. The adapter intentionally uses structured events for
interactive prompts; no Buzz compatibility or prose parsing is involved.
"""

from __future__ import annotations

import asyncio
from contextvars import ContextVar
import json
import logging
import mimetypes
import hashlib
import os
import re
import time
from datetime import datetime
from pathlib import Path
from typing import Any

from gateway.config import Platform, PlatformConfig
from gateway.platforms.base import BasePlatformAdapter, SendResult
from gateway.platforms.event import MessageEvent, MessageType

try:
    from .client import TinyRPC
    from .interactions import InteractionMixin
    from .media import incoming_media
    from .ingress import EventTracker, IdentityLock, event_thread, reply_parent
    from . import tasks as taskmod
    from .tasks import TaskState, answer_tags, request_root, request_text
except ImportError:  # Hermes plugin loader may import adapter.py as a top-level module.
    from client import TinyRPC
    from interactions import InteractionMixin
    from media import incoming_media
    from ingress import EventTracker, IdentityLock, event_thread, reply_parent
    import tasks as taskmod
    from tasks import TaskState, answer_tags, request_root, request_text

logger = logging.getLogger(__name__)

_HEX = re.compile(r"\b[0-9a-f]{64}\b", re.I)
_MENTION = re.compile(r"(?<![0-9A-Za-z])@([0-9a-f]{64})(?![0-9A-Za-z])", re.I)
# Buzz's convention: this exact message from an allowed user stops the current turn.
CANCEL_COMMAND = "!cancel"


def _tags(event: dict, name: str) -> list[list[str]]:
    return [tag for tag in event.get("tags", []) if len(tag) >= 2 and tag[0] == name]


def _tag(event: dict, name: str) -> str | None:
    found = _tags(event, name)
    return found[0][1] if found else None


def _explicit_mentions(text: str) -> list[str]:
    """Return only public keys explicitly written as @mentions in text."""
    return list(dict.fromkeys(match.group(1).lower() for match in _MENTION.finditer(text or "")))


def _mention_tags(text: str) -> list[list[str]]:
    return [["p", pubkey] for pubkey in _explicit_mentions(text)]


class TinyAdapter(InteractionMixin, BasePlatformAdapter):
    """Native room messages and interactions for Hermes."""

    supports_code_blocks = True
    supports_async_delivery = True
    MAX_MESSAGE_LENGTH = 12000
    MAX_INTERACTION_OPTIONS = 12
    # Helper-death recovery backoff in seconds; Hermes's own watcher starts at 30s.
    RECONNECT_MIN = 1.0
    RECONNECT_MAX = 30.0
    SWEEP_INTERVAL = 60.0
    # Long-task cards: at most one 43003 progress event per task in this many seconds.
    PROGRESS_INTERVAL = 15.0
    MAX_TASKS = 200

    def __init__(self, config: PlatformConfig):
        super().__init__(config, Platform("tiny"))
        extra = config.extra or {}
        self.relay_url = str(extra.get("relay_url") or os.environ.get("TINY_RELAY_URL", "")).strip()
        self.cli_path = str(extra.get("cli_path") or os.environ.get("TINY_CLI_PATH", "tinyagent"))
        self.rooms = [x.strip() for x in str(extra.get("rooms") or extra.get("channels") or os.environ.get("TINY_CHANNELS", "")).split(",") if x.strip()]
        home = extra.get("home_channel") or getattr(getattr(config, "home_channel", None), "chat_id", None) \
            or os.environ.get("TINY_HOME_CHANNEL", "")
        if isinstance(home, dict):
            home = home.get("chat_id", "")
        self.home_room = str(home).strip() or (self.rooms[0] if self.rooms else "")
        self.allowed = {x.strip().lower() for x in str(extra.get("allowed_users") or os.environ.get("TINY_ALLOWED_USERS", "")).split(",") if x.strip()}
        require_mention = extra.get("require_mention", os.environ.get("TINY_REQUIRE_MENTION", "true"))
        self.require_mention = str(require_mention).strip().lower() not in {"false", "0", "no", "off"}
        self.rpc: TinyRPC | None = None
        self.pubkey = ""
        self._subscription = "tinyagent"
        self._loop_task: asyncio.Task | None = None
        # request event id -> (room, Hermes key, interaction kind, expiry, option ids)
        self._pending = {}
        self._tracker = None
        self._sent = {}
        self._session_assignees: dict[str, str] = {}
        self._processing: set[str] = set()
        self._reply_routes: dict[str, dict] = {}
        self._turn_routes: dict[str, dict] = {}
        self._turn_route = ContextVar("tinyagent_turn_route", default=None)
        self._lock: IdentityLock | None = None
        self._closed = False
        self._reconnect_task: asyncio.Task | None = None
        self._last_sweep = 0.0
        # request id -> long-task card state, in intake order.
        self._tasks: dict[str, TaskState] = {}

    @property
    def name(self) -> str:
        return "Tiny"

    def set_message_handler(self, handler):
        # Bind routing when Hermes actually starts the turn, not when a queued
        # message arrives. Its public prompt hooks omit reply anchors for Tiny.
        async def routed(event):
            session = self._event_session_key(event)
            route = {**self._reply_routes.get(event.message_id, {}), "session": session}
            token = self._turn_route.set(route)
            self._turn_routes[session] = route
            task = self._tasks.get(event.message_id)
            try:
                response = await handler(event)
            except BaseException as exc:
                # The failure hook fires before Hermes posts its notice; keep the text now.
                if task is not None and not isinstance(exc, asyncio.CancelledError):
                    task.note_error(str(exc) or type(exc).__name__)
                raise
            else:
                if task is not None and isinstance(response, str):
                    task.final_text = response
                return response
            finally:
                if self._turn_routes.get(session) is route:
                    self._turn_routes.pop(session, None)
                self._turn_route.reset(token)
        super().set_message_handler(routed)

    async def _process_message_background(self, event, session_key):
        # Include Hermes' final chunks and attachment sends, which run after the
        # message handler returns. Each queued turn gets its own task context.
        token = self._turn_route.set({**self._reply_routes.get(event.message_id, {}), "session": session_key})
        try:
            return await super()._process_message_background(event, session_key)
        finally:
            self._turn_route.reset(token)

    async def _reply_tags(self, room, reply_to=None, metadata=None):
        metadata = metadata or {}
        current = self._turn_route.get() or {}
        if current.get("room") != room:
            current = {}
        reply = reply_to or metadata.get("reply_to_message_id") or metadata.get("message_id") or current.get("reply_to_message_id")
        if reply and reply != current.get("reply_to_message_id"):
            current = {}
        route = self._reply_routes.get(reply, {})
        if route.get("room") != room:
            route = {}
        thread = metadata.get("thread_id") or route.get("thread_id") or current.get("thread_id")
        root = thread or route.get("root")
        if reply and not root:
            # Synthetic sends may refer to messages received before this process.
            rows = await self.rpc.query({"ids": [reply], "#h": [room], "limit": 1})
            parent = rows[0] if rows else None
            root = (event_thread(parent) if parent else None) or reply
            if parent and parent.get("kind") in (11, 12):
                thread = root
        tags = [["e", str(root), "", "root"]] if root else []
        if reply and reply != root:
            tags.append(["e", str(reply), "", "reply"])
        return tags, 12 if thread else 9

    async def connect(self, *, is_reconnect: bool = False) -> bool:
        """Start the helper and subscribe. Hermes's watcher calls this on a fresh adapter
        with is_reconnect=True; helper death inside a live adapter reuses _establish."""
        self._closed = False
        await self._establish()
        return True

    async def _establish(self) -> None:
        """Spawn a helper, prove the identity, take its lock and subscribe from the cursor."""
        rpc = TinyRPC(self.relay_url, cli_path=self.cli_path, on_exit=self._on_helper_exit)
        try:
            identity = await rpc.call("identity")
            pubkey = str((identity or {}).get("pubkey", ""))
            if not pubkey:
                raise RuntimeError("Tiny identity did not return a public key")
            if self._lock is None or self.pubkey.lower() != pubkey.lower():
                if self._lock is not None:
                    self._lock.release()
                lock = IdentityLock(self.relay_url, pubkey)
                lock.acquire()
                self._lock = lock
            self.pubkey = pubkey
            if self._tracker is None or self._tracker.identity != pubkey.lower():
                self._tracker = EventTracker(self.relay_url, pubkey, self.rooms)
            filter: dict[str, Any] = {"kinds": [9, 11, 12, 1111, taskmod.KIND_JOB_REQUEST, taskmod.KIND_JOB_CANCEL],
                                      **self._tracker.filter_since()}
            if self.rooms:
                filter["#h"] = self.rooms
            await rpc.subscribe(self._subscription, filter, self._on_event)
        except BaseException:
            await rpc.close()
            raise
        previous, self.rpc = self.rpc, rpc
        if previous is not None and previous is not rpc:
            await previous.close()
        self._mark_connected()

    def _on_helper_exit(self, rpc) -> None:
        """TinyRPC reports the helper died on its own; recover in the background."""
        if self._closed or rpc is not self.rpc:
            return
        logger.warning("Tiny helper exited; reconnecting")
        self._mark_disconnected()
        if self._reconnect_task is None or self._reconnect_task.done():
            self._reconnect_task = asyncio.create_task(self._reconnect_loop())

    async def _reconnect_loop(self) -> None:
        """Bounded exponential backoff until the helper is back or the adapter closes.

        The adapter stays installed in Hermes throughout, so process-local waiters and
        the identity lock survive; the gateway's reconnect watcher only builds a new
        adapter for fatal errors, which this path never raises.
        """
        delay = self.RECONNECT_MIN
        while not self._closed:
            await asyncio.sleep(delay)
            if self._closed:
                return
            try:
                await self._establish()
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                delay = min(delay * 2, self.RECONNECT_MAX)
                logger.warning("Tiny helper reconnect failed: %s; retrying in %.0fs", exc, delay)
                continue
            logger.info("Tiny helper reconnected from cursor %s", self._tracker.cursor if self._tracker else "?")
            return

    async def disconnect(self) -> None:
        self._closed = True
        task, self._reconnect_task = self._reconnect_task, None
        if task is not None and task is not asyncio.current_task():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        rpc, self.rpc = self.rpc, None
        if rpc is not None:
            if rpc.alive:
                await asyncio.gather(rpc.unsubscribe(self._subscription), return_exceptions=True)
            await rpc.close()
        if self._lock is not None:
            self._lock.release()
            self._lock = None
        for task in self._tasks.values():
            self._stop_progress_timer(task)
        self._mark_disconnected()

    async def _on_event(self, event: dict) -> None:
        event_id = event.get("id") if isinstance(event, dict) else None
        if event_id in self._processing:
            return
        self._processing.add(event_id)
        now = time.monotonic()
        if now - self._last_sweep >= self.SWEEP_INTERVAL:
            self._last_sweep = now
            self._sweep_pending()
        try:
            await self._receive_event(event)
        finally:
            self._processing.discard(event_id)

    async def _receive_event(self, event: dict) -> None:
        if not isinstance(event, dict) or event.get("pubkey", "").lower() == self.pubkey.lower():
            return
        if event.get("kind") in (1111, 7):
            await self._on_answer(event)
            return
        room = _tag(event, "h")
        author = str(event.get("pubkey", ""))
        if not room or (self.rooms and room not in self.rooms) or not self._authorized(author):
            return
        if self._tracker and not self._tracker.should_accept(event, now=int(time.time())):
            return
        kind = event.get("kind")
        if kind == taskmod.KIND_JOB_REQUEST:
            await self._on_task_request(event, room, author)
            return
        if kind == taskmod.KIND_JOB_CANCEL:
            await self._on_task_cancel(event, author)
            if self._tracker:
                self._tracker.mark(event)
            return
        if kind in (9, 12) and self._is_cancel_command(event):
            await self._on_cancel_command(event, room, author)
            if self._tracker:
                self._tracker.mark(event)
            return
        root = event_thread(event)
        parent_id = reply_parent(event)
        parent = None
        mentioned = self.pubkey in [row[1] for row in _tags(event, "p")]
        if parent_id:
            rows = await self.rpc.query({"ids": [parent_id], "#h": [room], "limit": 1})
            parent = rows[0] if rows else None
        own_reply = bool(parent and parent.get("pubkey") == self.pubkey)
        if self.require_mention and not mentioned and not own_reply:
            return
        event_id = str(event.get("id", ""))
        # Kind 11/12 carry Tiny thread sessions. Kind 9 remains the room session even when
        # it carries an e tag, matching Hermes' room routing contract.
        thread_id = event_id if event.get("kind") == 11 else root if event.get("kind") == 12 else None
        source = self.build_source(chat_id=room, chat_name=room, chat_type="group", user_id=author,
                                   thread_id=thread_id, message_id=event_id)
        media_urls, media_types = await incoming_media(self.rpc, event, self.relay_url)
        message_type = MessageType.TEXT
        if media_types:
            major = media_types[0].split("/", 1)[0]
            message_type = {"image": MessageType.PHOTO, "audio": MessageType.AUDIO,
                            "video": MessageType.VIDEO}.get(major, MessageType.DOCUMENT)
        message = MessageEvent(text=str(event.get("content", "")), message_type=message_type, source=source,
                               raw_message=event, message_id=event_id,
                               media_urls=media_urls, media_types=media_types,
                               media_text_inlined=[False] * len(media_urls),
                               timestamp=datetime.fromtimestamp(int(event.get("created_at", time.time()))),
                               reply_to_message_id=parent_id, reply_to_is_own_message=own_reply,
                               reply_to_text=parent.get("content") if parent else None)
        self._session_assignees[self._event_session_key(message)] = author
        self._reply_routes[event_id] = {"room": room, "root": root or event_id,
                                         "reply_to_message_id": event_id, "thread_id": thread_id, "user_id": author}
        if len(self._reply_routes) > 10000:
            self._reply_routes.pop(next(iter(self._reply_routes)))
        await self._deliver(message, event)

    async def _deliver(self, message: MessageEvent, event: dict) -> None:
        """Hand a turn to Hermes. The event counts as delivered only once Hermes has
        accepted it; a failed hand-off leaves it eligible for redelivery until the
        attempt budget is spent."""
        event_id = str(event.get("id", ""))
        try:
            await self.handle_message(message)
        except BaseException as exc:
            if self._tracker and not isinstance(exc, asyncio.CancelledError):
                if self._tracker.fail(event):
                    logger.error("Tiny event %s retired after repeated failures", event_id, exc_info=exc)
            raise
        if self._tracker:
            self._tracker.mark(event)

    # -- long-task cards -------------------------------------------------

    async def _on_task_request(self, event: dict, room: str, author: str) -> None:
        """Turn a 43001 that asks this key into a thread turn anchored at the request."""
        request_id = str(event.get("id", ""))
        if self.pubkey not in [row[1] for row in _tags(event, "p")]:
            if self._tracker:
                self._tracker.mark(event)
            return
        # Restart safety: a request this key already finished (or the requester
        # cancelled) is not run again.
        done = await self.rpc.query({"kinds": list(taskmod.TERMINAL_KINDS), "#e": [request_id],
                                     "#h": [room], "limit": 1})
        if done or request_id in self._tasks:
            if self._tracker:
                self._tracker.mark(event)
            return
        root = request_root(event)
        source = self.build_source(chat_id=room, chat_name=room, chat_type="group", user_id=author,
                                   thread_id=root, message_id=request_id)
        message = MessageEvent(text=request_text(event), message_type=MessageType.TEXT, source=source,
                               raw_message=event, message_id=request_id,
                               timestamp=datetime.fromtimestamp(int(event.get("created_at", time.time()))))
        session = self._event_session_key(message)
        task = TaskState(request_id, room, author, root, session)
        self._tasks[request_id] = task
        while len(self._tasks) > self.MAX_TASKS:
            self._forget_task(next(iter(self._tasks)))
        self._session_assignees[session] = author
        self._reply_routes[request_id] = {"room": room, "root": root, "reply_to_message_id": request_id,
                                          "thread_id": root, "user_id": author}
        try:
            await self._deliver(message, event)
        except BaseException:
            self._forget_task(request_id)
            raise

    def _forget_task(self, request_id: str) -> None:
        task = self._tasks.pop(request_id, None)
        if task is not None:
            self._stop_progress_timer(task)

    def _task_for_send(self, room: str, tags: list[list[str]]) -> TaskState | None:
        """Return the running task whose thread a send lands in, if any."""
        root = next((row[1] for row in tags if len(row) >= 4 and row[0] == "e" and row[3] == "root"), None)
        if not root:
            return None
        matches = [task for task in self._tasks.values()
                   if not task.terminal and task.room == room and task.thread_root == root]
        return next((task for task in matches if task.accepted), matches[0] if matches else None)

    def _task_for_message(self, message_id: str) -> TaskState | None:
        return next((task for task in self._tasks.values()
                     if not task.terminal and message_id in task.classes), None)

    def _task_for_session(self, session: str) -> TaskState | None:
        matches = [task for task in self._tasks.values() if not task.terminal and task.session_key == session]
        return next((task for task in matches if task.accepted), matches[0] if matches else None)

    def _observe(self, task: TaskState | None, message_id: str | None, content: str, cls: str, *,
                 final: bool = False, file_name: str | None = None) -> None:
        if task is None or not message_id:
            return
        task.observe(message_id, content, cls, final=final, file_name=file_name)
        self._schedule_progress(task)

    def _clock(self) -> float:
        return time.monotonic()

    def _schedule_progress(self, task: TaskState) -> None:
        """Publish pending progress now, or once the throttle window has passed."""
        if task.terminal or not task.accepted or not task.has_progress or task.timer is not None:
            return
        delay = task.progress_due(self._clock(), self.PROGRESS_INTERVAL)
        if delay <= 0:
            task.timer = asyncio.ensure_future(self._flush_progress(task.request_id))
            return
        loop = asyncio.get_running_loop()
        task.timer = loop.call_later(delay, self._progress_timer_fired, task.request_id)

    def _progress_timer_fired(self, request_id: str) -> None:
        task = self._tasks.get(request_id)
        if task is None:
            return
        task.timer = asyncio.ensure_future(self._flush_progress(request_id))

    def _stop_progress_timer(self, task: TaskState) -> None:
        timer, task.timer = task.timer, None
        try:
            current = asyncio.current_task()
        except RuntimeError:
            current = None
        if timer is not None and timer is not current:
            timer.cancel()

    async def _stop_task_turn(self, task: TaskState) -> None:
        """Stop the Hermes turn of a task: cancel it when it runs, drop it when still queued."""
        if task.accepted:
            await self.cancel_session_processing(task.session_key)
            return
        pending = getattr(self, "_pending_messages", {}).get(task.session_key)
        if pending is not None and str(getattr(pending, "message_id", "")) == task.request_id:
            self._pending_messages.pop(task.session_key, None)

    async def _flush_progress(self, request_id: str) -> None:
        task = self._tasks.get(request_id)
        if task is None:
            return
        if task.timer is not None and task.timer is not asyncio.current_task():
            task.timer.cancel()
        try:
            if task.terminal or not task.has_progress:
                return
            content = task.take_progress(self._clock())
            result = await self._publish(task.room, content, answer_tags(task), kind=taskmod.KIND_JOB_PROGRESS)
            if not result.success:
                logger.warning("Tiny task %s progress failed: %s", request_id[:8], result.error)
        finally:
            task.timer = None
            if task.has_progress:
                self._schedule_progress(task)

    async def _publish_terminal(self, task: TaskState, kind: int, content: str, extra: list[list[str]] = ()) -> None:
        """Publish exactly one terminal event for a task, whatever happens to the send."""
        if not task.end():
            return
        self._stop_progress_timer(task)
        result = await self._publish(task.room, content, [*answer_tags(task), *extra], kind=kind)
        if not result.success:
            logger.error("Tiny task %s terminal event %d failed: %s", task.request_id[:8], kind, result.error)
        self._forget_task(task.request_id)

    async def on_processing_start(self, event: MessageEvent) -> None:
        await super().on_processing_start(event)
        task = self._tasks.get(str(event.message_id or ""))
        if task is None or task.terminal or task.accepted:
            return
        task.accepted = True
        result = await self._publish(task.room, "", answer_tags(task), kind=taskmod.KIND_JOB_ACCEPTED)
        if not result.success:
            logger.warning("Tiny task %s accepted event failed: %s", task.request_id[:8], result.error)

    async def on_processing_complete(self, event: MessageEvent, outcome) -> None:
        await super().on_processing_complete(event, outcome)
        task = self._tasks.get(str(event.message_id or ""))
        if task is None:
            return
        name = str(getattr(outcome, "value", outcome)).lower()
        if task.terminal:
            self._forget_task(task.request_id)
            return
        if name == "success":
            content, extra = task.result()
            await self._publish_terminal(task, taskmod.KIND_JOB_RESULT, content, extra)
        elif name == "cancelled":
            await self._publish_terminal(task, taskmod.KIND_JOB_ERROR, "Cancelled")
        else:
            await self._publish_terminal(task, taskmod.KIND_JOB_ERROR, task.failure())

    async def _on_task_cancel(self, event: dict, author: str) -> None:
        """A 43005 from the requester ends the task; the cancel itself is the terminal event."""
        task = self._tasks.get(str(_tag(event, "e") or ""))
        if task is None or task.terminal or author != task.requester:
            return
        task.end()
        self._stop_progress_timer(task)
        await self._stop_task_turn(task)
        self._forget_task(task.request_id)

    def _is_cancel_command(self, event: dict) -> bool:
        return (str(event.get("content", "")).strip() == CANCEL_COMMAND
                and self.pubkey in [row[1] for row in _tags(event, "p")])

    async def _on_cancel_command(self, event: dict, room: str, author: str) -> None:
        """Buzz's !cancel: stop the current turn of the room or thread session instead of
        forwarding it. A task turn ends with a 43006 naming who cancelled it."""
        event_id = str(event.get("id", ""))
        thread_id = event_thread(event) if event.get("kind") == 12 else None
        source = self.build_source(chat_id=room, chat_name=room, chat_type="group", user_id=author,
                                   thread_id=thread_id, message_id=event_id)
        session = self._event_session_key(MessageEvent(text=CANCEL_COMMAND, source=source, message_id=event_id))
        task = self._task_for_session(session)
        if task is None:
            root = event_thread(event)
            if root:
                task = next((entry for entry in self._tasks.values()
                             if not entry.terminal and entry.room == room and entry.thread_root == root), None)
            elif session not in getattr(self, "_active_sessions", {}):
                # A bare room-level !cancel with no turn of its own stops the room's newest task.
                running = [entry for entry in self._tasks.values()
                           if not entry.terminal and entry.room == room and entry.accepted]
                task = running[-1] if running else None
        if task is not None:
            await self._publish_terminal(task, taskmod.KIND_JOB_ERROR, f"Cancelled by {author}")
            await self._stop_task_turn(task)
            return
        await self.cancel_session_processing(session)

    async def create_handoff_thread(self, parent_chat_id: str, name: str) -> str | None:
        """Open a real thread in the room so a handed-off session continues there."""
        result = await self._publish(parent_chat_id, str(name or "").strip() or "Handoff", [], kind=11)
        if not result.success or not result.message_id:
            logger.warning("Tiny handoff thread failed: %s", result.error)
            return None
        return str(result.message_id)

    def _authorized(self, author: str) -> bool:
        return author.lower() in self.allowed

    @staticmethod
    def _mentions(content: str) -> list[str]:
        return _explicit_mentions(content)

    async def _publish(self, room: str, content: str, tags: list[list[str]], *, kind: int = 9) -> SendResult:
        if not self.rpc:
            return SendResult(success=False, error="Tiny is disconnected", retryable=True)
        try:
            result = await self.rpc.publish({"kind": kind, "content": content, "tags": [["h", room], *tags]})
            event = result if isinstance(result, dict) else {}
            return SendResult(success=True, message_id=event.get("id"), raw_response=event)
        except Exception as exc:
            return SendResult(success=False, error=str(exc), retryable=True)

    async def send(self, chat_id: str, content: str, reply_to: str | None = None, metadata: dict | None = None) -> SendResult:
        tags, kind = await self._reply_tags(chat_id, reply_to, metadata)
        mentions = _explicit_mentions(content)
        for pubkey in (metadata or {}).get("mentions", []):
            if _HEX.fullmatch(str(pubkey)):
                mentions.append(str(pubkey).lower())
        for pubkey in dict.fromkeys(mentions):
            tags.append(["p", pubkey])
            tags.append(["mention", pubkey])
        result = await self._publish(chat_id, content, tags, kind=kind)
        task = self._task_for_send(chat_id, tags)
        if result.success:
            self._sent[result.message_id] = tags
            if len(self._sent) > 1000:
                self._sent.pop(next(iter(self._sent)))
            # Hermes marks a streamed draft with expect_edits and a final reply with notify;
            # anything else in the thread is tool chrome or a status line.
            meta = metadata or {}
            final = bool(meta.get("notify"))
            cls = taskmod.TEXT if final or meta.get("expect_edits") else taskmod.CHROME
            self._observe(task, result.message_id, content, cls, final=final)
        elif task is not None:
            task.note_error(result.error or "Tiny send failed")
        return result

    async def edit_message(self, chat_id: str, message_id: str, content: str, *, finalize: bool = False) -> SendResult:
        tags = [["e", message_id]]
        for key in _explicit_mentions(content):
            tags.extend([["p", key], ["mention", key]])
        result = await self._publish(chat_id, content, tags, kind=40003)
        task = self._task_for_message(str(message_id))
        if result.success:
            result.message_id = message_id
            self._observe(task, str(message_id), content, taskmod.CHROME, final=finalize)
        elif task is not None:
            task.note_error(result.error or "Tiny edit failed")
        return result

    async def delete_message(self, chat_id: str, message_id: str) -> bool:
        # Tiny's room deletion is a moderator action; this member connector cannot claim deletion.
        return False

    async def _send_file(self, chat_id: str, file_path: str, caption: str | None = None,
                         media_type: str | None = None, metadata: dict | None = None,
                         reply_to: str | None = None, file_name: str | None = None) -> SendResult:
        if not self.rpc:
            return SendResult(success=False, error="Tiny is disconnected", retryable=True)
        try:
            path = Path(file_path)
            if not 0 < path.stat().st_size <= 32 << 20:
                return SendResult(success=False, error="Tiny attachments must be between 1 byte and 32 MiB")
            data = await asyncio.to_thread(path.read_bytes)
            mime = media_type or mimetypes.guess_type(path.name)[0] or "application/octet-stream"
            filename = Path(file_name).name if file_name else path.name
            descriptor = await self.rpc.upload(chat_id, filename, mime, data)
            target = descriptor.get("url") or descriptor.get("path") or descriptor.get("sha256")
            if not target:
                return SendResult(success=False, error="Tiny upload returned no descriptor")
            digest = descriptor.get("sha256") or descriptor.get("hash")
            if digest != hashlib.sha256(data).hexdigest():
                return SendResult(success=False, error="Tiny returned a different attachment hash")
            fields = ["url " + str(target), "m " + mime, "filename " + filename]
            if digest:
                fields.append("x " + str(digest))
            fields.append("size " + str(len(data)))
            tags, kind = await self._reply_tags(chat_id, reply_to, metadata)
            tags.append(["imeta", *fields])
            for key in _explicit_mentions(caption or ""):
                tags.extend([["p", key], ["mention", key]])
            result = await self._publish(chat_id, caption or "", tags, kind=kind)
            task = self._task_for_send(chat_id, tags)
            if result.success:
                self._observe(task, result.message_id, caption or "", taskmod.FILE, file_name=filename)
            elif task is not None:
                task.note_error(result.error or "Tiny upload failed")
            return result
        except Exception as exc:
            return SendResult(success=False, error=str(exc), retryable=True)

    async def send_image_file(self, chat_id: str, image_path: str, caption: str | None = None,
                              reply_to=None, metadata=None, **kwargs) -> SendResult:
        return await self._send_file(chat_id, image_path, caption,
                                     mimetypes.guess_type(image_path)[0] or "image/png", metadata, reply_to)

    async def send_document(self, chat_id: str, file_path: str, caption: str | None = None,
                            file_name=None, reply_to=None, metadata=None, **kwargs) -> SendResult:
        return await self._send_file(chat_id, file_path, caption, metadata=metadata, reply_to=reply_to, file_name=file_name)

    async def send_video(self, chat_id: str, video_path: str, caption: str | None = None,
                         reply_to=None, metadata=None, **kwargs) -> SendResult:
        return await self._send_file(chat_id, video_path, caption, metadata=metadata, reply_to=reply_to)

    async def send_voice(self, chat_id: str, audio_path: str, caption: str | None = None,
                         reply_to=None, metadata=None, **kwargs) -> SendResult:
        return await self._send_file(chat_id, audio_path, caption,
                                     mimetypes.guess_type(audio_path)[0] or "audio/ogg", metadata, reply_to)

    async def send_typing(self, chat_id: str, metadata=None) -> None:
        await self._publish(chat_id, "", [["expiration", str(int(time.time()) + 30)], ["status", "typing"]], kind=20002)

    async def get_chat_info(self, chat_id: str) -> dict[str, Any]:
        return {"name": chat_id, "type": "group"}


def check_requirements() -> bool:
    return bool(os.environ.get("TINY_RELAY_URL") and os.environ.get("TINY_PRIVATE_KEY"))


def validate_config(config: PlatformConfig) -> bool:
    extra = config.extra or {}
    return bool(extra.get("relay_url") or os.environ.get("TINY_RELAY_URL"))


def _env_enablement() -> dict | None:
    relay = os.environ.get("TINY_RELAY_URL", "").strip()
    key = os.environ.get("TINY_PRIVATE_KEY", "").strip()
    if not relay or not key:
        return None
    result = {"relay_url": relay}
    for env, key_name in (("TINY_ALLOWED_USERS", "allowed_users"), ("TINY_CLI_PATH", "cli_path"),
                          ("TINY_REQUIRE_MENTION", "require_mention")):
        if os.environ.get(env):
            result[key_name] = os.environ[env]
    home = os.environ.get("TINY_HOME_CHANNEL", "").strip()
    if home:
        # Hermes lifts this dict into a HomeChannel for cron and standalone delivery.
        result["home_channel"] = {"chat_id": home, "name": "Home"}
    return result


async def _standalone_send(pconfig, chat_id: str, message: str, *, thread_id=None, media_files=None,
                           force_document=False):
    extra = getattr(pconfig, "extra", {}) or {}
    rpc = TinyRPC(str(extra.get("relay_url") or os.environ.get("TINY_RELAY_URL", "")),
                  cli_path=str(extra.get("cli_path") or os.environ.get("TINY_CLI_PATH", "tinyagent")))
    try:
        await rpc.connect()
        adapter = TinyAdapter(pconfig)
        adapter.rpc = rpc
        media = list(media_files or [])
        last_id = None
        if message and len(media) != 1:
            result = await adapter.send(chat_id, message, metadata={"thread_id": thread_id})
            if not result.success:
                return {"error": result.error or "Tiny send failed"}
            last_id = result.message_id
        for item in media:
            path = str(item[0] if isinstance(item, (list, tuple)) else item)
            is_voice = bool(item[1]) if isinstance(item, (list, tuple)) and len(item) > 1 else False
            mime = mimetypes.guess_type(path)[0] or ("audio/ogg" if is_voice else "application/octet-stream")
            if force_document:
                mime = "application/octet-stream"
            result = await adapter._send_file(chat_id, path, message if len(media) == 1 else None,
                                              mime, {"thread_id": thread_id})
            if not result.success:
                return {"error": result.error or "Tiny media send failed"}
            last_id = result.message_id or last_id
        return {"success": True, "message_id": last_id, "media_delivered": bool(media)}
    except Exception as exc:
        return {"error": str(exc)}
    finally:
        await rpc.close()


def register(ctx):
    ctx.register_platform(
        name="tiny", label="Tiny", adapter_factory=TinyAdapter,
        check_fn=check_requirements, validate_config=validate_config,
        required_env=["TINY_RELAY_URL", "TINY_PRIVATE_KEY"],
        install_hint="Requires the tinyagent helper binary on PATH or at TINY_CLI_PATH",
        env_enablement_fn=_env_enablement, cron_deliver_env_var="TINY_HOME_CHANNEL",
        standalone_sender_fn=_standalone_send, allowed_users_env="TINY_ALLOWED_USERS",
        max_message_length=12000, platform_hint="You are collaborating through Tinyrelay. Only explicit @ mentions notify a person. Use @ followed by their 64-character public key when you need their attention; ordinary updates stay quiet.",
        emoji="◈", pii_safe=False, allow_update_command=True,
    )
