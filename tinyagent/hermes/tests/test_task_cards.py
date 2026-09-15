"""Long-task cards through the real Hermes processing pipeline."""

import asyncio
import hashlib
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
from adapter import TinyAdapter
from gateway.config import PlatformConfig
from gateway.platform_registry import PlatformEntry, platform_registry
from gateway.platforms.event import ProcessingOutcome
from ingress import EventTracker

OWNER = "b" * 64
BOT = "a" * 64
OTHER = "c" * 64
STRANGER = "f" * 64
REQUEST = "d" * 64


def tag(event, name):
    return next((row[1] for row in event.get("tags", []) if len(row) > 1 and row[0] == name), None)


class FakeRPC:
    def __init__(self):
        self.events = []
        self.rows = []
        self.terminal = []
        self.fail_kinds = set()
        # kind -> number of publishes of that kind still to refuse
        self.fail_times = {}
        self.attempts = []

    async def query(self, filter):
        if "ids" in filter:
            return [row for row in self.rows if row["id"] in filter["ids"]]
        if "#e" in filter:
            return [row for row in self.terminal
                    if row["kind"] in filter.get("kinds", []) and tag(row, "e") in filter["#e"]]
        return []

    async def publish(self, event):
        self.attempts.append(event["kind"])
        if event["kind"] in self.fail_kinds or self.fail_times.get(event["kind"], 0) > 0:
            self.fail_times[event["kind"]] = self.fail_times.get(event["kind"], 0) - 1
            raise RuntimeError("relay refused the event")
        result = {"id": f"{len(self.events) + 1:064x}", "created_at": int(time.time()), "pubkey": BOT, **event}
        self.events.append(result)
        return result

    async def upload(self, room, filename, mime, data):
        return {"url": f"https://tiny.test/media/{filename}", "sha256": hashlib.sha256(data).hexdigest()}

    def kinds(self, kind):
        return [event for event in self.events if event["kind"] == kind]


@pytest.fixture
def adapter(monkeypatch, tmp_path):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    platform_registry.register(PlatformEntry(name="tiny", label="Tiny", adapter_factory=TinyAdapter,
                                            check_fn=lambda: True))
    instance = TinyAdapter(PlatformConfig(enabled=True, extra={"rooms": "room", "allowed_users": f"{OWNER},{OTHER}"}))
    instance.rpc = FakeRPC()
    instance.pubkey = BOT
    yield instance


def request(request_id=REQUEST, *, author=OWNER, assignee=BOT, room="room", subject=None, content="Run the build",
            root=None):
    tags = [["h", room], ["p", assignee]]
    if subject:
        tags.append(["subject", subject])
    if root:
        tags.append(["e", root, "", "root"])
    return {"kind": 43001, "id": request_id, "pubkey": author, "created_at": int(time.time()),
            "content": content, "tags": tags}


def cancel(request_id=REQUEST, *, author=OWNER, event_id="9" * 64):
    return {"kind": 43005, "id": event_id, "pubkey": author, "created_at": int(time.time()), "content": "",
            "tags": [["e", request_id], ["h", "room"]]}


def bang_cancel(*, author=OTHER, kind=9, event_id="8" * 64, extra=None):
    return {"kind": kind, "id": event_id, "pubkey": author, "created_at": int(time.time()), "content": " !cancel \n",
            "tags": [["h", "room"], ["p", BOT], *(extra or [])]}


def answer_tags(event):
    return [row for row in event["tags"] if row[0] in ("e", "p", "h")]


async def settle(rounds=5):
    for _ in range(rounds):
        await asyncio.sleep(0)


def admit(message):
    """What Hermes's handle_message records once it starts or queues a turn."""
    message._gateway_accepted = True


def capture(adapter):
    received = []
    async def handle(message):
        received.append(message)
        admit(message)
    adapter.handle_message = handle
    return received


