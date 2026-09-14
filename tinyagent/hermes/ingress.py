"""Replay-safe ingress bookkeeping for Tiny's Nostr event stream.

This module has no Hermes or network dependencies. State is scoped to the relay,
agent identity and subscribed rooms, so reconnects cannot replay old commands.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import tempfile
import time
from pathlib import Path
from typing import Iterable

_EVENT_ID = re.compile(r"^[0-9a-f]{64}$", re.I)
_REPLAY_WINDOW = 300
_FUTURE_SKEW = 300


def _tag_values(event: dict, name: str) -> list[list[str]]:
    return [tag for tag in event.get("tags", []) if isinstance(tag, list) and len(tag) >= 2 and tag[0] == name]


def _expiration(event: dict) -> int | None:
    values = _tag_values(event, "expiration")
    if not values:
        return None
    try:
        return int(values[0][1])
    except (TypeError, ValueError):
        return None


class EventTracker:
    """Bounded persistent event cursor and ID set for one Tiny subscription."""

    def __init__(self, relay: str, identity: str, rooms: Iterable[str], *, state_dir: str | Path | None = None,
                 now: int | None = None, max_seen: int = 10000):
        self.relay, self.identity = str(relay), str(identity).lower()
        self.rooms = tuple(sorted(str(room) for room in rooms))
        root = Path(state_dir or os.environ.get("HERMES_HOME", "~/.hermes")).expanduser()
        key = hashlib.sha256((self.relay + "\0" + self.identity + "\0" + "\0".join(self.rooms)).encode()).hexdigest()[:24]
        self.path = root / "state" / "tinyagent" / f"{key}.json"
        self.max_seen = max(1, int(max_seen))
        self.now = int(time.time() if now is None else now)
        self.floor = self.now
        self.cursor = self.now
        self.seen: list[str] = []
        self._seen: set[str] = set()
        self._load()
        # Establish the first-start floor immediately, including a clean restart before
        # any event arrives. This prevents a later process from treating its own startup
        # time as an advancing cursor.
        self._save()

    def _load(self) -> None:
        try:
            data = json.loads(self.path.read_text())
            self.floor = int(data.get("floor", data.get("cursor", self.floor)))
            self.cursor = int(data.get("cursor", self.cursor))
            self.seen = [str(value) for value in data.get("seen", [])][-self.max_seen:]
            self._seen = set(self.seen)
        except (OSError, ValueError, TypeError):
            return

    def _save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        os.chmod(self.path.parent, 0o700)
        payload = json.dumps({"floor": self.floor, "cursor": self.cursor, "seen": self.seen[-self.max_seen:]}, separators=(",", ":"))
        fd, temporary = tempfile.mkstemp(prefix="tinyagent-", dir=self.path.parent)
        try:
            with os.fdopen(fd, "w") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.path)
        finally:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass

    def should_accept(self, event: dict, *, now: int | None = None) -> bool:
        """Return true only for unseen, non-expired events at/after the cursor."""
        event_id = str(event.get("id", ""))
        if not _EVENT_ID.fullmatch(event_id) or event_id in self._seen:
            return False
        try:
            created = int(event.get("created_at"))
        except (TypeError, ValueError):
            return False
        if created < max(self.floor, self.cursor - _REPLAY_WINDOW):
            return False
        if created > int(time.time() if now is None else now) + _FUTURE_SKEW:
            return False
        expires = _expiration(event)
        if expires is None and any(len(tag) >= 2 and tag[0] == "expiration" for tag in event.get("tags", [])):
            return False
        if expires is not None and expires <= int(time.time() if now is None else now):
            return False
        return True

    def mark(self, event: dict) -> None:
        """Record a delivered event and atomically persist the cursor."""
        event_id = str(event.get("id", ""))
        if not _EVENT_ID.fullmatch(event_id):
            return
        try:
            self.cursor = max(self.cursor, int(event.get("created_at", self.cursor)))
        except (TypeError, ValueError):
            pass
        if event_id not in self._seen:
            self._seen.add(event_id)
            self.seen.append(event_id)
            if len(self.seen) > self.max_seen:
                # Retire the overlap instead of forgetting IDs still eligible for replay.
                self.floor = self.cursor + 1
                self._seen = set(self.seen[-self.max_seen:])
                self.seen = self.seen[-self.max_seen:]
        self._save()

    def filter_since(self) -> dict:
        """Return an overlapping subscription filter; same-timestamp IDs are retained."""
        # Match should_accept's bounded overlap. A one-second overlap is insufficient
        # when the helper returns events out of order or the gateway was offline.
        result = {"since": max(self.floor, self.cursor - _REPLAY_WINDOW)}
        if self.rooms:
            result["#h"] = list(self.rooms)
        return result


def event_thread(event: dict) -> str | None:
    """Return canonical NIP-10 root thread ID, including marked e tags."""
    marked_root = [tag[1] for tag in _tag_values(event, "e") if len(tag) >= 4 and tag[3] == "root"]
    if marked_root:
        return marked_root[0]
    marked_reply = [tag[1] for tag in _tag_values(event, "e") if len(tag) >= 4 and tag[3] == "reply"]
    if marked_reply:
        return marked_reply[0]
    e_tags = _tag_values(event, "e")
    return e_tags[0][1] if e_tags else None


def reply_parent(event: dict) -> str | None:
    """Return the direct NIP-10 parent, preferring the marked reply e tag."""
    marked = [tag[1] for tag in _tag_values(event, "e") if len(tag) >= 4 and tag[3] == "reply"]
    return marked[0] if marked else event_thread(event)


def is_reply_to_own(event: dict, own_ids: set[str]) -> bool:
    """True when the event's canonical parent or root is one of our event IDs."""
    return any(tag[1] in own_ids for tag in _tag_values(event, "e"))


__all__ = ["EventTracker", "event_thread", "reply_parent", "is_reply_to_own"]
