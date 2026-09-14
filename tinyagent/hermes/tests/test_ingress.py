import json
import hashlib
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[1]))

from ingress import EventTracker, event_thread, is_reply_to_own, reply_parent


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
    assert len(json.loads(tracker.path.read_text())["seen"]) == 2


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
    assert not tracker.should_accept(event('0', 101))
    assert tracker.should_accept(event('next', 104))
