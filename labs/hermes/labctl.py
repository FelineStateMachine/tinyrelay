"""Send a lab message or answer a native request using the disposable operator."""

import json
import sys
from pathlib import Path

from rpc import Client, answer, public, tag


def main(args):
    if not args or args[0] == "help":
        print("send TEXT | requests | answer EVENT_ID OPTION_ID | events | identity | export-key")
        return
    if args[0] == "export-key":
        print(json.loads(Path("/operator/owner.json").read_text())["secret"])
        return
    with Client() as client:
        if args[0] == "send":
            result = client.publish(9, " ".join(args[1:]), [["h", "lab"], ["p", public("agent")]])
        elif args[0] == "requests":
            result = client.browse("browseapprovals", {"state": "open"})
        elif args[0] == "answer":
            rows = client.query({"ids": [args[1]], "kinds": [9], "limit": 1})
            if not rows or tag(rows[0], "tinyagent") != "1":
                raise ValueError("The lab request was not found")
            result = answer(client, rows[0], " ".join(args[2:]))
        elif args[0] == "events":
            result = client.query({"kinds": [9, 1111, 40003], "#h": ["lab"], "limit": 30})
        elif args[0] == "identity":
            result = client.call("identity")
        else:
            raise ValueError("Unknown lab command; run help")
        print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main(sys.argv[1:])
