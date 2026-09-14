"""Lab operator using the connector's public JSON-lines interface."""

import json
import os
from pathlib import Path
import selectors
import subprocess
import time


def public(name):
    return Path(f"/public/{name}.pub").read_text().strip()


def tag(event, name):
    return next((row[1] for row in event.get("tags", []) if len(row) > 1 and row[0] == name), None)


class Client:
    def __init__(self, actor="owner"):
        key = json.loads(Path(f"/operator/{actor}.json").read_text())["secret"]
        env = {**os.environ, "TINY_PRIVATE_KEY": key}
        self.process = subprocess.Popen(
            ["tinyagent", "rpc", "--relay", "http://tiny:7447"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, env=env,
        )
        self.sequence = 0
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.process.stdout, selectors.EVENT_READ)

    def call(self, method, params=None, timeout=30):
        self.sequence += 1
        self.process.stdin.write(json.dumps({"id": self.sequence, "method": method, "params": params or {}}) + "\n")
        self.process.stdin.flush()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not self.selector.select(max(0, deadline - time.monotonic())):
                break
            line = self.process.stdout.readline()
            if not line:
                raise RuntimeError("tinyagent exited before replying")
            message = json.loads(line)
            if message.get("id") != self.sequence:
                continue
            if "error" in message:
                raise RuntimeError(message["error"]["message"])
            return message.get("result")
        raise TimeoutError(f"tinyagent {method} timed out")

    def publish(self, kind, content, tags):
        return self.call("publish", {"event": {"kind": kind, "content": content, "tags": tags}})

    def query(self, filter):
        return self.call("query", {"filter": filter}) or []

    def browse(self, method, params):
        response = self.call("request", {"path": "/manage/rpc", "method": "POST", "body": {
            "jsonrpc": "2.0", "id": 1, "method": method, "params": [params],
        }})
        if response.get("error"):
            raise RuntimeError(str(response["error"]))
        return response.get("result")

    def close(self):
        self.selector.close()
        self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


def answer(client, request, content):
    return client.publish(1111, content, [
        ["h", tag(request, "h")], ["E", request["id"]], ["K", "9"], ["P", request["pubkey"]],
        ["e", request["id"]], ["k", "9"], ["p", request["pubkey"]],
    ])


def wait_event(client, filter, predicate=lambda _event: True, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        events = client.query(filter)
        match = next((event for event in events if predicate(event)), None)
        if match:
            return match
        time.sleep(0.3)
    raise TimeoutError(f"No matching event for {filter}")