def test_request_becomes_a_thread_turn_anchored_at_the_request(adapter):
    received = capture(adapter)
    asyncio.run(adapter._on_event(request(subject="Nightly build", content="Run it and report.")))
    message = received[0]
    assert message.text == "Nightly build\n\nRun it and report."
    assert message.message_id == REQUEST and message.source.thread_id == REQUEST
    assert message.source.chat_id == "room" and message.source.user_id == OWNER
    task = adapter._tasks[REQUEST]
    assert task.requester == OWNER and task.thread_root == REQUEST and not task.accepted
    assert adapter._session_assignees[task.session_key] == OWNER
    assert adapter.rpc.events == []
    # A request inside an existing thread keeps its own session, named by the request,
    # while its replies sit in that thread under the request.
    asyncio.run(adapter._on_event(request("e" * 64, root="7" * 64, content="Continue here")))
    assert received[1].text == "Continue here" and received[1].source.thread_id == "e" * 64
    assert adapter._tasks["e" * 64].thread_root == "7" * 64
    assert adapter._tasks["e" * 64].session_key != task.session_key
    reply = asyncio.run(adapter.send("room", "On it", reply_to="e" * 64, metadata={"thread_id": "e" * 64}))
    assert reply.success and adapter.rpc.events[-1]["kind"] == 12
    assert ["e", "7" * 64, "", "root"] in adapter.rpc.events[-1]["tags"]
    assert ["e", "e" * 64, "", "reply"] in adapter.rpc.events[-1]["tags"]
    assert adapter._tasks["e" * 64].classes == {reply.message_id: "chrome"}


@pytest.mark.parametrize("mutation", ["author", "assignee", "room"])
def test_requests_need_an_allowed_author_our_key_and_a_watched_room(adapter, mutation):
    received = capture(adapter)
    changes = {"author": {"author": STRANGER}, "assignee": {"assignee": OTHER}, "room": {"room": "elsewhere"}}
    asyncio.run(adapter._on_event(request(**changes[mutation])))
    assert received == [] and adapter._tasks == {} and adapter.rpc.events == []


@pytest.mark.parametrize("kind", [43004, 43005, 43006])
def test_finished_requests_are_not_run_again(adapter, tmp_path, kind):
    received = capture(adapter)
    adapter._tracker = EventTracker("http://relay", BOT, ["room"], state_dir=tmp_path, now=int(time.time()) - 10)
    adapter.rpc.terminal = [{"kind": kind, "tags": [["e", REQUEST], ["h", "room"]]}]
    event = request()
    asyncio.run(adapter._on_event(event))
    assert received == [] and adapter._tasks == {} and adapter.rpc.events == []
    assert not adapter._tracker.should_accept(event)


def test_accepted_is_published_when_processing_starts(adapter):
    received = capture(adapter)
    async def run():
        await adapter._on_event(request())
        assert adapter.rpc.events == []
        await adapter.on_processing_start(received[0])
        await adapter.on_processing_start(received[0])
        accepted = adapter.rpc.kinds(43002)
        assert len(accepted) == 1 and accepted[0]["content"] == ""
        assert answer_tags(accepted[0]) == [["h", "room"], ["e", REQUEST], ["p", OWNER]]
        assert adapter._tasks[REQUEST].accepted
    asyncio.run(run())


