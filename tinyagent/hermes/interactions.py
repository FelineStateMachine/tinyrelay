"""Native request cards and exact Hermes callback correlation."""

from __future__ import annotations

from dataclasses import dataclass
import json
import time
from types import SimpleNamespace

from gateway.platforms.base import SendResult


def tag(event, name):
    return next((row[1] for row in event.get("tags", []) if len(row) > 1 and row[0] == name), None)


@dataclass
class Pending:
    room: str
    assignee: str
    session: str
    callback_id: str
    interaction: str
    selection: str
    options: dict[str, str]
    created_at: int
    expires: int


class InteractionMixin:
    async def send_exec_approval(self, chat_id, command, session_key, description="dangerous command",
                                 metadata=None, allow_permanent=True, allow_session=True, smart_denied=False):
        # Current Hermes centralizes prompt formatting; older releases call the same
        # public adapter hook without providing that base implementation.
        parent = getattr(super(), "send_exec_approval", None)
        if parent is not None:
            return await parent(chat_id=chat_id, command=command, session_key=session_key,
                                description=description, metadata=metadata, allow_permanent=allow_permanent,
                                allow_session=allow_session, smart_denied=smart_denied)
        actions = [("Approve once", "once", "primary")]
        if not smart_denied and allow_session:
            actions.append(("Approve for session", "session", "secondary"))
            if allow_permanent:
                actions.append(("Always approve", "always", "secondary"))
        actions.append(("Deny", "deny", "danger"))
        text = f"Command approval required\n\n```\n{command}\n```\n\nReason: {description}"
        if smart_denied:
            text += "\n\nAn override applies to this operation only."
        return await self._send_exec_approval_prompt(SimpleNamespace(
            chat_id=chat_id, command=command, session_key=session_key, text=text,
            metadata=metadata, actions=actions))

    def _assignee(self, session_key, metadata):
        return self._session_assignees.get(session_key) or (metadata or {}).get("user_id", "")

    async def _request(self, pending, content, subject=""):
        if not self._authorized(pending.assignee):
            return SendResult(success=False, error="Tiny request has no authorized human assignee")
        if len(pending.options) > 12:
            return SendResult(success=False, error="Tiny supports at most 12 choices per question")
        tags = [["tinyagent", "1"], ["request", "question"], ["interaction", pending.interaction],
                ["p", pending.assignee], ["selection", pending.selection],
                ["expiration", str(pending.expires)], ["subject", subject or content[:100]]]
        for key, label in pending.options.items():
            visible = label.encode("utf-8")[:200].decode("utf-8", errors="ignore")
            tags.append(["option", key, visible])
        if pending.interaction == "question" and pending.options:
            tags.append(["freeform", "true"])
        # Assignment and notification are separate even when the same key appears in both.
        if pending.assignee in self._mentions(content):
            tags.append(["mention", pending.assignee])
        result = await self._publish(pending.room, content, tags)
        if result.success and result.message_id:
            self._pending[result.message_id] = pending
        return result

    def _pending_request(self, room, session, callback, interaction, options, selection, metadata, ttl=300):
        now = int(time.time())
        return Pending(room, self._assignee(session, metadata), session, callback, interaction,
                       selection, options, now, now + ttl)

    async def send_clarify(self, chat_id, question, choices, clarify_id, session_key, metadata=None):
        from tools import clarify_gateway
        with clarify_gateway._lock:
            entry = clarify_gateway._entries.get(clarify_id)
            multiple = bool(entry and entry.multi_select)
        options = {f"c{index}": str(label) for index, label in enumerate(choices or [])}
        selection = "multiple" if multiple else "single" if choices else "text"
        pending = self._pending_request(chat_id, session_key, clarify_id, "question", options,
                                        selection, metadata, ttl=clarify_gateway.get_clarify_timeout() or 3600)
        return await self._request(pending, question)

    async def _send_exec_approval_prompt(self, prompt):
        from tools.approval import list_gateway_approvals
        used = {entry.callback_id for entry in self._pending.values() if entry.interaction == "approval"}
        candidates = [entry for entry in list_gateway_approvals(prompt.session_key)
                      if entry.get("request_id") and entry["request_id"] not in used]
        matching = [entry for entry in candidates if entry.get("command") == prompt.command]
        # Hermes redacts command previews. A single remaining waiter is still unambiguous;
        # multiple unmatched waiters require the gateway's standard fallback.
        entry = matching[0] if matching else candidates[0] if len(candidates) == 1 else None
        if entry is None:
            return SendResult(success=False, error="Tiny cannot identify the pending Hermes approval")
        options = {choice: label for label, choice, _style in prompt.actions}
        from tools.approval_context import _get_approval_timeout
        pending = self._pending_request(prompt.chat_id, prompt.session_key, entry["request_id"],
                                        "approval", options, "single", prompt.metadata,
                                        ttl=int(_get_approval_timeout()))
        return await self._request(pending, prompt.text, prompt.command[:200])

    async def send_slash_confirm(self, chat_id, title, message, session_key, confirm_id, metadata=None):
        options = {"once": "Approve once", "always": "Always approve", "cancel": "Cancel"}
        pending = self._pending_request(chat_id, session_key, confirm_id, "confirmation", options,
                                        "single", metadata)
        return await self._request(pending, f"{title}\n\n{message}", title)

    async def _on_answer(self, event):
        event_id = tag(event, "e")
        pending = self._pending.get(event_id)
        if pending is None or not self._answer_matches(event, event_id, pending):
            return
        response = self._answer_content(event.get("content", ""), pending)
        if response is None:
            return
        if pending.interaction == "question":
            from tools.clarify_gateway import resolve_gateway_clarify
            resolved = resolve_gateway_clarify(pending.callback_id, response)
        elif pending.interaction == "approval":
            from tools.approval import resolve_gateway_approval
            resolved = resolve_gateway_approval(pending.session, response, request_id=pending.callback_id) > 0
        else:
            from tools import slash_confirm
            current = slash_confirm.get_pending(pending.session)
            resolved = bool(current and current.get("confirm_id") == pending.callback_id)
            if resolved:
                result = await slash_confirm.resolve(pending.session, pending.callback_id, response)
                if result:
                    await self.send(pending.room, result)
        # Waiters are process-local in Hermes. Never apply a late answer to another request.
        self._pending.pop(event_id, None)

    def _answer_matches(self, event, event_id, pending):
        now = int(time.time())
        if now >= pending.expires:
            self._pending.pop(event_id, None)
            return False
        if event.get("kind") != 1111 or event.get("pubkey") != pending.assignee:
            return False
        if not self._authorized(pending.assignee):
            return False
        created = event.get("created_at", 0)
        if not isinstance(created, int) or not pending.created_at <= created < pending.expires or created > now + 60:
            return False
        expected = {"h": pending.room, "e": event_id, "E": event_id, "k": "9", "K": "9",
                    "p": self.pubkey, "P": self.pubkey}
        return all(tag(event, key) == value for key, value in expected.items())

    @staticmethod
    def _answer_content(content, pending):
        if not isinstance(content, str) or not content.strip() or len(content) > 8000:
            return None
        if pending.selection == "text":
            return content
        try:
            decoded = json.loads(content)
        except ValueError:
            decoded = None
        if pending.interaction == "question" and isinstance(decoded, dict):
            text = decoded.get("text")
            return text if set(decoded) == {"text"} and isinstance(text, str) and text.strip() else None
        if pending.selection == "multiple":
            if not isinstance(decoded, list) or not decoded or any(not isinstance(key, str) for key in decoded):
                return None
            if len(decoded) != len(set(decoded)) or any(key not in pending.options for key in decoded):
                return None
            return json.dumps([pending.options[key] for key in decoded])
        if content not in pending.options:
            return None
        return pending.options[content] if pending.interaction == "question" else content
