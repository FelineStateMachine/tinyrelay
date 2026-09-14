"""Native Tinyrelay adapter for Hermes Agent.

Tiny is a Nostr relay connection. The adapter intentionally uses structured events for
interactive prompts; no Buzz compatibility or prose parsing is involved.
"""

from __future__ import annotations

import asyncio
import json
import mimetypes
import hashlib
import os
import re
import time
from datetime import datetime
from pathlib import Path
from typing import Any

from gateway.config import Platform, PlatformConfig
from gateway.platforms.base import BasePlatformAdapter, ExecApprovalPrompt, SendResult
from gateway.platforms.event import MessageEvent, MessageType

try:
    from .client import TinyRPC
    from .interactions import InteractionMixin
    from .media import incoming_media
    from .ingress import EventTracker, event_thread, reply_parent
except ImportError:  # Hermes plugin loader may import adapter.py as a top-level module.
    from client import TinyRPC
    from interactions import InteractionMixin
    from media import incoming_media
    from ingress import EventTracker, event_thread, reply_parent

_HEX = re.compile(r"\b[0-9a-f]{64}\b", re.I)
_MENTION = re.compile(r"(?<![0-9A-Za-z])@([0-9a-f]{64})(?![0-9A-Za-z])", re.I)


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

    def __init__(self, config: PlatformConfig):
        super().__init__(config, Platform("tiny"))
        extra = config.extra or {}
        self.relay_url = str(extra.get("relay_url") or os.environ.get("TINY_RELAY_URL", "")).strip()
        self.cli_path = str(extra.get("cli_path") or os.environ.get("TINY_CLI_PATH", "tinyagent"))
        self.rooms = [x.strip() for x in str(extra.get("rooms") or extra.get("channels") or os.environ.get("TINY_CHANNELS", "")).split(",") if x.strip()]
        home = extra.get("home_channel") or os.environ.get("TINY_HOME_CHANNEL", "")
        self.home_room = str(home).strip() or (self.rooms[0] if self.rooms else "")
        self.allowed = {x.strip().lower() for x in str(extra.get("allowed_users") or os.environ.get("TINY_ALLOWED_USERS", "")).split(",") if x.strip()}
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

    @property
    def name(self) -> str:
        return "Tiny"

    async def connect(self, *, is_reconnect: bool = False) -> bool:
        self.rpc = TinyRPC(self.relay_url, cli_path=self.cli_path)
        identity = await self.rpc.call("identity")
        self.pubkey = str(identity.get("pubkey", ""))
        if not self.pubkey:
            raise RuntimeError("Tiny identity did not return a public key")
        self._tracker = EventTracker(self.relay_url, self.pubkey, self.rooms)
        filter: dict[str, Any] = {"kinds": [9, 11, 12, 1111], **self._tracker.filter_since()}
        if self.rooms:
            filter["#h"] = self.rooms
        await self.rpc.subscribe(self._subscription, filter, self._on_event)
        self._mark_connected()
        return True

    async def disconnect(self) -> None:
        if self.rpc:
            with_context = self.rpc.unsubscribe(self._subscription)
            await asyncio.gather(with_context, return_exceptions=True)
            await self.rpc.close()
        self.rpc = None
        self._mark_disconnected()

    async def _on_event(self, event: dict) -> None:
        event_id = event.get("id") if isinstance(event, dict) else None
        if event_id in self._processing:
            return
        self._processing.add(event_id)
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
        root = event_thread(event)
        parent_id = reply_parent(event)
        parent = None
        mentioned = self.pubkey in [row[1] for row in _tags(event, "p")]
        if parent_id:
            rows = await self.rpc.query({"ids": [parent_id], "#h": [room], "limit": 1})
            parent = rows[0] if rows else None
        own_reply = bool(parent and parent.get("pubkey") == self.pubkey)
        if not mentioned and not own_reply:
            return
        event_id = str(event.get("id", ""))
        # Only thread kinds create a separate thread session. Kind 9 replies stay in the room.
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
        if self._tracker:
            self._tracker.mark(event)
        await self.handle_message(message)

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
        tags: list[list[str]] = []
        if reply_to:
            tags.append(["e", str(reply_to)])
        mentions = _explicit_mentions(content)
        for pubkey in (metadata or {}).get("mentions", []):
            if _HEX.fullmatch(str(pubkey)):
                mentions.append(str(pubkey).lower())
        for pubkey in dict.fromkeys(mentions):
            tags.append(["p", pubkey])
            tags.append(["mention", pubkey])
        thread_id = (metadata or {}).get("thread_id")
        if thread_id:
            tags.insert(0, ["e", str(thread_id), "", "root"])
        result = await self._publish(chat_id, content, tags, kind=12 if thread_id else 9)
        if result.success:
            self._sent[result.message_id] = tags
            if len(self._sent) > 1000:
                self._sent.pop(next(iter(self._sent)))
        return result

    async def edit_message(self, chat_id: str, message_id: str, content: str, *, finalize: bool = False) -> SendResult:
        tags = [["e", message_id]]
        for key in _explicit_mentions(content):
            tags.extend([["p", key], ["mention", key]])
        result = await self._publish(chat_id, content, tags, kind=40003)
        if result.success:
            result.message_id = message_id
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
            tags = [["imeta", *fields]]
            thread_id = (metadata or {}).get("thread_id")
            if thread_id:
                tags.insert(0, ["e", str(thread_id), "", "root"])
            if reply_to:
                tags.append(["e", str(reply_to), "", "reply"])
            for key in _explicit_mentions(caption or ""):
                tags.extend([["p", key], ["mention", key]])
            return await self._publish(chat_id, caption or "", tags, kind=12 if thread_id else 9)
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
    for env, key_name in (("TINY_HOME_CHANNEL", "home_channel"), ("TINY_ALLOWED_USERS", "allowed_users"),
                           ("TINY_CLI_PATH", "cli_path")):
        if os.environ.get(env):
            result[key_name] = os.environ[env]
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
