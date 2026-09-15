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


def admit(message):
    """What Hermes's handle_message records once it starts or queues a turn."""
    message._gateway_accepted = True


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


class FakeHelper:
    """Stands in for TinyRPC: answers identity, records filters and dies on demand."""

    instances = []
    fail_identity = False

    def __init__(self, relay_url, key_env="TINY_PRIVATE_KEY", cli_path="tinyagent", on_exit=None):
        self.relay_url, self.on_exit = relay_url, on_exit
        self.filters, self.subscriptions, self.events = [], {}, []
        self.closed = self.dead = False
        FakeHelper.instances.append(self)

    @property
    def alive(self):
        return not (self.closed or self.dead)

    async def call(self, method, params=None, timeout=30):
        if FakeHelper.fail_identity:
            raise RuntimeError("helper unavailable")
        return {"pubkey": BOT} if method == "identity" else {}

    async def subscribe(self, name, filter, callback):
        self.filters.append(filter)
        self.subscriptions[name] = callback

    async def unsubscribe(self, name):
        self.subscriptions.pop(name, None)

    async def query(self, _filter):
        return []

    async def close(self):
        self.closed = True

    def die(self):
        self.dead = True
        self.on_exit(self)


@pytest.fixture
def helper(monkeypatch):
    import adapter as module
    FakeHelper.instances.clear()
    FakeHelper.fail_identity = False
    monkeypatch.setattr(module, "TinyRPC", FakeHelper)
    yield FakeHelper
    FakeHelper.fail_identity = False


def live_adapter(marks=None):
    instance = TinyAdapter(PlatformConfig(enabled=True, extra={"relay_url": "http://relay", "rooms": "room",
                                                               "allowed_users": OWNER}))
    instance.RECONNECT_MIN = 0.01
    instance.RECONNECT_MAX = 0.03
    if marks is not None:
        original = instance._mark_connected
        def marked(**kwargs):
            marks.append("connected")
            original(**kwargs)
        instance._mark_connected = marked
    return instance


def test_helper_death_reconnects_from_persisted_cursor(adapter, helper, tmp_path):
    from ingress import EventTracker
    marks = []
    instance = live_adapter(marks)
    now = int(time.time())
    # A tracker with history: the floor is old and the cursor moves with delivered events.
    instance._tracker = EventTracker("http://relay", BOT, ["room"], now=now - 1000)
    received = []
    async def handle(message):
        received.append(message)
        admit(message)
    instance.handle_message = handle

    async def run():
        assert await instance.connect()
        first = helper.instances[-1]
        assert instance.is_connected and instance._lock.held
        assert first.filters == [{"kinds": [9, 11, 12, 1111, 43001, 43005], "since": now - 1000, "#h": ["room"]}]
        await first.subscriptions["tinyagent"]({"kind": 9, "id": "d" * 64, "pubkey": OWNER, "created_at": now,
                                                "content": "hello", "tags": [["h", "room"], ["p", BOT]]})
        assert len(received) == 1 and instance._tracker.cursor == now
        first.die()
        assert not instance.is_connected
        await asyncio.wait_for(instance._reconnect_task, 2)
        second = helper.instances[-1]
        assert second is not first and first.closed and instance.rpc is second
        assert second.filters == [{"kinds": [9, 11, 12, 1111, 43001, 43005], "since": now - 300, "#h": ["room"]}]
        assert instance.is_connected and instance._lock.held and marks == ["connected", "connected"]
        # The same tracker persists across the reconnect: the delivered event stays deduplicated.
        assert not instance._tracker.should_accept({"id": "d" * 64, "created_at": now, "tags": []})
        await instance.disconnect()
        assert not instance.is_connected and instance._lock is None and second.closed
    asyncio.run(run())


def test_reconnect_backs_off_and_disconnect_cancels_it(adapter, helper, monkeypatch):
    import adapter as module
    instance = live_adapter()
    delays = []
    real_sleep = asyncio.sleep
    async def sleep(delay, *args):
        delays.append(delay)
        await real_sleep(0)
    monkeypatch.setattr(module.asyncio, "sleep", sleep)

    async def run():
        await instance.connect()
        first = helper.instances[-1]
        helper.fail_identity = True
        first.die()
        task = instance._reconnect_task
        for _ in range(12):
            await real_sleep(0)
        assert delays[:4] == [0.01, 0.02, 0.03, 0.03]
        assert not instance.is_connected
        await instance.disconnect()
        assert task.cancelled() and instance._reconnect_task is None
        spawned = len(helper.instances)
        helper.fail_identity = False
        for _ in range(5):
            await real_sleep(0)
        assert len(helper.instances) == spawned
        assert not instance.is_connected
    asyncio.run(run())


