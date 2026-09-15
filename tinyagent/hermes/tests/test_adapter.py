"""Exercise real Hermes waiters through the Tiny adapter's public hooks."""

import asyncio
import json
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
from adapter import TinyAdapter
from gateway.config import PlatformConfig
from gateway.platform_registry import PlatformEntry, platform_registry
from tools import approval, clarify_gateway, slash_confirm
from tools.approval_gateway_wait import _ApprovalEntry

OWNER = "b" * 64
BOT = "a" * 64
OTHER = "c" * 64


class FakeRPC:
    def __init__(self):
        self.events = []
        self.rows = []

    async def query(self, filter):
        return [row for row in self.rows if row["id"] in filter.get("ids", [])]

    async def publish(self, event):
        result = {"id": f"{len(self.events) + 1:064x}", "created_at": int(time.time()), "pubkey": BOT, **event}
        self.events.append(result)
        return result

    async def query(self, _filter):
        return []


@pytest.fixture
def adapter(monkeypatch, tmp_path):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    platform_registry.register(PlatformEntry(name="tiny", label="Tiny", adapter_factory=TinyAdapter,
                                            check_fn=lambda: True))
    instance = TinyAdapter(PlatformConfig(enabled=True, extra={"rooms": "room", "allowed_users": f"{OWNER},{OTHER}"}))
    instance.rpc = FakeRPC()
    instance.pubkey = BOT
    instance._session_assignees["session"] = OWNER
    yield instance
    clarify_gateway._entries.clear()
    clarify_gateway._session_index.clear()
    approval._gateway_queues.clear()
    slash_confirm._pending.clear()


def response(request, content, author=OWNER):
    event_id = request["id"]
    return {"kind": 1111, "pubkey": author, "created_at": int(time.time()), "content": content,
            "tags": [["h", "room"], ["e", event_id], ["E", event_id], ["p", BOT], ["P", BOT],
                     ["k", str(request["kind"])], ["K", str(request["kind"])]]}


def test_real_inbound_source_tracks_human_session(adapter):
    async def run():
        received = []
        async def handle(message):
            received.append(message)
        adapter.handle_message = handle
        await adapter._on_event({"kind": 9, "id": "d" * 64, "pubkey": OWNER, "created_at": int(time.time()),
                                 "content": "hello", "tags": [["h", "room"], ["p", BOT]]})
        assert len(received) == 1
        key = adapter._event_session_key(received[0])
        assert adapter._session_assignees[key] == OWNER
        assert received[0].source.platform.value == "tiny"
    asyncio.run(run())


@pytest.mark.parametrize("multiple,content,expected", [(False, "c1", "beta"),
    (True, '["c0"]', '["alpha"]'), (True, '["c0","c1"]', '["alpha", "beta"]'),
    (False, '{"text":"my own answer"}', "my own answer"),
    (False, '{"choices":["c1"],"text":"because it is safer"}', "beta\nbecause it is safer"),
    (True, '{"choices":["c0","c1"],"text":"both apply"}', '["alpha", "beta"]\nboth apply')])
def test_clarify_resolves_exact_real_waiter(adapter, multiple, content, expected):
    async def run():
        entry = clarify_gateway.register("question-id", "session", "Pick", ["alpha", "beta"], multiple)
        untouched = clarify_gateway.register("other-question", "session", "Other", ["yes"])
        result = await adapter.send_clarify("room", "Pick", ["alpha", "beta"], "question-id", "session")
        assert result.success
        request = adapter.rpc.events[-1]
        assert ["p", OWNER] in request["tags"]
        assert not any(tag[0] == "mention" for tag in request["tags"])
        assert ["freeform", "true"] in request["tags"]
        assert all("session" not in str(tag) for tag in request["tags"])
        await adapter._on_answer(response(request, content))
        assert entry.event.is_set() and entry.response == expected
        assert not untouched.event.is_set()
    asyncio.run(run())


@pytest.mark.parametrize("mutation", ["actor", "room", "root", "kind", "invalid", "duplicate", "expired"])
def test_invalid_answers_do_not_resolve(adapter, mutation):
    async def run():
        entry = clarify_gateway.register("question-id", "session", "Pick", ["alpha", "beta"], True)
        await adapter.send_clarify("room", "Pick", ["alpha", "beta"], "question-id", "session")
        request = adapter.rpc.events[-1]
        answer = response(request, '["c0"]')
        if mutation == "actor": answer["pubkey"] = OTHER
        if mutation == "room": answer["tags"][0][1] = "elsewhere"
        if mutation == "root": answer["tags"][2][1] = "f" * 64
        if mutation == "kind": answer["kind"] = 7
        if mutation == "invalid": answer["content"] = '["unknown"]'
        if mutation == "duplicate": answer["content"] = '["c0","c0"]'
        if mutation == "expired": adapter._pending[request["id"]].expires = int(time.time()) - 1
        await adapter._on_answer(answer)
        assert not entry.event.is_set()
    asyncio.run(run())