def test_progress_is_coalesced_and_throttled(adapter, monkeypatch):
    received = capture(adapter)
    clock = [1000.0]
    monkeypatch.setattr(adapter, "_clock", lambda: clock[0])
    thread = {"thread_id": REQUEST}
    async def run():
        await adapter._on_event(request())
        await adapter.send("room", "Before accept", metadata=thread)
        await settle()
        assert adapter.rpc.kinds(43003) == []
        await adapter.on_processing_start(received[0])
        chrome = await adapter.send("room", "\U0001f527 terminal: \"ls\"", metadata=thread)
        await settle()
        progress = adapter.rpc.kinds(43003)
        assert [event["content"] for event in progress] == ["\U0001f527 terminal: \"ls\""]
        assert answer_tags(progress[0]) == [["h", "room"], ["e", REQUEST], ["p", OWNER]]
        await adapter.edit_message("room", chrome.message_id, "\U0001f527 terminal: \"ls\"\n\U0001f50d web_search: \"docs\"")
        draft = await adapter.send("room", "Reading the docs ▉", metadata={**thread, "expect_edits": True})
        await adapter.edit_message("room", draft.message_id, "Reading the docs\nComparing versions ▉")
        await settle()
        assert len(adapter.rpc.kinds(43003)) == 1
        task = adapter._tasks[REQUEST]
        assert task.timer is not None and task.has_progress
        clock[0] += adapter.PROGRESS_INTERVAL
        await adapter._flush_progress(REQUEST)
        progress = adapter.rpc.kinds(43003)
        assert len(progress) == 2
        assert progress[-1]["content"] == "Comparing versions\n\U0001f50d web_search: \"docs\""
        clock[0] += adapter.PROGRESS_INTERVAL
        await adapter._flush_progress(REQUEST)
        assert len(adapter.rpc.kinds(43003)) == 2
        assert [event["kind"] for event in adapter.rpc.events if event["kind"] in (9, 12)] == [12, 12, 12]
        assert all(["e", REQUEST, "", "root"] in event["tags"] for event in adapter.rpc.events if event["kind"] == 12)
    asyncio.run(run())


def pipeline(adapter, handler):
    seen = []
    async def wrapped(message):
        seen.append(message)
        return await handler(message)
    adapter.set_message_handler(wrapped)
    async def process(message):
        admit(message)
        await adapter._process_message_background(message, adapter._event_session_key(message))
    adapter.handle_message = process
    return seen


def grade_like_0_21_1(adapter):
    """Hermes 0.21.1 does not count attachment delivery: a file-only turn grades as failure."""
    original = adapter.on_processing_complete
    async def complete(event, outcome):
        posted_text = any(event["kind"] == 12 and not any(row[0] == "imeta" for row in event["tags"])
                          for event in adapter.rpc.events)
        if outcome == ProcessingOutcome.SUCCESS and not posted_text:
            outcome = ProcessingOutcome.FAILURE
        await original(event, outcome)
    adapter.on_processing_complete = complete


async def finish_turns(adapter, timeout=5):
    tasks = list(adapter._session_tasks.values())
    if tasks:
        await asyncio.wait_for(asyncio.gather(*tasks, return_exceptions=True), timeout)
    await settle()


def test_success_publishes_one_result_linking_the_thread_reply(adapter):
    async def handler(message):
        return "Build passed on main.\nAll 42 tests green."
    seen = pipeline(adapter, handler)
    asyncio.run(adapter._on_event(request()))
    events = adapter.rpc.events
    replies = [event for event in events if event["kind"] == 12]
    assert len(replies) == 1 and ["e", REQUEST, "", "root"] in replies[0]["tags"]
    results = adapter.rpc.kinds(43004)
    assert len(results) == 1
    assert results[0]["content"] == "Build passed on main."
    assert answer_tags(results[0]) == [["h", "room"], ["e", REQUEST], ["p", OWNER], ["e", replies[0]["id"]]]
    order = [event["kind"] for event in events if event["kind"] in (43002, 12, 43004)]
    assert order == [43002, 12, 43004]
    assert adapter.rpc.kinds(43006) == [] and adapter.rpc.kinds(43003) == [] and REQUEST not in adapter._tasks
    asyncio.run(adapter.on_processing_complete(seen[0], ProcessingOutcome.SUCCESS))
    assert len(adapter.rpc.kinds(43004)) == 1


