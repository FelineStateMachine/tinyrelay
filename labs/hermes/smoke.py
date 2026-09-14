"""Exercise unchanged Hermes over the native Tiny connection."""

import base64
import hashlib
import json
import time
import uuid
from urllib.parse import urlsplit

from rpc import Client, answer, public, tag, wait_event


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def send(client, text, extra=None):
    return client.publish(9, text, [["h", "lab"], ["p", public("agent")], *(extra or [])])


def done(client, marker, since):
    event = wait_event(client, {"kinds": [9, 40003], "authors": [public("agent")],
                               "#h": ["lab"], "since": since},
                       lambda row: f"LAB_DONE {marker}" in row["content"])
    check(not any(row[0] in ("p", "mention") for row in event["tags"]),
          "Ordinary agent output unexpectedly mentions someone")
    return event


def interaction(client, scenario, run):
    marker = f"LAB_{scenario}_{run}"
    prior = {row["id"] for row in client.query({"kinds": [9], "#h": ["lab"], "#request": ["question"]})}
    sent = send(client, marker)
    request = wait_event(client, {"kinds": [9], "authors": [public("agent")], "#h": ["lab"],
                                  "#request": ["question"], "since": sent["created_at"]},
                         lambda row: row["id"] not in prior)
    check(tag(request, "tinyagent") == "1", "Request lost its native interaction marker")
    check(tag(request, "p") == public("owner"), "Request does not name its human assignee")
    check(tag(request, "mention") is None, "Assignee was turned into an unsolicited mention")
    check(int(tag(request, "expiration") or 0) > time.time(), "Request has no future expiry")
    options = {row[1]: row[2] for row in request["tags"] if len(row) >= 3 and row[0] == "option"}
    check(bool(options) or scenario == "OPEN", "Options were flattened into text")
    if scenario == "OPEN":
        check(tag(request, "selection") == "text", "Open question lost its text input")
        choice = "Custom lab answer"
    elif scenario == "APPROVAL":
        check(tag(request, "interaction") == "approval", "Approval lost its type")
        choice = next(key for key in options if key == "once")
    else:
        check(tag(request, "interaction") == "question", "Question lost its type")
        choice = next(key for key, label in options.items() if label.startswith("Continue"))
    if scenario == "MULTI":
        check(tag(request, "selection") == "multiple", "Multiple choice became single choice")
        choice = json.dumps([key for key, label in options.items() if label.startswith(("Continue", "Stop"))])
    if scenario == "CUSTOM":
        check(tag(request, "freeform") == "true", "Choice question lost its custom input")
        choice = json.dumps({"text": "Custom lab answer"})
    # A signed response from another room member must not resolve the operator's request.
    with Client("observer") as observer:
        answer(observer, request, choice)
    answer(client, request, "" if scenario == "OPEN" else "option-that-was-not-offered")
    time.sleep(0.5)
    state = client.browse("browseapproval", {"id": request["id"]})
    check(state["item"]["state"] == "open", "An unauthorized or invalid answer settled the card")
    answer(client, request, choice)
    final = done(client, marker, sent["created_at"])
    expected = "tinyagent-lab" if scenario == "APPROVAL" else "Custom lab answer" if scenario in ("OPEN", "CUSTOM") else "Continue"
    check(expected in final["content"], "Hermes did not receive the selected response")
    if scenario == "MULTI":
        check("Stop" in final["content"], "Hermes lost a selected choice")
    state = client.browse("browseapproval", {"id": request["id"]})
    check(state["item"]["state"] == "answered", "The valid answer did not settle the card")
    print(f"PASS {scenario.lower()}: native card, silent assignment, authorized answer, Hermes resumed")


def media(client, run):
    marker = f"LAB_MEDIA_{run}"
    content = f"tinyagent-attachment-{run}\n".encode()
    descriptor = client.call("upload", {"room": "lab", "filename": "lab-note.txt", "type": "text/plain",
                                         "data": base64.b64encode(content).decode()})
    digest = hashlib.sha256(content).hexdigest()
    check(descriptor["sha256"] == digest, "Uploaded attachment hash differs")
    path = urlsplit(descriptor["url"]).path
    downloaded = client.call("download", {"path": path})
    check(base64.b64decode(downloaded["data"]) == content, "Authenticated download differs")
    sent = send(client, marker, [["imeta", f"url {descriptor['url']}", "m text/plain", f"x {digest}",
                                  f"size {len(content)}", "filename lab-note.txt"]])
    final = done(client, marker, sent["created_at"])
    check(f"tinyagent-attachment-{run}" in final["content"], "Attachment text did not reach Hermes")
    returned = wait_event(client, {"kinds": [9], "authors": [public("agent")], "#h": ["lab"],
                                    "since": sent["created_at"]},
                          lambda row: any(t[0] == "imeta" and f"x {digest}" in t for t in row["tags"]))
    attachment = next(t for t in returned["tags"] if t[0] == "imeta")
    url = next(t[4:] for t in attachment if t.startswith("url "))
    check(base64.b64decode(client.call("download", {"path": urlsplit(url).path})["data"]) == content,
          "Hermes outbound attachment differs")
    print("PASS media: authenticated upload, Hermes read_file, native outbound attachment, matching bytes")


def main():
    run = uuid.uuid4().hex[:12]
    with Client() as client:
        marker = f"LAB_HELLO_{run}"
        sent = send(client, marker)
        done(client, marker, sent["created_at"])
        print("PASS connection: real Hermes replied through Tiny without a mention")
        for scenario in ("QUESTION", "MULTI", "CUSTOM", "OPEN", "APPROVAL"):
            interaction(client, scenario, run)
        media(client, run)
        marker = f"LAB_MENTION_{run}"
        sent = send(client, f"{marker} @{public('owner')}")
        mentioned = wait_event(client, {"kinds": [9, 40003], "authors": [public("agent")],
                                       "#h": ["lab"], "#p": [public("owner")], "since": sent["created_at"]},
                               lambda row: f"LAB_DONE {marker}" in row["content"])
        check(tag(mentioned, "mention") == public("owner"), "Explicit agent mention was not preserved")
        print("PASS mention: explicit model mention becomes a native recipient")
    print(json.dumps({"ok": True, "run": run, "connection": "tiny", "connector": "tinyagent"}))


if __name__ == "__main__":
    main()