def test_open_ended_question_is_structured(adapter):
    async def run():
        entry = clarify_gateway.register("question-id", "session", "Explain", None)
        await adapter.send_clarify("room", "Explain", None, "question-id", "session")
        request = adapter.rpc.events[-1]
        assert ["selection", "text"] in request["tags"]
        await adapter._on_answer(response(request, "Here is my answer"))
        assert entry.response == "Here is my answer"
    asyncio.run(run())


def test_approval_uses_exact_request_and_offered_scope(adapter):
    async def run():
        first = _ApprovalEntry({"command": "first", "request_id": "first-id"})
        second = _ApprovalEntry({"command": "second", "request_id": "second-id"})
        approval._gateway_queues["session"] = [first, second]
        assert (await adapter.send_exec_approval("room", "second", "session",
                                                allow_permanent=False, allow_session=False)).success
        request = adapter.rpc.events[-1]
        await adapter._on_answer(response(request, "always"))
        assert not first.event.is_set() and not second.event.is_set()
        await adapter._on_answer(response(request, "once"))
        assert not first.event.is_set() and second.event.is_set()
        assert second.result == "once"
        await adapter._on_answer(response(request, "once"))
        assert not first.event.is_set()
    asyncio.run(run())


def test_confirmation_calls_real_handler_once(adapter):
    async def run():
        choices = []
        async def handler(choice):
            choices.append(choice)
            return "Reloaded"
        slash_confirm.register("session", "confirm-id", "reload", handler)
        await adapter.send_slash_confirm("room", "Reload?", "Confirm", "session", "confirm-id")
        request = adapter.rpc.events[-1]
        await adapter._on_answer(response(request, "once"))
        await adapter._on_answer(response(request, "once"))
        assert choices == ["once"]
        assert adapter.rpc.events[-1]["content"] == "Reloaded"
    asyncio.run(run())


def test_explicit_mentions_and_edits(adapter):
    async def run():
        result = await adapter.send("room", "ordinary", reply_to="d" * 64)
        assert not any(tag[0] in ("p", "mention") for tag in adapter.rpc.events[-1]["tags"])
        edited = await adapter.edit_message("room", result.message_id, f"Attention @{OWNER}")
        assert edited.message_id == result.message_id
        event = adapter.rpc.events[-1]
        assert event["kind"] == 40003
        assert ["p", OWNER] in event["tags"] and ["mention", OWNER] in event["tags"]
        assert not await adapter.delete_message("room", result.message_id)
    asyncio.run(run())


def test_native_prompt_mentions_assignee_only_when_explicit(adapter):
    async def run():
        clarify_gateway.register("q", "session", "Pick", ["yes"])
        await adapter.send_clarify("room", f"@{OWNER} Pick", ["yes"], "q", "session")
        assert ["mention", OWNER] in adapter.rpc.events[-1]["tags"]
    asyncio.run(run())


def test_native_prompt_stays_in_current_thread(adapter):
    async def run():
        clarify_gateway.register("q", "session", "Pick", ["yes"])
        result = await adapter.send_clarify("room", "Pick", ["yes"], "q", "session",
                                            metadata={"thread_id": "r" * 64, "message_id": "m" * 64})
        assert result.success
        event = adapter.rpc.events[-1]
        assert event["kind"] == 12
        assert ["e", "r" * 64, "", "root"] in event["tags"]
        assert ["e", "m" * 64, "", "reply"] in event["tags"]
    asyncio.run(run())