@pytest.mark.parametrize("hermes", ["0.21.3", "0.21.1"])
def test_file_only_result_names_and_links_the_files(adapter, tmp_path, hermes):
    note = tmp_path / "report.txt"
    note.write_text("build log")
    async def handler(message):
        return f"MEDIA:{note}"
    pipeline(adapter, handler)
    if hermes == "0.21.1":
        grade_like_0_21_1(adapter)
    asyncio.run(adapter._on_event(request()))
    files = [event for event in adapter.rpc.events if any(row[0] == "imeta" for row in event["tags"])]
    assert len(files) == 1 and files[0]["kind"] == 12
    results = adapter.rpc.kinds(43004)
    assert len(results) == 1 and results[0]["content"] == "Posted report.txt"
    assert ["e", files[0]["id"]] in results[0]["tags"]
    assert adapter.rpc.kinds(43006) == [] and REQUEST not in adapter._tasks


@pytest.mark.parametrize("shape", ["nothing-delivered", "send-failed"])
def test_true_failures_still_publish_an_error(adapter, shape):
    async def handler(message):
        return "Build passed."
    seen = pipeline(adapter, handler)
    if shape == "nothing-delivered":
        # Hermes graded the turn as failed and the adapter saw no reply reach the room.
        original = adapter.on_processing_complete
        async def complete(event, outcome):
            adapter._tasks[REQUEST].classes.clear()
            await original(event, ProcessingOutcome.FAILURE)
        adapter.on_processing_complete = complete
    else:
        adapter.rpc.fail_kinds = {12}
    asyncio.run(adapter._on_event(request()))
    errors = adapter.rpc.kinds(43006)
    assert len(errors) == 1 and adapter.rpc.kinds(43004) == [] and REQUEST not in adapter._tasks
    expected = "The task failed" if shape == "nothing-delivered" else "relay refused the event"
    assert errors[0]["content"] == expected


def test_failure_publishes_one_error_with_the_exposed_text(adapter):
    async def handler(message):
        raise RuntimeError("model exploded")
    pipeline(adapter, handler)
    asyncio.run(adapter._on_event(request()))
    errors = adapter.rpc.kinds(43006)
    assert len(errors) == 1 and errors[0]["content"] == "model exploded"
    assert answer_tags(errors[0]) == [["h", "room"], ["e", REQUEST], ["p", OWNER]]
    notice = [event for event in adapter.rpc.events if event["kind"] == 12]
    assert len(notice) == 1 and "model exploded" in notice[0]["content"]
    assert adapter.rpc.kinds(43004) == [] and REQUEST not in adapter._tasks


def test_terminal_publication_is_retried_and_happens_once(adapter):
    async def handler(message):
        return "Done"
    seen = pipeline(adapter, handler)
    adapter.TERMINAL_RETRY_DELAYS = (0, 0)
    adapter.rpc.fail_times = {43004: 2}
    asyncio.run(adapter._on_event(request()))
    assert adapter.rpc.attempts.count(43004) == 3
    assert len(adapter.rpc.kinds(43004)) == 1 and adapter.rpc.kinds(43006) == []
    assert REQUEST not in adapter._tasks
    asyncio.run(adapter.on_processing_complete(seen[0], ProcessingOutcome.SUCCESS))
    asyncio.run(adapter.on_processing_complete(seen[0], ProcessingOutcome.FAILURE))
    assert len(adapter.rpc.kinds(43004)) == 1 and adapter.rpc.kinds(43006) == []


def test_terminal_publication_gives_up_after_three_attempts(adapter):
    async def handler(message):
        return "Done"
    seen = pipeline(adapter, handler)
    adapter.TERMINAL_RETRY_DELAYS = (0, 0)
    adapter.rpc.fail_kinds = {43004}
    forgotten = []
    original = adapter._forget_task
    def forget(request_id):
        forgotten.append(request_id in adapter._tasks)
        original(request_id)
    adapter._forget_task = forget
    asyncio.run(adapter._on_event(request()))
    assert adapter.rpc.attempts.count(43004) == 3
    # The task stayed known until the last attempt, then was dropped exactly once.
    assert forgotten[0] and REQUEST not in adapter._tasks
    assert adapter.rpc.kinds(43004) == [] and adapter.rpc.kinds(43006) == []
    adapter.rpc.fail_kinds = set()
    asyncio.run(adapter.on_processing_complete(seen[0], ProcessingOutcome.SUCCESS))
    assert adapter.rpc.kinds(43004) == []