def test_watcher_reconnect_uses_the_same_path(adapter, helper):
    instance = live_adapter()

    async def run():
        await instance.connect()
        first = helper.instances[-1]
        tracker = instance._tracker
        await instance.disconnect()
        assert first.closed and not instance.is_connected
        # Hermes's watcher calls connect(is_reconnect=True); the persisted cursor is reused.
        assert await instance.connect(is_reconnect=True)
        second = helper.instances[-1]
        assert second is not first and instance.is_connected and instance._lock.held
        assert second.filters[0]["since"] == tracker.filter_since()["since"]
        await instance.disconnect()
    asyncio.run(run())


def test_identity_lock_refuses_a_second_gateway(adapter, helper):
    first, second = live_adapter(), live_adapter()

    async def run():
        await first.connect()
        with pytest.raises(RuntimeError, match="another Hermes gateway already runs this Tiny identity"):
            await second.connect()
        assert helper.instances[-1].closed and not second.is_connected and second._lock is None
        await first.disconnect()
        assert await second.connect()
        assert second.is_connected
        await second.disconnect()
    asyncio.run(run())


def test_event_is_marked_only_after_hermes_accepts_it(adapter, tmp_path):
    from ingress import EventTracker
    now = int(time.time())
    adapter._tracker = EventTracker("http://relay", BOT, ["room"], state_dir=tmp_path, now=now - 10)
    poison = {"kind": 9, "id": "d" * 64, "pubkey": OWNER, "created_at": now, "content": "boom",
              "tags": [["h", "room"], ["p", BOT]]}
    attempts = []
    async def failing(message):
        attempts.append(message.message_id)
        raise RuntimeError("Hermes rejected the turn")
    adapter.handle_message = failing
    for attempt in (1, 2):
        with pytest.raises(RuntimeError):
            asyncio.run(adapter._on_event(poison))
        assert adapter._tracker.should_accept(poison)
        assert adapter._tracker.attempts == {poison["id"]: attempt}
    with pytest.raises(RuntimeError):
        asyncio.run(adapter._on_event(poison))
    assert len(attempts) == 3
    assert not adapter._tracker.should_accept(poison) and adapter._tracker.attempts == {}
    asyncio.run(adapter._on_event(poison))
    assert len(attempts) == 3
    handled = []
    async def accept(message):
        handled.append(message.message_id)
        admit(message)
    adapter.handle_message = accept
    good = {**poison, "id": "e" * 64, "content": "hello"}
    asyncio.run(adapter._on_event(good))
    asyncio.run(adapter._on_event(good))
    assert handled == [good["id"]]
    assert not adapter._tracker.should_accept(good)
    assert json.loads(adapter._tracker.path.read_text())["seen"] == {poison["id"]: now, good["id"]: now}


def test_event_refused_without_an_admission_receipt_stays_deliverable(adapter, tmp_path):
    # Hermes returns normally but records no receipt: no turn was started or queued
    # (no handler installed, for example). The event is a failed attempt, not seen.
    from ingress import EventTracker
    now = int(time.time())
    adapter._tracker = EventTracker("http://relay", BOT, ["room"], state_dir=tmp_path, now=now - 10)
    event = {"kind": 9, "id": "d" * 64, "pubkey": OWNER, "created_at": now, "content": "hello",
             "tags": [["h", "room"], ["p", BOT]]}
    calls = []
    async def refused(message):
        calls.append(message.message_id)
    adapter.handle_message = refused
    asyncio.run(adapter._on_event(event))
    assert calls == [event["id"]]
    assert adapter._tracker.should_accept(event) and adapter._tracker.attempts == {event["id"]: 1}
    asyncio.run(adapter._on_event(event))
    asyncio.run(adapter._on_event(event))
    assert len(calls) == 3 and not adapter._tracker.should_accept(event)
    # The real handle_message with no gateway handler installed is exactly that case.
    adapter.handle_message = TinyAdapter.handle_message.__get__(adapter)
    adapter._message_handler = None
    other = {**event, "id": "e" * 64}
    asyncio.run(adapter._on_event(other))
    assert adapter._tracker.attempts == {other["id"]: 1} and adapter._tracker.should_accept(other)
    # A slash command during a busy session is dispatched inline without a receipt.
    async def inline(message):
        calls.append(message.message_id)
    adapter.handle_message = inline
    command = {**event, "id": "f" * 64, "content": "/new"}
    asyncio.run(adapter._on_event(command))
    assert not adapter._tracker.should_accept(command) and command["id"] not in adapter._tracker.attempts
    # An older Hermes without the receipt admits by returning.
    async def legacy(message):
        message._gateway_accepted = None
    adapter.handle_message = legacy
    old = {**event, "id": "1" * 64}
    asyncio.run(adapter._on_event(old))
    assert not adapter._tracker.should_accept(old)


