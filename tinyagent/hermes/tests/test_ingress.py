import json
import hashlib
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))

import ingress
from ingress import EventTracker, IdentityLock, event_thread, is_reply_to_own, reply_parent


def event(event_id, created=100, tags=None):
    return {"id": hashlib.sha256(event_id.encode()).hexdigest(), "created_at": created, "tags": tags or []}


def test_first_tracker_does_not_replay_history(tmp_path):
    tracker = EventTracker("http://relay", "a" * 64, ["lab"], state_dir=tmp_path, now=100)
    assert not tracker.should_accept(event("old", 99))
    assert tracker.should_accept(event("same", 100))


def test_persisted_cursor_overlaps_timestamp_and_deduplicates(tmp_path):
    first = EventTracker("r", "a", ["lab"], state_dir=tmp_path, now=100)
    first.mark(event("one", 101))
    second = EventTracker("r", "a", ["lab"], state_dir=tmp_path, now=100)
    assert second.filter_since()["since"] == 100
    assert second.should_accept(event("two", 101))
    assert not second.should_accept(event("one", 101))


def test_expiration_and_bounded_seen(tmp_path):
    tracker = EventTracker("r", "a", ["lab"], state_dir=tmp_path, now=100, max_seen=2)
    assert not tracker.should_accept(event("expired", 100, [["expiration", "100"]]))
    for index in range(3):
        tracker.mark(event(str(index), 101 + index))
    # The persisted set stays bounded. Over budget, the oldest second (101) is retired
    # and the floor moves to the oldest retained second (102), so the two newest IDs
    # remain deduplicated instead of being forgotten.
    state = json.loads(tracker.path.read_text())
    assert len(state["seen"]) == 2
    assert set(state["seen"].values()) == {102, 103}
    assert state["floor"] == 102


def test_nip10_root_reply_and_own_parent():
    root = "r" * 64
    parent = "p" * 64
    child = event("c", tags=[["e", root, "", "root"], ["e", parent, "", "reply"]])
    assert event_thread(child) == root
    assert reply_parent(child) == parent
    assert is_reply_to_own(child, {parent})


def test_overlap_accepts_out_of_order_events_and_survives_restart(tmp_path):
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100)
    tracker.mark(event('newer', 400))
    assert tracker.should_accept(event('earlier', 200))
    tracker.mark(event('earlier', 200))
    restored = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=500)
    assert restored.filter_since()['since'] == 100
    assert not restored.should_accept(event('earlier', 200))
    assert restored.should_accept(event('same-time', 400))


def test_evicted_ids_cannot_become_commands_again(tmp_path):
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100, max_seen=2)
    for i in range(3): tracker.mark(event(str(i), 101 + i))
    # ID 0 (second 101) was retired by the overflow; the floor now rejects that second.
    assert not tracker.should_accept(event('0', 101))
    assert not tracker.should_accept(event('1', 102))
    assert tracker.should_accept(event('next', 104))


def test_overflow_keeps_same_second_and_replay_window_events(tmp_path):
    # Overflowing the ID cache may retire old IDs, but never events the relay can
    # still legitimately deliver: unseen events in the cursor's own second and
    # out-of-order events inside the replay window.
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100, max_seen=3)
    tracker.mark(event('a', 101))
    tracker.mark(event('b', 102))
    tracker.mark(event('c', 103))
    tracker.mark(event('d', 103))
    assert tracker.should_accept(event('same-second', 103))
    assert tracker.should_accept(event('overlap', 102))
    assert not tracker.should_accept(event('b', 102))
    assert not tracker.should_accept(event('d', 103))
    assert not tracker.should_accept(event('a', 101))


def test_window_eviction_does_not_raise_floor(tmp_path):
    # IDs older than the replay window are dropped without touching the floor, so a
    # large cache never has to retire live seconds.
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100, max_seen=2)
    tracker.mark(event('old-1', 101))
    tracker.mark(event('old-2', 102))
    tracker.mark(event('new', 1000))
    assert tracker.floor == 100
    assert set(tracker.seen.values()) == {1000}
    assert tracker.filter_since()['since'] == 700
    assert not tracker.should_accept(event('old-1', 101))


