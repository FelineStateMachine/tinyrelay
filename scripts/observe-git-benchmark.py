#!/usr/bin/env python3
"""Observe a Git benchmark and its Docker cgroups without changing its exit code."""

import argparse
import datetime as dt
import json
import os
import pathlib
import signal
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def write_json(path, value):
    path.write_text(json.dumps(value, sort_keys=True) + "\n", encoding="utf-8")


def http_capture(base, token, path, output):
    if not base:
        return {"path": path, "status": "disabled"}
    url = base.rstrip("/") + path
    request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(request, timeout=3) as response:
            data = response.read()
            output.write_bytes(data)
            return {"path": path, "status": response.status, "bytes": len(data)}
    except Exception as exc:  # diagnostics are supplementary to the benchmark
        return {"path": path, "status": "error", "error": str(exc)}


def container_pid(name):
    try:
        result = subprocess.run(
            ["docker", "inspect", "--format", "{{.State.Pid}}", name],
            check=True, capture_output=True, text=True,
        )
        pid = int(result.stdout.strip())
        return pid if pid > 0 else None
    except (OSError, ValueError, subprocess.CalledProcessError):
        return None


def cgroup_root(pid):
    try:
        for line in pathlib.Path(f"/proc/{pid}/cgroup").read_text().splitlines():
            hierarchy, _, path = line.partition("::")
            if hierarchy == "0":
                root = pathlib.Path("/sys/fs/cgroup") / path.lstrip("/")
                return root
    except OSError:
        pass
    return None


def read_number(path):
    try:
        return int(path.read_text().strip())
    except (OSError, ValueError):
        return None


def read_text(path):
    try:
        return path.read_text()
    except OSError:
        return None


def sample_container(name):
    pid = container_pid(name)
    sample = {"time": utc(), "container": name, "pid": pid}
    if not pid:
        sample["error"] = "container PID unavailable"
        return sample
    root = cgroup_root(pid)
    if root is None:
        sample["error"] = "cgroup v2 path unavailable"
        return sample
    sample["cgroup"] = str(root)
    sample["cpu.stat"] = read_text(root / "cpu.stat")
    sample["memory.current"] = read_number(root / "memory.current")
    sample["memory.peak"] = read_number(root / "memory.peak")
    sample["memory.stat"] = read_text(root / "memory.stat")
    sample["io.stat"] = read_text(root / "io.stat")
    sample["pids.current"] = read_number(root / "pids.current")
    sample["process.status"] = read_text(pathlib.Path(f"/proc/{pid}/status"))
    sample["memory.events"] = read_text(root / "memory.events")
    return sample


def host_sample():
    return {
        "time": utc(),
        "meminfo": read_text(pathlib.Path("/proc/meminfo")),
        "loadavg": read_text(pathlib.Path("/proc/loadavg")),
    }


def capture_cpu(base, token, output, result):
    if not base:
        return
    request = urllib.request.Request(
        base.rstrip("/") + "/debug/pprof/profile?seconds=60",
        headers={"Authorization": "Bearer " + token},
    )
    try:
        with urllib.request.urlopen(request, timeout=65) as response:
            output.write_bytes(response.read())
            result["status"] = response.status
    except Exception as exc:
        result["status"] = "error"
        result["error"] = str(exc)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, type=pathlib.Path)
    parser.add_argument("--container", action="append", default=[])
    parser.add_argument("--diagnostics", default="")
    parser.add_argument("--token", default="")
    parser.add_argument("--cpu-profile", action="store_true")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command and args.command[0] == "--" else args.command
    if not command:
        parser.error("a benchmark command is required")
    args.out.mkdir(parents=True, exist_ok=True)
    log_path = args.out / "harness.log"
    log = log_path.open("w", encoding="utf-8")

    def log_line(text):
        line = f"[{utc()}] {text}\n"
        log.write(line)
        log.flush()
        sys.stdout.write(line)
        sys.stdout.flush()

    write_json(args.out / "metadata.json", {
        "started": utc(), "command": command, "containers": args.container,
        "diagnostics": args.diagnostics or None, "cpu_profile": args.cpu_profile,
        "python": sys.version, "host": dict(zip(("sysname", "nodename", "release", "version", "machine"), os.uname())),
    })
    before = args.out / "diagnostics-before"
    after = args.out / "diagnostics-after"
    before.mkdir(exist_ok=True)
    after.mkdir(exist_ok=True)
    diag_paths = ["/metrics", "/debug/pprof/heap", "/debug/pprof/goroutine?debug=2"]
    before_results = [http_capture(args.diagnostics, args.token, path, before / path.rsplit("/", 1)[-1].split("?", 1)[0]) for path in diag_paths]
    write_json(args.out / "diagnostics-before.json", before_results)
    samples = (args.out / "samples.jsonl").open("w", encoding="utf-8")
    stop = threading.Event()

    def monitor():
        while not stop.is_set():
            record = {"time": utc(), "host": host_sample(), "containers": [sample_container(name) for name in args.container]}
            samples.write(json.dumps(record, sort_keys=True) + "\n")
            samples.flush()
            stop.wait(0.5)

    monitor_thread = threading.Thread(target=monitor, name="cgroup-monitor")
    monitor_thread.start()
    profile_result = {}
    profile_thread = None
    if args.cpu_profile and args.diagnostics:
        profile_thread = threading.Thread(target=capture_cpu, args=(args.diagnostics, args.token, args.out / "cpu.pprof", profile_result), name="cpu-profile")
        profile_thread.start()
        log_line("cpu profile started (bounded to 60 seconds)")
    log_line("phase benchmark-start")
    child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
    try:
        assert child.stdout is not None
        for line in child.stdout:
            log_line(line.rstrip("\n"))
        exit_code = child.wait()
    except BaseException:
        child.send_signal(signal.SIGTERM)
        child.wait(timeout=10)
        raise
    finally:
        stop.set()
        monitor_thread.join(timeout=3)
        samples.close()
    log_line(f"phase benchmark-end exit={exit_code}")
    after_results = [http_capture(args.diagnostics, args.token, path, after / path.rsplit("/", 1)[-1].split("?", 1)[0]) for path in diag_paths]
    write_json(args.out / "diagnostics-after.json", after_results)
    if profile_thread is not None:
        profile_thread.join(timeout=67)
        if profile_thread.is_alive():
            profile_result.update({"status": "timeout"})
        write_json(args.out / "cpu-profile.json", profile_result)
    write_json(args.out / "result.json", {"finished": utc(), "exit_code": exit_code, "profile": profile_result})
    log.close()
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