def test_task_request_refused_by_hermes_is_not_kept(adapter, tmp_path):
    from ingress import EventTracker
    now = int(time.time())
    adapter._tracker = EventTracker("http://relay", BOT, ["room"], state_dir=tmp_path, now=now - 10)
    async def refused(message):
        pass
    adapter.handle_message = refused
    event = {"kind": 43001, "id": "d" * 64, "pubkey": OWNER, "created_at": now, "content": "Run it",
             "tags": [["h", "room"], ["p", BOT]]}
    asyncio.run(adapter._on_event(event))
    assert adapter._tasks == {} and adapter._tracker.should_accept(event)
    assert adapter._tracker.attempts == {event["id"]: 1}


class RacingProcess:
    """A helper that delivers subscription frames before acknowledging the subscribe call,
    which the Go helper's scheduling permits."""

    def __init__(self, *, events=(), subscribe_error=None, die_on_subscribe=False):
        from test_client import FakeProcess
        self.inner = FakeProcess()
        self.stdout, self.stdin = self.inner.stdout, self.inner.stdin
        self.stdin.process = self
        self.events, self.subscribe_error, self.die_on_subscribe = list(events), subscribe_error, die_on_subscribe
        self.terminated = False

    @property
    def returncode(self):
        return self.inner.returncode

    def on_request(self, request):
        method = request["method"]
        if method == "identity":
            reply = {"id": request["id"], "result": {"pubkey": BOT}}
        elif method == "query":
            reply = {"id": request["id"], "result": []}
        elif method == "subscribe" and self.subscribe_error:
            reply = {"id": request["id"], "error": {"message": self.subscribe_error}}
        else:
            reply = {"id": request["id"], "result": {}}
        if method != "subscribe":
            self.stdout.feed_data((json.dumps(reply) + "\n").encode())
            return
        name = request["params"]["subscription"]
        frames = [{"event": "connected", "subscription": name}]
        frames += [{"event": "event", "subscription": name, "data": event} for event in self.events]
        self.stdout.feed_data("".join(json.dumps(frame) + "\n" for frame in frames).encode())
        if self.die_on_subscribe:
            self.inner.die()
            return
        asyncio.get_running_loop().call_later(0.01, self.stdout.feed_data, (json.dumps(reply) + "\n").encode())

    def terminate(self):
        self.terminated = True
        self.inner.terminate()

    async def wait(self):
        return self.inner.returncode


class Racing:
    """Plans the next helper process; the process itself is built inside the running loop."""

    def __init__(self):
        self.kwargs = {}
        self.process = None

    def __call__(self, **kwargs):
        self.kwargs = kwargs
        return self

    @property
    def terminated(self):
        return self.process is not None and self.process.terminated

    @property
    def returncode(self):
        return None if self.process is None else self.process.returncode


@pytest.fixture
def racing(monkeypatch):
    import client as module
    plan = Racing()
    async def create(*args, **kwargs):
        plan.process = RacingProcess(**plan.kwargs)
        return plan.process
    monkeypatch.setattr(module.asyncio, "create_subprocess_exec", create)
    return plan


def test_events_delivered_before_the_subscribe_acknowledgment_reach_the_session(adapter, racing):
    now = int(time.time())
    request = {"kind": 43001, "id": "d" * 64, "pubkey": OWNER, "created_at": now, "content": "Run it",
               "tags": [["h", "room"], ["p", BOT]]}
    hello = {"kind": 9, "id": "e" * 64, "pubkey": OWNER, "created_at": now, "content": "hello",
             "tags": [["h", "room"], ["p", BOT]]}
    process = racing(events=[request, hello])
    instance = live_adapter()
    received, errors = [], []
    async def handle(message):
        received.append(message.message_id)
        admit(message)
    instance.handle_message = handle
    original = instance._on_event
    async def guarded(event):
        try:
            await original(event)
        except Exception as exc:
            errors.append(f"{type(exc).__name__}: {exc}")
            raise
    instance._on_event = guarded

    async def run():
        await instance.connect()
        for _ in range(20):
            await asyncio.sleep(0)
        assert errors == [] and sorted(received) == sorted([request["id"], hello["id"]])
        assert instance.is_connected and instance.rpc is not None and instance.rpc.alive
        assert instance._tracker.attempts == {} and set(instance._tracker.seen) == {request["id"], hello["id"]}
        assert "d" * 64 in instance._tasks
        await instance.disconnect()
        assert process.terminated
    asyncio.run(run())


