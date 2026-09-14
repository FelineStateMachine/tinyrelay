"""Create identities and seed a disposable Tiny and Hermes workspace."""

import json
import os
from pathlib import Path
import subprocess
import sys
import time

from rpc import Client, public


def save(path, text, private=False):
    path.write_text(text)
    path.chmod(0o600 if private else 0o644)


def identity():
    return json.loads(subprocess.check_output(["tinyagent", "keygen"], text=True))


def initialize():
    os.umask(0o077)
    for directory in ("/public", "/operator", "/hermes-home", "/tiny-data"):
        Path(directory).mkdir(parents=True, exist_ok=True)
    for name in ("owner", "observer"):
        path = Path(f"/operator/{name}.json")
        if not path.exists():
            save(path, json.dumps(identity()), private=True)
        actor = json.loads(path.read_text())
        save(Path(f"/public/{name}.pub"), actor["pubkey"] + "\n")
    home = Path("/hermes-home")
    key_path = home / "tiny-identity.json"
    if not key_path.exists():
        save(key_path, json.dumps(identity()), private=True)
    agent = json.loads(key_path.read_text())
    save(Path("/public/agent.pub"), agent["pubkey"] + "\n")
    env = {
        "TINY_PRIVATE_KEY": agent["secret"], "TINY_RELAY_URL": "http://tiny:7447",
        "TINY_CHANNELS": "lab", "TINY_HOME_CHANNEL": "lab", "TINY_ALLOWED_USERS": public("owner"),
        "TINY_CLI_PATH": "/usr/local/bin/tinyagent",
    }
    save(home / ".env", "".join(f"{name}={value}\n" for name, value in env.items()), private=True)
    config = {
        "model": {"provider": "custom", "default": "tiny-lab", "base_url": "http://model:8080/v1",
                  "api_key": "lab-only", "api_mode": "chat_completions", "context_length": 131072},
        "agent": {"max_turns": 12},
        "display": {"busy_input_mode": "queue"},
        "model_catalog": {"enabled": False},
        "tools": {"tool_search": {"enabled": "off"}},
        "platform_toolsets": {"tiny": ["clarify", "code_execution", "terminal", "file"]},
        "platforms": {"tiny": {"enabled": True, "extra": {"rooms": "lab", "allowed_users": public("owner")}}},
        "plugins": {"enabled": ["tinyagent"]},
        "streaming": {"enabled": True, "edit_interval": 0.2, "buffer_threshold": 10},
        "auxiliary": {"title_generation": {"enabled": False}},
        "memory": {"memory_enabled": False, "user_profile_enabled": False},
        "compression": {"enabled": False},
        "terminal": {"backend": "local", "cwd": "/home/hermes/.hermes/workspace"},
    }
    # JSON is a YAML subset; no bootstrap dependency is needed.
    save(home / "config.yaml", json.dumps(config, indent=2) + "\n", private=True)
    (home / "workspace").mkdir(exist_ok=True)
    for path in [home, *home.rglob("*")]:
        if not path.is_symlink():
            os.chown(path, 10001, 10001)
    Path("/public").chmod(0o755)
    os.chown("/tiny-data", 10001, 10001)
    Path("/tiny-data").chmod(0o700)
    print("Disposable identities and Hermes configuration are ready.")


def seed():
    with Client() as client:
        client.browse("setpolicy", {"writes": "allowlist", "reads": "members",
                                     "directoryPublic": False, "features": {"files": True}})
        client.publish(0, json.dumps({"name": "Lab operator"}), [])
        if not client.query({"kinds": [9007], "#h": ["lab"], "limit": 1}):
            client.publish(9007, "A disposable workspace for the tinyagent Hermes connector.",
                           [["h", "lab"], ["name", "Hermes lab"], ["visibility", "open"]])
        for actor in ("agent", "observer"):
            key = public(actor)
            tags = [["d", key], ["p", key], ["name", f"Lab {actor}"], ["room", "lab"],
                    ["jobs", "both"], ["expiration", str(int(time.time()) + 86400)], ["rate", "600"]]
            tags += [["k", str(kind)] for kind in (0, 9, 11, 12, 7, 1111, 40003, 20001, 20002)]
            client.publish(30392, "", tags)
            client.publish(9000, "", [["h", "lab"], ["p", key, "member"]])
    print("Lab room and one-day grants are ready.")


if __name__ == "__main__":
    {"init": initialize, "seed": seed}[sys.argv[1]]()