def test_requests_sharing_a_thread_run_as_separate_turns(adapter):
    # Three requests in one thread arrive while the first still runs. Each gets its
    # own session, so Hermes never folds two requests into one turn: three handler
    # calls, three accepted events, three results, no orphaned card.
    adapter._busy_text_mode = "queue"
    adapter._busy_text_debounce_seconds = 0
    release = asyncio.Event()
    seen = []
    async def handler(message):
        seen.append(message)
        if len(seen) == 1:
            await release.wait()
        return f"Finished {message.text}"
    adapter.set_message_handler(handler)
    common = "7" * 64
    ids = [f"{index + 10:064x}" for index in range(3)]
    async def run():
        for index, request_id in enumerate(ids):
            await adapter._on_event(request(request_id, root=common, content=f"job {index}"))
            await settle()
        assert [message.text for message in seen] == ["job 0", "job 1", "job 2"]
        assert adapter._pending_messages == {}
        release.set()
        await finish_turns(adapter)
    asyncio.run(run())
    assert sorted(tag(event, "e") for event in adapter.rpc.kinds(43002)) == ids
    results = adapter.rpc.kinds(43004)
    assert sorted(tag(event, "e") for event in results) == ids
    assert adapter.rpc.kinds(43006) == [] and adapter._tasks == {}
    replies = [event for event in adapter.rpc.events if event["kind"] == 12]
    assert len(replies) == 3
    assert all(["e", common, "", "root"] in event["tags"] for event in replies)
    assert sorted(row[1] for event in replies for row in event["tags"] if len(row) > 3 and row[3] == "reply") == ids
    for result in results:
        reply = next(event for event in replies if ["e", tag(result, "e"), "", "reply"] in event["tags"])
        assert ["e", reply["id"]] in result["tags"] and result["content"] == reply["content"]


def test_requests_merged_by_hermes_end_with_an_error_naming_the_carrier(adapter):
    # Safety net for a Hermes that still coalesces two requests into one turn: the
    # request that lost its turn gets an error card naming the turn that carried it.
    received = capture(adapter)
    ids = [f"{index + 10:064x}" for index in range(3)]
    async def run():
        for index, request_id in enumerate(ids):
            await adapter._on_event(request(request_id, content=f"job {index}"))
        first, second, third = (adapter._tasks[request_id] for request_id in ids)
        second.session_key = third.session_key = first.session_key
        await adapter.on_processing_start(received[0])
        # The third request is still queued under the session: it keeps its card.
        adapter._pending_messages[first.session_key] = received[2]
        await adapter.on_processing_complete(received[0], ProcessingOutcome.SUCCESS)
        errors = adapter.rpc.kinds(43006)
        assert [tag(event, "e") for event in errors] == [ids[1]]
        assert errors[0]["content"] == f"Merged into task {ids[2]}"
        assert [tag(event, "e") for event in adapter.rpc.kinds(43004)] == [ids[0]]
        assert ids[0] not in adapter._tasks and ids[1] not in adapter._tasks and ids[2] in adapter._tasks
        adapter._pending_messages.clear()
        await adapter.on_processing_start(received[2])
        await adapter.on_processing_complete(received[2], ProcessingOutcome.SUCCESS)
        assert adapter._tasks == {} and len(adapter.rpc.kinds(43004)) == 2
    asyncio.run(run())


def slow_turn(adapter):
    """Install a handler that never finishes on its own; the real handle_message spawns it."""
    seen = []
    async def handler(message):
        seen.append(message)
        await asyncio.sleep(30)
        return "never"
    adapter.set_message_handler(handler)
    return seen


def test_cancelled_turn_publishes_cancelled(adapter):
    seen = slow_turn(adapter)
    async def run():
        await adapter._on_event(request())
        await settle()
        assert len(seen) == 1 and adapter._tasks[REQUEST].accepted
        await adapter.cancel_session_processing(adapter._tasks[REQUEST].session_key)
        await settle()
        errors = adapter.rpc.kinds(43006)
        assert len(errors) == 1 and errors[0]["content"] == "Cancelled"
        assert adapter.rpc.kinds(43004) == [] and REQUEST not in adapter._tasks
    asyncio.run(run())