@pytest.mark.parametrize("failure", ["refused", "died"])
def test_failed_subscribe_rolls_the_session_back(adapter, racing, failure):
    now = int(time.time())
    hello = {"kind": 9, "id": "e" * 64, "pubkey": OWNER, "created_at": now, "content": "hello",
             "tags": [["h", "room"], ["p", BOT]]}
    process = racing(events=[hello], subscribe_error="relay refused" if failure == "refused" else None,
                     die_on_subscribe=failure == "died")
    instance = live_adapter()
    received = []
    async def handle(message):
        received.append(message.message_id)
        admit(message)
    instance.handle_message = handle

    async def run():
        with pytest.raises(RuntimeError):
            await instance.connect()
        for _ in range(10):
            await asyncio.sleep(0)
        # The first connect owns nothing afterwards: no helper, no reconnect loop, no lock.
        assert instance.rpc is None and not instance.is_connected and instance._lock is None
        assert instance._reconnect_task is None and instance._establishing is None
        assert process.terminated or process.returncode is not None
        # The event the helper pushed before failing was handled with the helper it came from.
        assert received == [hello["id"]] or received == []
        await instance.disconnect()
    asyncio.run(run())


def test_reconnect_keeps_the_previous_helper_reference_until_the_new_one_subscribes(adapter, helper):
    instance = live_adapter()

    async def run():
        await instance.connect()
        first = helper.instances[-1]
        first.die()
        helper.fail_identity = True
        for _ in range(8):
            await asyncio.sleep(0)
        # Every failed attempt leaves the dead helper as the session's reference, so a
        # late callback from it is refused as stale rather than crashing on None.
        assert instance.rpc is first and not instance.is_connected
        helper.fail_identity = False
        await asyncio.wait_for(instance._reconnect_task, 2)
        assert instance.rpc is helper.instances[-1] and instance.rpc is not first and instance.is_connected
        await instance.disconnect()
    asyncio.run(run())


def test_profile_scope_hands_the_helper_its_own_key(adapter, monkeypatch):
    from agent.secret_scope import reset_secret_scope, set_multiplex_active, set_secret_scope
    import adapter as module
    from client import TinyRPC
    monkeypatch.setenv("TINY_PRIVATE_KEY", "default-profile-key")
    monkeypatch.setenv("TINY_ALLOWED_USERS", OTHER)
    monkeypatch.setenv("TINY_RELAY_URL", "http://default")
    set_multiplex_active(True)
    token = set_secret_scope({"TINY_PRIVATE_KEY": "secondary-profile-key", "TINY_ALLOWED_USERS": OWNER,
                              "TINY_RELAY_URL": "http://secondary"})
    try:
        env = TinyRPC("http://secondary").helper_env()
        assert env["TINY_PRIVATE_KEY"] == "secondary-profile-key"
        assert set(env) <= {"PATH", "HOME", "TMPDIR", "TINY_PRIVATE_KEY", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
                            "http_proxy", "https_proxy", "no_proxy"}
        scoped = TinyAdapter(PlatformConfig(enabled=True, extra={}))
        assert scoped.allowed == {OWNER} and scoped.relay_url == "http://secondary"
        assert module._env_enablement()["allowed_users"] == OWNER
    finally:
        reset_secret_scope(token)
    try:
        # Multiplexing with no scope fails closed for the key; passive probes stay quiet.
        with pytest.raises(RuntimeError):
            TinyRPC("http://secondary").helper_env()
        assert TinyAdapter(PlatformConfig(enabled=True, extra={})).allowed == set()
        assert module.check_requirements() is False
    finally:
        set_multiplex_active(False)
    plain = TinyAdapter(PlatformConfig(enabled=True, extra={}))
    assert plain.allowed == {OTHER} and TinyRPC("http://default").helper_env()["TINY_PRIVATE_KEY"] == "default-profile-key"


