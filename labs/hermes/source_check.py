"""Check that the lab never edits the upstream Hermes source files."""

import hashlib
import json
from pathlib import Path
import sys

ROOT = Path("/opt/hermes")
MANIFEST = Path("/opt/hermes-source.json")


def record():
    files = {}
    for path in sorted(ROOT.rglob("*")):
        if path.is_file() and not path.is_symlink():
            files[str(path.relative_to(ROOT))] = hashlib.sha256(path.read_bytes()).hexdigest()
    MANIFEST.write_text(json.dumps(files, sort_keys=True))


def verify():
    changed = []
    for name, expected in json.loads(MANIFEST.read_text()).items():
        path = ROOT / name
        if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            changed.append(name)
    if changed:
        raise SystemExit("Upstream Hermes files changed: " + ", ".join(changed[:20]))
    print("Unmodified Hermes source verified: " + Path("/opt/hermes-revision").read_text().strip())


if __name__ == "__main__":
    {"record": record, "verify": verify}[sys.argv[1]]()
