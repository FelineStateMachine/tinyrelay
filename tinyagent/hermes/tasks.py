"""Per-task state for long-task cards (kinds 43001 to 43006).

This module has no Hermes or network dependencies. The adapter records what it
sends during a task turn here and asks this state what to publish; the rules
for progress and result content live in one place.
"""

from __future__ import annotations

from dataclasses import dataclass, field
import re
from typing import Any

KIND_JOB_REQUEST = 43001
KIND_JOB_ACCEPTED = 43002
KIND_JOB_PROGRESS = 43003
KIND_JOB_RESULT = 43004
KIND_JOB_CANCEL = 43005
KIND_JOB_ERROR = 43006
TERMINAL_KINDS = (KIND_JOB_RESULT, KIND_JOB_CANCEL, KIND_JOB_ERROR)

# Message classes the adapter reports while a task runs.
TEXT = "text"      # assistant text: a streamed draft or a final reply
CHROME = "chrome"  # tool progress bubbles and other status sends
FILE = "file"      # an attachment message

SUMMARY_LIMIT = 200
PROGRESS_LINE_LIMIT = 200
RESULT_TEXT_LIMIT = 4000
ERROR_TEXT_LIMIT = 300
# Streaming cursor glyphs Hermes appends to an in-progress draft.
_CURSOR = re.compile("[\\s█-▏]+$")
_SPACES = re.compile(r"\s+")


def clip(text: str, limit: int) -> str:
    """Collapse whitespace and cut to ``limit`` characters with a marker."""
    text = _SPACES.sub(" ", str(text or "")).strip()
    if len(text) <= limit:
        return text
    return text[: max(0, limit - 3)].rstrip() + "..."


def first_line(text: str) -> str:
    """Return the first non-empty line of ``text``."""
    return next((line.strip() for line in str(text or "").splitlines() if line.strip()), "")


def last_line(text: str) -> str:
    """Return the last non-empty line of ``text`` without a streaming cursor."""
    text = _CURSOR.sub("", str(text or ""))
    return next((line.strip() for line in reversed(text.splitlines()) if line.strip()), "")


def request_text(event: dict) -> str:
    """Return the turn text for a request: the subject line, then the content."""
    subject = next((row[1] for row in event.get("tags", []) if len(row) > 1 and row[0] == "subject"), "")
    subject = str(subject or "").strip()
    content = str(event.get("content", "") or "").strip()
    if subject and content:
        return subject + "\n\n" + content
    return subject or content


def request_root(event: dict) -> str:
    """Return the marked root e tag of a request, else the request id."""
    for row in event.get("tags", []):
        if len(row) >= 4 and row[0] == "e" and row[3] == "root" and row[1]:
            return str(row[1])
    return str(event.get("id", ""))


@dataclass
class TaskState:
    request_id: str
    room: str
    requester: str
    thread_root: str
    session_key: str
    accepted: bool = False
    terminal: bool = False
    # Monotonic time of the last published progress; None before the first one.
    last_progress: float | None = None
    pending_text: str = ""
    pending_tool: str = ""
    # Text Hermes returned from the handler, used only when nothing was posted.
    final_text: str = ""
    error: str = ""
    # message id -> latest content and class, in posting order.
    messages: dict[str, str] = field(default_factory=dict)
    classes: dict[str, str] = field(default_factory=dict)
    files: list[tuple[str, str]] = field(default_factory=list)
    final_id: str | None = None
    timer: Any = None

    # -- observation -----------------------------------------------------

    def observe(self, message_id: str, content: str, cls: str, *, final: bool = False,
                file_name: str | None = None) -> None:
        """Record a successful send or edit that belongs to this task's turn."""
        if self.terminal or not message_id:
            return
        message_id = str(message_id)
        if message_id not in self.classes:
            self.classes[message_id] = cls
        if cls == FILE and file_name and message_id not in {row[0] for row in self.files}:
            self.files.append((message_id, file_name))
        if cls != FILE or content:
            self.messages[message_id] = content or ""
        if final and self.classes.get(message_id) != FILE:
            # The result follows a final reply; no progress line repeats it.
            self.final_id = message_id
            self.pending_text = self.pending_tool = ""
            return
        line = last_line(content)
        if not line:
            return
        if self.classes.get(message_id) == TEXT:
            self.pending_text = clip(line, PROGRESS_LINE_LIMIT)
        elif cls != FILE:
            self.pending_tool = clip(line, PROGRESS_LINE_LIMIT)

    def note_error(self, text: str) -> None:
        text = clip(text, ERROR_TEXT_LIMIT)
        if text:
            self.error = text

    # -- progress --------------------------------------------------------

    @property
    def has_progress(self) -> bool:
        return bool(self.pending_text or self.pending_tool)

    def progress_due(self, now: float, interval: float) -> float:
        """Return seconds to wait before the pending progress may be published."""
        if self.last_progress is None:
            return 0.0
        return max(0.0, self.last_progress + interval - now)

    def take_progress(self, now: float) -> str:
        """Return the coalesced progress line and reset the pending state."""
        parts = [part for part in (self.pending_text, self.pending_tool) if part]
        self.pending_text = self.pending_tool = ""
        self.last_progress = now
        return "\n".join(parts)

    # -- terminal --------------------------------------------------------

    def end(self) -> bool:
        """Mark the task terminal; True only for the first caller."""
        if self.terminal:
            return False
        self.terminal = True
        self.pending_text = self.pending_tool = ""
        return True

    def result(self) -> tuple[str, list[list[str]]]:
        """Return the 43004 content and extra tags.

        Rule: when the final reply is in the thread (a send Hermes marked final or a
        finalizing edit, else the last text message), the content is its first line,
        clipped to 200 characters, and an e tag names that message. Posted files add
        one e tag each and a "Posted ..." line. When nothing was posted, the content
        is the text Hermes returned, else "Done".
        """
        final_id = self.final_id
        if final_id is None:
            texts = [key for key, cls in self.classes.items() if cls == TEXT and self.messages.get(key, "").strip()]
            final_id = texts[-1] if texts else None
        tags: list[list[str]] = []
        lines: list[str] = []
        if final_id is not None and self.messages.get(final_id, "").strip():
            lines.append(clip(first_line(self.messages[final_id]), SUMMARY_LIMIT))
            tags.append(["e", final_id])
        if self.files:
            lines.append("Posted " + ", ".join(name for _, name in self.files))
            tags.extend(["e", message_id] for message_id, _ in self.files)
        if lines:
            return "\n".join(lines), tags
        text = str(self.final_text or "").strip()
        return (text[:RESULT_TEXT_LIMIT] if text else "Done"), []

    def failure(self) -> str:
        return self.error or "The task failed"


def answer_tags(task: TaskState) -> list[list[str]]:
    """Tags every answer to a request carries besides the room's h tag."""
    return [["e", task.request_id], ["p", task.requester]]


__all__ = ["TaskState", "TERMINAL_KINDS", "KIND_JOB_REQUEST", "KIND_JOB_ACCEPTED", "KIND_JOB_PROGRESS",
           "KIND_JOB_RESULT", "KIND_JOB_CANCEL", "KIND_JOB_ERROR", "TEXT", "CHROME", "FILE",
           "answer_tags", "clip", "first_line", "last_line", "request_text", "request_root"]