def test_requester_cancel_ends_the_task_silently(adapter):
    seen = slow_turn(adapter)
    async def run():
        await adapter._on_event(request())
        await settle()
        session = adapter._tasks[REQUEST].session_key
        await adapter._on_event(cancel(author=OTHER))
        assert REQUEST in adapter._tasks and not adapter._session_tasks[session].done()
        await adapter._on_event(cancel())
        await settle()
        assert REQUEST not in adapter._tasks
        assert session not in adapter._session_tasks
        assert adapter.rpc.kinds(43006) == [] and adapter.rpc.kinds(43004) == []
        assert len(seen) == 1
    asyncio.run(run())


@pytest.mark.parametrize("shape", ["thread", "room-reference", "room-bare"])
def test_bang_cancel_stops_a_task_and_names_the_canceller(adapter, shape):
    seen = slow_turn(adapter)
    async def run():
        await adapter._on_event(request())
        await settle()
        session = adapter._tasks[REQUEST].session_key
        if shape == "thread":
            event = bang_cancel(kind=12, extra=[["e", REQUEST, "", "root"]])
        elif shape == "room-reference":
            event = bang_cancel(extra=[["e", REQUEST]])
        else:
            event = bang_cancel()
        await adapter._on_event(event)
        await settle()
        errors = adapter.rpc.kinds(43006)
        assert len(errors) == 1 and errors[0]["content"] == f"Cancelled by {OTHER}"
        assert REQUEST not in adapter._tasks and session not in adapter._session_tasks
        assert len(seen) == 1
    asyncio.run(run())


def test_bang_cancel_stops_an_ordinary_turn_without_forwarding(adapter):
    seen = slow_turn(adapter)
    cancelled = []
    async def cancel_session(session_key, **kwargs):
        cancelled.append(session_key)
    adapter.cancel_session_processing = cancel_session
    async def run():
        plain = {"kind": 9, "id": "5" * 64, "pubkey": OWNER, "created_at": int(time.time()),
                 "content": "hello", "tags": [["h", "room"], ["p", BOT]]}
        await adapter._on_event(plain)
        await settle()
        assert len(seen) == 1
        await adapter._on_event(bang_cancel(author=OWNER))
        assert cancelled == [adapter._event_session_key(seen[0])]
        assert len(seen) == 1 and {event["kind"] for event in adapter.rpc.events} <= {20002}
        # Without our p tag the text is an ordinary message and follows the mention rule.
        await adapter._on_event({**bang_cancel(author=OWNER, event_id="6" * 64), "tags": [["h", "room"]]})
        assert len(seen) == 1 and cancelled == [adapter._event_session_key(seen[0])]
        for task in list(adapter._session_tasks.values()):
            task.cancel()
        await asyncio.gather(*adapter._session_tasks.values(), return_exceptions=True)
    asyncio.run(run())


def test_create_handoff_thread_opens_a_real_thread(adapter):
    async def run():
        thread_id = await adapter.create_handoff_thread("room", "Hermes - review")
        root = adapter.rpc.events[-1]
        assert thread_id == root["id"] and root["kind"] == 11 and root["content"] == "Hermes - review"
        assert root["tags"] == [["h", "room"]]
        result = await adapter.send("room", "Continuing here", metadata={"thread_id": thread_id})
        reply = adapter.rpc.events[-1]
        assert result.success and reply["kind"] == 12 and ["e", thread_id, "", "root"] in reply["tags"]
        assert await adapter.create_handoff_thread("room", "") is not None
        assert adapter.rpc.events[-1]["content"] == "Handoff"
        adapter.rpc.fail_kinds = {11}
        assert await adapter.create_handoff_thread("room", "Broken") is None
    asyncio.run(run())
