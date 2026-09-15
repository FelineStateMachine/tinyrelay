"""Replay-safe ingress bookkeeping for Tiny's Nostr event stream.

This module has no Hermes or network dependencies. State is scoped to the relay,
agent identity and subscribed rooms, so reconnects cannot replay old commands.
"""

from __future__ import annotations

import fcntl
import hashlib
import json
import logging
import os
import re
import tempfile
import time
from pathlib import Path
from typing import Iterable

_EVENT_ID = re.compile(r"^[0-9a-f]{64}$", re.I)
_REPLAY_WINDOW = 300
_FUTURE_SKEW = 300
# Hard ceiling on remembered IDs, far above anything the replay window holds in practice.
MAX_SEEN = 100000
# A poison event is retired after this many failed hand-offs to Hermes.
MAX_ATTEMPTS = 3
_MAX_ATTEMPT_ENTRIES = 256

logger = logging.getLogger(__name__)


def state_root(state_dir: str | Path | None = None) -> Path:
    """Return the Hermes home used for adapter state.

    The active Hermes home wins when the CLI package is importable (profiles may
    override the environment); otherwise HERMES_HOME, then ~/.hermes.
    """
    if state_dir:
        return Path(state_dir).expanduser()
    try:
        from hermes_cli.config import get_hermes_home
        return Path(get_hermes_home()).expanduser()
    except Exception:
        return Path(os.environ.get("HERMES_HOME", "").strip() or "~/.hermes").expanduser()


def adapter_state_dir(root: str | Path | None = None) -> Path:
    """Return the directory holding tracker state for the active Hermes home."""
    return state_root(root) / "state" / "tinyagent"


def shared_root() -> Path:
    """Return the Hermes root every profile shares: the parent of ``profiles`` when the
    process home is a named profile, else the process home itself."""
    try:
        from hermes_constants import get_default_hermes_root
        return Path(get_default_hermes_root()).expanduser()
    except Exception:
        home = Path(os.environ.get("HERMES_HOME", "").strip() or "~/.hermes").expanduser()
        return home.parent.parent if home.parent.name == "profiles" else home