def test_identity_lock_is_shared_by_profile_homes_of_one_installation(adapter, helper, tmp_path, monkeypatch):
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "hermes"))
    first, second = live_adapter(), live_adapter()

    async def run():
        token = set_hermes_home_override(tmp_path / "hermes" / "profiles" / "a")
        try:
            await first.connect()
        finally:
            reset_hermes_home_override(token)
        token = set_hermes_home_override(tmp_path / "hermes" / "profiles" / "b")
        try:
            with pytest.raises(RuntimeError, match="another Hermes gateway already runs this Tiny identity"):
                await second.connect()
        finally:
            reset_hermes_home_override(token)
        assert first._lock.path.parent == tmp_path / "hermes" / "state" / "tinyagent" / "locks"
        await first.disconnect()
    asyncio.run(run())


def test_env_enablement_seeds_a_home_channel(adapter, monkeypatch):
    import adapter as module
    from gateway.config import GatewayConfig, HomeChannel, Platform
    from gateway.config_env import _enable_plugin_platform
    monkeypatch.setenv("TINY_RELAY_URL", "http://relay")
    monkeypatch.setenv("TINY_PRIVATE_KEY", "k" * 64)
    monkeypatch.setenv("TINY_HOME_CHANNEL", "home-room")
    assert module._env_enablement()["home_channel"] == {"chat_id": "home-room", "name": "Home"}
    class Ctx:
        def register_platform(self, **kwargs):
            platform_registry.register(PlatformEntry(**kwargs))
    module.register(Ctx())
    config = GatewayConfig()
    _enable_plugin_platform(config, platform_registry.get("tiny"))
    home = config.platforms[Platform("tiny")].home_channel
    assert isinstance(home, HomeChannel) and home.chat_id == "home-room" and home.name == "Home"
    assert "home_channel" not in config.platforms[Platform("tiny")].extra
    assert TinyAdapter(config.platforms[Platform("tiny")]).home_room == "home-room"


def test_answers_resume_typing_after_waits(adapter):
    async def run():
        entry = _ApprovalEntry({"command": "ls", "request_id": "approval-id"})
        approval._gateway_queues["session"] = [entry]
        adapter.pause_typing_for_chat("room")
        await adapter.send_exec_approval("room", "ls", "session")
        request = adapter.rpc.events[-1]
        assert "room" in adapter._typing_paused
        await adapter._on_answer(response(request, "once"))
        assert entry.event.is_set() and "room" not in adapter._typing_paused
        async def handler(choice): return None
        slash_confirm.register("session", "confirm-id", "reload", handler)
        adapter.pause_typing_for_chat("room")
        await adapter.send_slash_confirm("room", "Reload?", "Confirm", "session", "confirm-id")
        await adapter._on_answer(response(adapter.rpc.events[-1], "once"))
        assert "room" not in adapter._typing_paused
    asyncio.run(run())


def test_pending_requests_are_bounded_and_swept(adapter, monkeypatch):
    import interactions
    monkeypatch.setattr(interactions, "MAX_PENDING", 3)
    async def run():
        for index in range(5):
            clarify_gateway.register(f"q{index}", "session", "Pick", ["yes"])
            await adapter.send_clarify("room", "Pick", ["yes"], f"q{index}", "session")
        assert [entry.callback_id for entry in adapter._pending.values()] == ["q2", "q3", "q4"]
        stale = next(iter(adapter._pending.values()))
        stale.expires = int(time.time()) - 1
        adapter._sweep_pending()
        assert [entry.callback_id for entry in adapter._pending.values()] == ["q3", "q4"]
        # An answer sweeps too, so an expired card never lingers.
        list(adapter._pending.values())[0].expires = int(time.time()) - 1
        await adapter._on_answer({"kind": 1111, "tags": [["e", "0" * 64]], "pubkey": OWNER,
                                  "created_at": int(time.time()), "content": "c0"})
        assert [entry.callback_id for entry in adapter._pending.values()] == ["q4"]
    asyncio.run(run())


def test_receive_path_sweeps_pending_once_a_minute(adapter, monkeypatch):
    sweeps = []
    monkeypatch.setattr(adapter, "_sweep_pending", lambda now=None: sweeps.append(now))
    async def handle(message): pass
    adapter.handle_message = handle
    base = {"kind": 9, "pubkey": OWNER, "created_at": int(time.time()), "content": "hello",
            "tags": [["h", "room"], ["p", BOT]]}
    async def run():
        await adapter._on_event({**base, "id": "d" * 64})
        await adapter._on_event({**base, "id": "e" * 64})
    asyncio.run(run())
    assert len(sweeps) == 1
    adapter._last_sweep = time.monotonic() - adapter.SWEEP_INTERVAL
    asyncio.run(adapter._on_event({**base, "id": "f" * 64}))
    assert len(sweeps) == 2
