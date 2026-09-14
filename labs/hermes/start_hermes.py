import os
from pathlib import Path
import subprocess

subprocess.run(["python", "/lab/source_check.py", "verify"], check=True)
home = Path(os.environ["HERMES_HOME"])
plugins = home / "plugins"
plugins.mkdir(parents=True, exist_ok=True)
legacy = plugins / "tiny"
if legacy.is_symlink() and str(legacy.readlink()) in ("/opt/tiny-plugin", "/opt/tinyagent"):
    legacy.unlink()
plugin = plugins / "tinyagent"
if plugin.is_symlink() and str(plugin.readlink()) != "/opt/tinyagent":
    plugin.unlink()
if not plugin.exists():
    plugin.symlink_to("/opt/tinyagent", target_is_directory=True)
os.chdir("/opt/hermes")
os.execvp("python", ["python", "-m", "hermes_cli.main", "gateway", "run"])