@pytest.mark.parametrize("kind", [9, 12])
def test_real_inbound_reply_keeps_prompt_and_answer_in_existing_thread(adapter, kind):
    async def run():
        root, parent, incoming = "d" * 64, "e" * 64, "f" * 64
        adapter.rpc.rows = [{"id": parent, "kind": kind, "pubkey": BOT, "content": "Earlier reply",
                             "tags": [["h", "room"], ["e", root, "", "root"]]}]
        async def handle(message):
            session = adapter._event_session_key(message)
            assert (message.source.thread_id is None) == (kind == 9)
            entry = clarify_gateway.register("thread-question", session, "Pick", ["Yes", "No"])
            # A newer queued message must not steal the active turn's assignee.
            adapter._session_assignees[session] = OTHER
            current_route = adapter._turn_routes[session]
            adapter._turn_routes[session] = {**current_route, "user_id": OTHER, "reply_to_message_id": "1" * 64}
            # Hermes forwards only thread_id for Tiny, and nothing for kind 9.
            metadata = {"thread_id": message.source.thread_id} if message.source.thread_id else None
            await adapter.send_clarify("room", "Pick", ["Yes", "No"], "thread-question", session, metadata)
            request = adapter.rpc.events[-1]
            assert ["p", OWNER] in request["tags"]
            assert request["kind"] == kind
            assert ["e", root, "", "root"] in request["tags"]
            assert ["e", incoming, "", "reply"] in request["tags"]
            await adapter._on_answer(response(request, '{"choices":["c1"],"text":"Keep this thread."}'))
            assert entry.event.is_set() and entry.response == "No\nKeep this thread."
            adapter._turn_routes[session] = current_route
            await adapter.send("room", "Acknowledged", metadata=metadata)
            assert ["e", root, "", "root"] in adapter.rpc.events[-1]["tags"]
        adapter.set_message_handler(handle)
        adapter.handle_message = adapter._message_handler
        await adapter._on_event({"kind": kind, "id": incoming, "pubkey": OWNER,
            "created_at": int(time.time()), "content": "Ask here", "tags": [
                ["h", "room"], ["p", BOT], ["e", root, "", "root"], ["e", parent, "", "reply"]]})
        # The final gateway send occurs after the handler completes.
        await adapter.send("room", "Final reply", reply_to=incoming)
        event = adapter.rpc.events[-1]
        assert event["kind"] == kind
        assert ["e", root, "", "root"] in event["tags"]
        assert ["e", incoming, "", "reply"] in event["tags"]
        assert not adapter._turn_routes and adapter._turn_route.get() is None
    asyncio.run(run())


def test_gateway_final_attachment_keeps_the_turn_route(adapter, tmp_path):
    async def run():
        import hashlib
        root, incoming = "d" * 64, "e" * 64
        note = tmp_path / "answer.txt"
        note.write_text("Details for this answer.")
        async def upload(room, filename, mime, data):
            return {"url": "https://tiny.test/answer.txt", "sha256": hashlib.sha256(data).hexdigest()}
        adapter.rpc.upload = upload
        async def handler(message):
            return f"Here is the answer.\nMEDIA:{note}"
        adapter.set_message_handler(handler)
        async def process(message):
            await adapter._process_message_background(message, adapter._event_session_key(message))
        adapter.handle_message = process
        await adapter._on_event({"kind": 9, "id": incoming, "pubkey": OWNER,
            "created_at": int(time.time()), "content": "Respond here", "tags": [
                ["h", "room"], ["p", BOT], ["e", root, "", "root"]]})
        replies = [event for event in adapter.rpc.events if event["kind"] == 9]
        assert len(replies) > 1
        assert all(["e", root, "", "root"] in event["tags"] for event in replies)
        assert not adapter._turn_routes and adapter._turn_route.get() is None
    asyncio.run(run())


def test_threaded_kind9_reply_keeps_root_session(adapter):
    async def run():
        received = []
        async def handle(message):
            received.append(message)
        adapter.handle_message = handle
        root = "r" * 64
        event_id = "e" * 64
        await adapter._on_event({"kind": 9, "id": event_id, "pubkey": OWNER,
                                 "created_at": int(time.time()), "content": "follow-up",
                                 "tags": [["h", "room"], ["e", root], ["p", BOT]]})
        assert len(received) == 1
        assert received[0].source.thread_id is None
        assert received[0].reply_to_message_id == root
    asyncio.run(run())


def test_threaded_question_callback_uses_kind12_correlation(adapter):
    async def run():
        entry = clarify_gateway.register("question-id", "session", "Pick", ["yes"])
        await adapter.send_clarify("room", "Pick", ["yes"], "question-id", "session",
                                   metadata={"thread_id": "r" * 64})
        request = adapter.rpc.events[-1]
        await adapter._on_answer(response(request, "c0"))
        assert entry.response == "yes"
    asyncio.run(run())


def test_outbound_document_preserves_thread_caption_and_bytes(adapter, tmp_path):
    import hashlib
    data = b'attachment bytes'
    path = tmp_path / 'note.txt'
    path.write_bytes(data)
    digest = hashlib.sha256(data).hexdigest()
    uploads = []
    async def upload(room, name, mime, body):
        uploads.append((room, name, mime, body))
        return {'url': f'http://tiny/media/{digest}', 'sha256': digest}
    adapter.rpc.upload = upload
    result = asyncio.run(adapter.send_document(chat_id='room', file_path=str(path),
        file_name='renamed.txt', caption=f'For @{OWNER}', metadata={'thread_id': 'd' * 64}, reply_to='e' * 64))
    assert result.success
    event = adapter.rpc.events[-1]
    assert uploads == [('room', 'renamed.txt', 'text/plain', data)]
    assert event['kind'] == 12
    assert ['e', 'd' * 64, '', 'root'] in event['tags']
    assert ['e', 'e' * 64, '', 'reply'] in event['tags']
    assert ['p', OWNER] in event['tags'] and ['mention', OWNER] in event['tags']
    assert any(t[0] == 'imeta' and f'x {digest}' in t and 'size 16' in t for t in event['tags'])


