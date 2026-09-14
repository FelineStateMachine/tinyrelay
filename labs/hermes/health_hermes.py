"""Check Hermes' own runtime record without changing its source."""

import json
import os
from pathlib import Path

status = json.loads((Path(os.environ["HERMES_HOME"]) / "gateway_state.json").read_text())
os.kill(status["pid"], 0)
if status.get("platforms", {}).get("tiny", {}).get("state") != "connected":
    raise SystemExit("Tiny is not connected")
