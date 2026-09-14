import json
import os
from pathlib import Path
import subprocess

owner = Path("/public/owner.pub").read_text().strip()
tenants = json.loads(subprocess.check_output(["tiny", "tenant", "list", "--data-dir", "/data"]))
if not any(item.get("Name", item.get("name")) == "lab" for item in tenants or []):
    subprocess.run(["tiny", "tenant", "create", "--data-dir", "/data", "--name", "lab",
                    "--owner", owner, "--template", "default"], check=True)
os.execvp("tiny", ["tiny", "serve", "--data-dir", "/data", "--listen", ":7447",
                   "--public-url", "http://tiny:7447", "--default-tenant", "lab"])
