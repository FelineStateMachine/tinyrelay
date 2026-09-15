"""Hermes-free rules for long-task cards: intake text, progress coalescing and results."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[1]))

from tasks import CHROME, FILE, TEXT, TaskState, answer_tags, clip, last_line, request_root, request_text

OWNER = "b" * 64


def task(**overrides):
    fields = dict(request_id="1" * 64, room="room", requester=OWNER, thread_root="1" * 64, session_key="s")
    fields.update(overrides)
    return TaskState(**fields)


def test_request_text_prefixes_subject_and_falls_back():
    assert request_text({"content": "Do the thing", "tags": [["subject", "Nightly build"]]}) == "Nightly build\n\nDo the thing"
    assert request_text({"content": "", "tags": [["subject", "Nightly build"]]}) == "Nightly build"
    assert request_text({"content": "Do the thing", "tags": []}) == "Do the thing"


def test_request_root_prefers_marked_root():
    assert request_root({"id": "1" * 64, "tags": [["e", "2" * 64, "", "root"]]}) == "2" * 64
    assert request_root({"id": "1" * 64, "tags": [["e", "2" * 64]]}) == "1" * 64


def test_progress_coalesces_latest_text_and_tool_line():
    state = task(accepted=True)
    state.observe("m1", "Looking at the repo ▉", TEXT)
    state.observe("m2", "\U0001f527 terminal: \"ls\"", CHROME)
    state.observe("m2", "\U0001f527 terminal: \"ls\"\n\U0001f50d web_search: \"docs\"", CHROME)
    state.observe("m1", "Looking at the repo\nStill reading the docs ▉", CHROME)
    assert state.has_progress
    assert state.take_progress(now=100.0) == "Still reading the docs\n\U0001f50d web_search: \"docs\""
    assert not state.has_progress and state.last_progress == 100.0
    assert state.progress_due(now=110.0, interval=15.0) == 5.0
    assert state.progress_due(now=115.0, interval=15.0) == 0.0
    assert task().progress_due(now=0.0, interval=15.0) == 0.0


def test_result_links_final_message_with_summary_line():
    state = task()
    state.observe("draft", "Working on it", TEXT)
    state.observe("final", "  First line of the answer.\nMore detail below.", TEXT, final=True)
    assert not state.has_progress
    content, tags = state.result()
    assert content == "First line of the answer."
    assert tags == [["e", "final"]]


def test_result_uses_last_text_message_without_a_final_mark():
    state = task()
    state.observe("first", "One", TEXT)
    state.observe("chrome", "\U0001f527 tool", CHROME)
    state.observe("second", "Two " * 100, TEXT)
    content, tags = state.result()
    assert content.endswith("...") and len(content) == 200
    assert tags == [["e", "second"]]


def test_result_names_posted_files():
    state = task()
    state.observe("f1", "", FILE, file_name="report.pdf")
    state.observe("f2", "caption", FILE, file_name="chart.png")
    assert state.result() == ("Posted report.pdf, chart.png", [["e", "f1"], ["e", "f2"]])
    state.observe("final", "Here you go", TEXT, final=True)
    assert state.result() == ("Here you go\nPosted report.pdf, chart.png", [["e", "final"], ["e", "f1"], ["e", "f2"]])


def test_result_without_posts_uses_returned_text_or_done():
    state = task()
    assert state.result() == ("Done", [])
    state.final_text = "Quiet answer"
    assert state.result() == ("Quiet answer", [])


def test_terminal_guard_and_failure_text():
    state = task()
    assert state.failure() == "The task failed"
    state.note_error("  boom\nline two  ")
    assert state.failure() == "boom line two"
    assert state.end() and not state.end()
    state.observe("late", "ignored", TEXT)
    assert state.messages == {} and not state.has_progress


def test_helpers():
    assert answer_tags(task()) == [["e", "1" * 64], ["p", OWNER]]
    assert clip("a  b\n c", 5) == "a b c" and clip("abcdef", 5) == "ab..."
    assert last_line("one\ntwo ▉\n\n") == "two"