def test_legacy_seen_list_loads(tmp_path):
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100)
    legacy_id = event('legacy', 105)['id']
    tracker.path.write_text(json.dumps({"floor": 100, "cursor": 105, "seen": [legacy_id]}))
    restored = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=200)
    assert restored.cursor == 105
    assert not restored.should_accept(event('legacy', 105))
    assert restored.should_accept(event('fresh', 105))
    state = json.loads(restored.path.read_text())
    assert state["seen"] == {legacy_id: 105}
    assert state["attempts"] == {}


def test_failed_handoffs_retire_after_three_attempts(tmp_path):
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100)
    poison = event('poison', 101)
    assert not tracker.fail(poison)
    assert tracker.should_accept(poison)
    restored = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100)
    assert restored.attempts == {poison["id"]: 1}
    assert not restored.fail(poison)
    assert restored.fail(poison)
    assert not restored.should_accept(poison)
    assert restored.attempts == {}
    assert json.loads(restored.path.read_text())["attempts"] == {}


def test_attempt_counts_are_bounded(tmp_path):
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path, now=100)
    for index in range(ingress._MAX_ATTEMPT_ENTRIES + 5):
        tracker.fail(event(f'fail-{index}', 101))
    assert len(tracker.attempts) == ingress._MAX_ATTEMPT_ENTRIES
    assert event('fail-0', 101)["id"] not in tracker.attempts


def test_state_root_prefers_hermes_home_env(tmp_path, monkeypatch):
    monkeypatch.setitem(sys.modules, "hermes_cli", None)
    monkeypatch.setitem(sys.modules, "hermes_cli.config", None)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "home"))
    assert ingress.state_root() == tmp_path / "home"
    monkeypatch.delenv("HERMES_HOME")
    assert ingress.state_root() == Path("~/.hermes").expanduser()
    tracker = EventTracker('r', 'a', ['lab'], state_dir=tmp_path)
    assert tracker.path.parent == tmp_path / "state" / "tinyagent"


def test_state_root_uses_hermes_active_home(tmp_path, monkeypatch):
    import types
    fake = types.ModuleType("hermes_cli.config")
    fake.get_hermes_home = lambda: tmp_path / "profile"
    package = types.ModuleType("hermes_cli")
    package.config = fake
    monkeypatch.setitem(sys.modules, "hermes_cli", package)
    monkeypatch.setitem(sys.modules, "hermes_cli.config", fake)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "ignored"))
    assert ingress.state_root() == tmp_path / "profile"


def test_identity_lock_is_exclusive_in_process(tmp_path):
    first = IdentityLock("http://relay", "A" * 64, state_dir=tmp_path)
    second = IdentityLock("http://relay", "a" * 64, state_dir=tmp_path)
    first.acquire()
    first.acquire()
    assert first.held and first.path == second.path
    with pytest.raises(RuntimeError, match="another Hermes gateway already runs this Tiny identity"):
        second.acquire()
    assert not second.held
    other = IdentityLock("http://relay", "b" * 64, state_dir=tmp_path)
    other.acquire()
    first.release()
    second.acquire()
    assert second.held and not first.held
    second.release()
    other.release()


def test_identity_lock_is_exclusive_across_processes(tmp_path):
    lock = IdentityLock("http://relay", "a" * 64, state_dir=tmp_path)
    script = textwrap.dedent(f"""
        import sys
        sys.path.insert(0, {str(Path(__file__).parents[1])!r})
        from ingress import IdentityLock
        lock = IdentityLock("http://relay", "a" * 64, state_dir={str(tmp_path)!r})
        lock.acquire()
        print("held", flush=True)
        sys.stdin.readline()
        lock.release()
    """)
    holder = subprocess.Popen([sys.executable, "-c", script], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    try:
        assert holder.stdout.readline().strip() == "held"
        with pytest.raises(RuntimeError):
            lock.acquire()
    finally:
        holder.stdin.write("done\n")
        holder.stdin.close()
        holder.wait(timeout=10)
    lock.acquire()
    assert lock.held
    lock.release()