def lock_dir(root: str | Path | None = None) -> Path:
    """Return the directory holding identity locks.

    Locks live under the shared Hermes root so two profiles cannot run the same
    identity. When that root cannot be written, the system temporary directory
    holds them instead.
    """
    if root:
        return adapter_state_dir(root) / "locks"
    candidate = shared_root() / "state" / "tinyagent" / "locks"
    try:
        candidate.mkdir(parents=True, exist_ok=True)
        os.chmod(candidate, 0o700)
        if os.access(candidate, os.W_OK):
            return candidate
    except OSError:
        pass
    fallback = Path(tempfile.gettempdir()) / "tinyagent-locks"
    fallback.mkdir(parents=True, exist_ok=True)
    logger.warning("Tiny identity locks use %s because %s is not writable", fallback, candidate)
    return fallback


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
    """Persistent event cursor and ID set for one Tiny subscription.

    State file (JSON): ``floor`` and ``cursor`` are Unix seconds; ``seen`` maps a
    delivered event ID to its ``created_at``; ``attempts`` maps an event ID to the
    number of failed hand-offs. Older files stored ``seen`` as a list of IDs and
    load with every listed ID pinned to the cursor.

    ``seen`` keeps every ID inside the replay window (``cursor - 300`` seconds, never
    below ``floor``); the window is what bounds correctness, so a burst of events in
    one second is never forgotten. ``max_seen`` is only a hard ceiling: when it is hit
    the oldest seconds are retired, the floor moves past them and a warning is logged.
    """

    def __init__(self, relay: str, identity: str, rooms: Iterable[str], *, state_dir: str | Path | None = None,
                 now: int | None = None, max_seen: int = MAX_SEEN):
        self.relay, self.identity = str(relay), str(identity).lower()
        self.rooms = tuple(sorted(str(room) for room in rooms))
        key = hashlib.sha256((self.relay + "\0" + self.identity + "\0" + "\0".join(self.rooms)).encode()).hexdigest()[:24]
        self.path = adapter_state_dir(state_dir) / f"{key}.json"
        self.max_seen = max(1, int(max_seen))
        self.now = int(time.time() if now is None else now)
        self.floor = self.now
        self.cursor = self.now
        self.seen: dict[str, int] = {}
        self.attempts: dict[str, int] = {}
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
            seen = data.get("seen", {})
            if isinstance(seen, list):
                # Legacy format: IDs without timestamps stay in the replay window.
                self.seen = {str(value): self.cursor for value in seen}
            else:
                self.seen = {str(key): int(value) for key, value in dict(seen).items()}
            attempts = data.get("attempts", {})
            self.attempts = {str(key): int(value) for key, value in dict(attempts).items()}
        except (OSError, ValueError, TypeError, AttributeError):
            return
        self._prune()

    def _save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        os.chmod(self.path.parent, 0o700)
        payload = json.dumps({"floor": self.floor, "cursor": self.cursor, "seen": self.seen,
                              "attempts": self.attempts}, separators=(",", ":"))
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

    def _window_start(self) -> int:
        return max(self.floor, self.cursor - _REPLAY_WINDOW)

    def _prune(self) -> None:
        """Drop IDs the window already rejects; the hard ceiling alone may raise the floor."""
        start = self._window_start()
        self.seen = {key: stamp for key, stamp in self.seen.items() if stamp >= start}
        if len(self.seen) > self.max_seen:
            stamps = sorted(self.seen.values(), reverse=True)
            threshold = stamps[self.max_seen - 1]
            if sum(1 for stamp in stamps if stamp >= threshold) > self.max_seen:
                # More IDs share the oldest retained second than fit; retire that second.
                threshold += 1
            self.seen = {key: stamp for key, stamp in self.seen.items() if stamp >= threshold}
            self.floor = max(self.floor, threshold)
            logger.warning("Tiny replay cache hit its hard ceiling of %d IDs; events before %d are now ignored",
                           self.max_seen, self.floor)
        self.attempts = {key: count for key, count in self.attempts.items() if key not in self.seen}
        while len(self.attempts) > _MAX_ATTEMPT_ENTRIES:
            self.attempts.pop(next(iter(self.attempts)))

    def should_accept(self, event: dict, *, now: int | None = None) -> bool:
        """Return true only for unseen, non-expired events at/after the cursor."""
        event_id = str(event.get("id", ""))
        if not _EVENT_ID.fullmatch(event_id) or event_id in self.seen:
            return False
        try:
            created = int(event.get("created_at"))
        except (TypeError, ValueError):
            return False
        if created < self._window_start():
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
            created = int(event.get("created_at", self.cursor))
        except (TypeError, ValueError):
            created = self.cursor
        self.cursor = max(self.cursor, created)
        self.seen.setdefault(event_id, created)
        self.attempts.pop(event_id, None)
        self._prune()
        self._save()

    def fail(self, event: dict) -> bool:
        """Count a failed hand-off; retire the event after MAX_ATTEMPTS and return True."""
        event_id = str(event.get("id", ""))
        if not _EVENT_ID.fullmatch(event_id):
            return False
        count = self.attempts.pop(event_id, 0) + 1
        if count >= MAX_ATTEMPTS:
            self.mark(event)
            return True
        self.attempts[event_id] = count
        self._prune()
        self._save()
        return False

    def filter_since(self) -> dict:
        """Return an overlapping subscription filter; same-timestamp IDs are retained."""
        # Match should_accept's bounded overlap. A one-second overlap is insufficient
        # when the helper returns events out of order or the gateway was offline.
        result = {"since": self._window_start()}
        if self.rooms:
            result["#h"] = list(self.rooms)
        return result


class IdentityLock:
    """Exclusive per-identity lock so one gateway consumes a Tiny key at a time.

    The lock file is keyed by relay and public key and lives in ``lock_dir()``, which
    every profile of one Hermes installation shares.
    """

    def __init__(self, relay: str, pubkey: str, *, state_dir: str | Path | None = None):
        key = hashlib.sha256((str(relay) + "\0" + str(pubkey).lower()).encode()).hexdigest()[:24]
        self.path = lock_dir(state_dir) / f"{key}.lock"
        self._fd: int | None = None

    @property
    def held(self) -> bool:
        return self._fd is not None

    def acquire(self) -> None:
        if self._fd is not None:
            return
        self.path.parent.mkdir(parents=True, exist_ok=True)
        os.chmod(self.path.parent, 0o700)
        fd = os.open(self.path, os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            os.close(fd)
            raise RuntimeError("another Hermes gateway already runs this Tiny identity") from None
        try:
            os.ftruncate(fd, 0)
            os.write(fd, str(os.getpid()).encode())
        except OSError:
            pass
        self._fd = fd

    def release(self) -> None:
        if self._fd is None:
            return
        fd, self._fd = self._fd, None
        try:
            fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)


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


__all__ = ["EventTracker", "IdentityLock", "MAX_ATTEMPTS", "MAX_SEEN", "event_thread", "reply_parent",
           "is_reply_to_own", "state_root", "adapter_state_dir", "shared_root", "lock_dir"]