def test_standalone_delivery_uses_native_media(adapter, tmp_path, monkeypatch):
    import adapter as module
    import hashlib
    path = tmp_path / 'voice.mp3'
    path.write_bytes(b'audio bytes')
    rpc = adapter.rpc
    async def connect(): pass
    async def close(): pass
    async def upload(room, name, mime, body):
        digest = hashlib.sha256(body).hexdigest()
        return {'url': f'http://tiny/media/{digest}', 'sha256': digest}
    rpc.connect, rpc.close, rpc.upload = connect, close, upload
    monkeypatch.setattr(module, 'TinyRPC', lambda *a, **kw: rpc)
    result = asyncio.run(module._standalone_send(adapter.config, 'room', f'For @{OWNER}',
        thread_id='d' * 64, media_files=[(str(path), True)]))
    assert result['success'] and result['media_delivered']
    assert len(rpc.events) == 1
    event = rpc.events[0]
    assert event['kind'] == 12
    assert ['mention', OWNER] in event['tags']
    assert any(t[0] == 'imeta' and 'm audio/mpeg' in t for t in event['tags'])


def test_outbound_media_rejects_wrong_relay_hash(adapter, tmp_path):
    path = tmp_path / 'note.txt'
    path.write_text('local content')
    async def upload(*args): return {'url': 'http://tiny/media/' + '0' * 64, 'sha256': '0' * 64}
    adapter.rpc.upload = upload
    result = asyncio.run(adapter.send_document('room', str(path)))
    assert not result.success and adapter.rpc.events == []


def test_duplicate_callbacks_cannot_race_before_media_intake(adapter, monkeypatch):
    import adapter as module
    received = []
    async def media(*args):
        await asyncio.sleep(0.01)
        return [], []
    async def handle(message): received.append(message)
    monkeypatch.setattr(module, 'incoming_media', media)
    adapter.handle_message = handle
    event = {'kind': 9, 'id': 'e' * 64, 'pubkey': OWNER, 'created_at': int(time.time()),
             'content': 'hello', 'tags': [['h', 'room'], ['p', BOT]]}
    async def run(): await asyncio.gather(adapter._on_event(event), adapter._on_event(event))
    asyncio.run(run())
    assert len(received) == 1


@pytest.mark.parametrize('setting,expected', [('true', 0), ('false', 1), (False, 1), ('0', 1)])
def test_room_message_trigger_respects_require_mention(adapter, setting, expected):
    instance = TinyAdapter(PlatformConfig(enabled=True, extra={
        'rooms': 'room', 'allowed_users': OWNER, 'require_mention': setting}))
    instance.pubkey = BOT
    instance.rpc = adapter.rpc
    received = []
    async def handle(message): received.append(message)
    instance.handle_message = handle
    async def run():
        for author in (OWNER, OTHER):
            await instance._on_event({'kind': 9, 'id': author, 'pubkey': author,
                'created_at': int(time.time()), 'content': 'hello', 'tags': [['h', 'room']]})
    asyncio.run(run())
    assert len(received) == expected
    assert all(message.source.user_id == OWNER for message in received)


@pytest.mark.parametrize('legacy', [False, True])
@pytest.mark.parametrize('session,permanent,smart,expected', [
    (True, True, False, {'once', 'session', 'always', 'deny'}),
    (True, False, False, {'once', 'session', 'deny'}),
    (False, True, False, {'once', 'deny'}),
    (True, True, True, {'once', 'deny'}),
])
def test_public_approval_hook_preserves_scopes_on_older_hermes(adapter, monkeypatch, legacy, session, permanent, smart, expected):
    from gateway.platforms.base import BasePlatformAdapter
    if legacy:
        monkeypatch.delattr(BasePlatformAdapter, 'send_exec_approval', raising=False)
    async def run():
        entry = _ApprovalEntry({'command': 'safe preview', 'request_id': 'scoped-id'})
        approval._gateway_queues['session'] = [entry]
        result = await adapter.send_exec_approval('room', 'safe preview', 'session',
            allow_session=session, allow_permanent=permanent, smart_denied=smart)
        assert result.success
        request = adapter.rpc.events[-1]
        assert {t[1] for t in request['tags'] if t[0] == 'option'} == expected
        assert not any(t[0] == 'mention' for t in request['tags'])
        await adapter._on_answer(response(request, 'once'))
        assert entry.event.is_set() and entry.result == 'once'
    asyncio.run(run())
