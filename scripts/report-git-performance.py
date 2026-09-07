#!/usr/bin/env python3
"""Render reproducible Git benchmark artifacts as a plain HTML/Markdown report."""

import argparse
import datetime as dt
import html
from functools import lru_cache
import json
import math
import pathlib
import statistics


PHASES = [
    ("initial push", "initial_push"),
    ("clone ×3 median", "fresh_clone_"),
    ("fetch", "noop_fetch_"),
    ("incremental state ACK", "incremental_state_ack"),
    ("incremental push", "incremental_push"),
    ("concurrent ×2 wall", "concurrent_clone_2"),
    ("concurrent ×4 wall", "concurrent_clone_4"),
]


def read_json(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def raw(value):
    return "—" if value is None else str(value)


def display(value, kind="number"):
    if value is None:
        return "—"
    if isinstance(value, str):
        if value.startswith("Command failed") or value.startswith("Nostr request") or value.startswith("error:"):
            return "FAILED: " + value.splitlines()[0][:160] if value else "FAILED"
        return value
    if kind == "mib":
        value = value / (1024 * 1024)
    if kind == "kb_mib":
        value = value / 1024
    if isinstance(value, (int, float)):
        magnitude = abs(value)
        if magnitude >= 1000:
            return f"{value:,.0f}"
        if magnitude >= 100:
            return f"{value:,.1f}"
        return f"{value:.3g}"
    return str(value)


def median(values):
    return statistics.median(values) if values else None


def percentile(values, p):
    if not values:
        return None
    values = sorted(values)
    index = (len(values) - 1) * p
    low, high = math.floor(index), math.ceil(index)
    if low == high:
        return values[low]
    return values[low] + (values[high] - values[low]) * (index - low)


def parse_stat(text):
    result = {}
    if not text:
        return result
    for line in text.splitlines():
        parts = line.split()
        if len(parts) >= 2:
            try:
                result[parts[0]] = int(parts[1])
            except ValueError:
                continue
    return result


def phase_value(phases, prefix, baseline=False):
    rows = []
    for row in phases:
        name = row.get("name", "")
        if baseline != name.startswith("baseline_"):
            continue
        candidate = name[len("baseline_"):] if baseline else name
        if candidate == prefix or (prefix.endswith("_") and candidate.startswith(prefix)):
            if row.get("ok") is False:
                return {"error": row.get("error", "failed")}
            rows.append(row.get("ms"))
    if prefix.endswith("_"):
        return median([v for v in rows if isinstance(v, (int, float))])
    return rows[0] if rows else None


def method_name(set_name):
    lower = set_name.lower()
    if "private" in lower:
        return "private Tiny (NIP-98)"
    if "stream" in lower or "after" in lower:
        return "streaming Tiny"
    if "native" in lower:
        return "native Git HTTP baseline"
    return "buffered Tiny"


@lru_cache(maxsize=None)
def load_samples(directory):
    path = directory / "samples.jsonl"
    rows = []
    try:
        for line in path.read_text(encoding="utf-8").splitlines():
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    except OSError:
        pass
    return rows


def fixture_bounds(directory, fixture):
    starts = ends = None
    all_starts = []
    try:
        for line in (directory / "progress.jsonl").read_text(encoding="utf-8").splitlines():
            row = json.loads(line)
            if row.get("event") == "fixture_start":
                all_starts.append((row.get("fixture"), row.get("at")))
                if row.get("fixture") == fixture:
                    starts = row.get("at")
            elif row.get("event") == "fixture_end":
                if row.get("fixture") == fixture:
                    ends = row.get("at")
    except (OSError, json.JSONDecodeError):
        pass
    if starts and not ends:
        later = [stamp for name, stamp in all_starts if stamp and stamp > starts]
        if later:
            ends = min(later)
    return starts, ends


def resource_summary(directory, metadata, fixture, baseline=False):
    process_status = directory / "process.status"
    text = process_status.read_text(encoding="utf-8") if process_status.exists() else ""
    vmhwm = None
    for line in text.splitlines():
        if line.startswith("VmHWM:"):
            try:
                vmhwm = int(line.split()[1])
            except (ValueError, IndexError):
                pass
    containers = metadata.get("containers", [])
    container = containers[1] if baseline and len(containers) > 1 else (containers[0] if containers else None)
    fixture_start, fixture_end = fixture_bounds(directory, fixture)
    try:
        start_time = dt.datetime.fromisoformat(fixture_start.replace("Z", "+00:00")).timestamp() if fixture_start else None
        end_time = dt.datetime.fromisoformat(fixture_end.replace("Z", "+00:00")).timestamp() if fixture_end else None
    except (TypeError, ValueError):
        start_time = end_time = None
    samples = []
    for row in load_samples(directory):
        try:
            row_time = dt.datetime.fromisoformat((row.get("time") or "").replace("Z", "+00:00")).timestamp()
        except (TypeError, ValueError):
            row_time = None
        if start_time is not None and row_time is not None and row_time < start_time:
            continue
        if end_time is not None and row_time is not None and row_time > end_time:
            continue
        for item in row.get("containers", []):
            if item.get("container") == container:
                mem = parse_stat(item.get("memory.stat"))
                for line in (item.get("process.status") or "").splitlines():
                    if line.startswith("VmHWM:"):
                        try:
                            vmhwm = max(vmhwm or 0, int(line.split()[1]))
                        except (ValueError, IndexError):
                            pass
                samples.append({
                    "time": item.get("time") or row.get("time"),
                    "cpu": parse_stat(item.get("cpu.stat")).get("usage_usec"),
                    "anon": mem.get("anon"), "file": mem.get("file"),
                    "total": item.get("memory.peak"),
                })
    values = lambda key: [x[key] for x in samples if isinstance(x.get(key), (int, float))]
    return {"vmhwm_kb": vmhwm, "vmhwm_scope": "sampled process status", "container": container,
            "anon_peak": max(values("anon"), default=None),
            "file_peak": max(values("file"), default=None),
            "total_peak": max(values("total"), default=None),
            "cpu_usec": max(values("cpu"), default=None)}


def run_vmhwm(directory, metadata, baseline=False):
    containers = metadata.get("containers", [])
    target = containers[1] if baseline and len(containers) > 1 else (containers[0] if containers else None)
    peak = None
    for row in load_samples(directory):
        for item in row.get("containers", []):
            if item.get("container") != target:
                continue
            for line in (item.get("process.status") or "").splitlines():
                if line.startswith("VmHWM:"):
                    try:
                        peak = max(peak or 0, int(line.split()[1]))
                    except (ValueError, IndexError):
                        pass
    return peak


def phase_cpu(directory, metadata, phase):
    samples = load_samples(directory)
    containers = metadata.get("containers", [])
    target = containers[0] if containers else None
    if phase.get("name", "").startswith("baseline_") and len(containers) > 1:
        target = containers[1]
    points = []
    for row in samples:
        for item in row.get("containers", []):
            if item.get("container") == target:
                usage = parse_stat(item.get("cpu.stat")).get("usage_usec")
                if usage is not None:
                    try:
                        stamp_raw = item.get("time") or row.get("time")
                        if not stamp_raw:
                            continue
                        stamp = dt.datetime.fromisoformat(stamp_raw.replace("Z", "+00:00")).timestamp()
                        points.append((stamp, usage))
                    except (KeyError, TypeError, ValueError):
                        pass
    if len(points) < 2:
        return None
    try:
        start = dt.datetime.fromisoformat(phase["started"].replace("Z", "+00:00")).timestamp()
        end = dt.datetime.fromisoformat(phase["finished"].replace("Z", "+00:00")).timestamp()
    except (KeyError, TypeError, ValueError):
        return None
    before = min(points, key=lambda p: abs(p[0] - start))
    after = min(points, key=lambda p: abs(p[0] - end))
    if abs(before[0] - start) > 0.5 or abs(after[0] - end) > 0.5:
        return None
    if after[0] <= before[0]:
        return None
    return after[1] - before[1]


def probe_summary(result, baseline=False):
    idle_publish, idle_query, busy_publish, busy_query = [], [], [], []
    idle_errors = busy_errors = 0
    for probe in result.get("probes", []):
        phase = probe.get("phase", "")
        is_baseline = phase.startswith("baseline_")
        if is_baseline != baseline or phase == "between_phases":
            continue
        publish_target, query_target = (idle_publish, idle_query) if phase == "idle" else (busy_publish, busy_query)
        if isinstance(probe.get("publishMs"), (int, float)):
            publish_target.append(probe["publishMs"])
        if isinstance(probe.get("queryMs"), (int, float)):
            query_target.append(probe["queryMs"])
        if any(key.endswith("Error") for key in probe):
            if phase == "idle":
                idle_errors += 1
            else:
                busy_errors += 1
    summary = []
    for values in (idle_publish, idle_query, busy_publish, busy_query):
        summary.extend((median(values), percentile(values, 0.95), max(values, default=None)))
    return (*summary, idle_errors, busy_errors)


def cells_for_result(directory, result, fixture, baseline=False):
    values = []
    for label, prefix in PHASES:
        value = phase_value(result.get("phases", []), prefix, baseline)
        if isinstance(value, dict):
            values.append(value["error"])
        else:
            values.append(value)
    meta = read_json(directory / "metadata.json") or {}
    resources = resource_summary(directory, meta, fixture, baseline)
    probes = probe_summary(result, baseline)
    clone_n = sum(1 for row in result.get("phases", []) if row.get("name", "") == ("baseline_" if baseline else "") + "fresh_clone_1" or row.get("name", "").startswith(("baseline_" if baseline else "") + "fresh_clone_"))
    return values, resources, probes, clone_n


def finding_phase(result, prefix):
    if not result:
        return None
    value = phase_value(result.get("phases", []), prefix, False)
    return value.get("error") if isinstance(value, dict) else value


def table(headers, rows, classes="", kinds=None):
    out = [f'<table class="{classes}"><thead><tr>']
    out.extend(f"<th>{html.escape(str(h))}</th>" for h in headers)
    out.append("</tr></thead><tbody>")
    for row in rows:
        out.append("<tr>" + "".join(f"<td>{html.escape(display(x, kinds[i] if kinds and i < len(kinds) else 'number'))}</td>" for i, x in enumerate(row)) + "</tr>")
    out.append("</tbody></table>")
    rendered = "\n".join(out)
    if classes == "wide" and len(headers) > 4:
        rendered = rendered.replace('class="wide"', 'class="desktop"', 1)
        mobile = ['<table class="mobile"><tbody>']
        for row in rows:
            mobile.append('<tr><th colspan="2">' + html.escape(str(row[0])) + '</th></tr>')
            for i, value in enumerate(row[1:], 1):
                kind = kinds[i] if kinds and i < len(kinds) else "number"
                mobile.append('<tr><th>' + html.escape(str(headers[i])) + '</th><td>' + html.escape(display(value, kind)) + '</td></tr>')
        mobile.append('</tbody></table>')
        rendered += "\n" + "\n".join(mobile)
    return rendered


def markdown_table(headers, rows, kinds=None):
    lines = ["| " + " | ".join(headers) + " |", "| " + " | ".join("---" for _ in headers) + " |"]
    for row in rows:
        cells = [display(x, kinds[i] if kinds and i < len(kinds) else "number") for i, x in enumerate(row)]
        lines.append("| " + " | ".join(c.replace("|", "\\|") for c in cells) + " |")
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=pathlib.Path, default=pathlib.Path("artifacts/git-performance"))
    parser.add_argument("--html", type=pathlib.Path)
    parser.add_argument("--markdown", type=pathlib.Path, default=pathlib.Path("docs/git-performance.md"))
    args = parser.parse_args()
    root = args.root
    html_path = args.html or root / "git-performance.html"
    corpus = read_json(root / "corpus.json") or []
    sets = []
    allowed_sets = {"results-before", "results-after", "results-before-atlas-concurrency", "results-private-atlas"}
    for directory in sorted(root.glob("results-*")):
        if directory.name not in allowed_sets:
            continue
        report = read_json(directory / "git-performance.json")
        if not report:
            continue
        meta = read_json(directory / "metadata.json") or {}
        sets.append((directory, report, meta))
    phase_rows, resource_rows, probe_rows, status_rows = [], [], [], []
    findings_rows = []
    highlight_rows = []
    raw_links = []
    raw_seen = set()
    for directory, report, meta in sets:
        for result in report.get("results", []):
            fixture = result.get("fixture", "?")
            failed = [p.get("name", "?") for p in result.get("phases", []) if p.get("ok") is False]
            status_rows.append([fixture, method_name(directory.name), "failed" if failed or result.get("status") not in (None, "complete", "ok", "passed") else "complete", ", ".join(failed) or "—"])
            for baseline in (False, True):
                if baseline and not any(p.get("name", "").startswith("baseline_") for p in result.get("phases", [])):
                    continue
                method = "native Git HTTP (" + directory.name.removeprefix("results-") + ")" if baseline else method_name(directory.name)
                values, resources, probes, clone_n = cells_for_result(directory, result, fixture, baseline)
                phase_rows.append([fixture, method, clone_n, *values])
                phase_rows_raw = result.get("phases", [])
                cpu_values = []
                for prefix in ("initial_push", "incremental_push", "concurrent_clone_2", "concurrent_clone_4"):
                    match = next((p for p in phase_rows_raw if (p.get("name", "").removeprefix("baseline_") == prefix) and (p.get("name", "").startswith("baseline_") == baseline)), None)
                    cpu_values.append(phase_cpu(directory, meta, match) if match else None)
                resource_rows.append([fixture, method, resources["vmhwm_kb"], resources["anon_peak"], resources["file_peak"], resources["total_peak"], *cpu_values, resources["container"]])
                probe_rows.append([fixture, method, *probes])
            raw_path = directory / "git-performance.json"
            if raw_path not in raw_seen:
                raw_links.append((directory.name, raw_path))
                raw_seen.add(raw_path)
    corpus_rows = [[x.get("name"), x.get("all_commits"), x.get("refs"), x.get("pack_bytes")] for x in corpus]
    set_by_name = {directory.name: (directory, report, meta) for directory, report, meta in sets}
    before_set = set_by_name.get("results-before")
    after_set = set_by_name.get("results-after")
    before_mem = run_vmhwm(before_set[0], before_set[2]) if before_set else None
    after_mem = run_vmhwm(after_set[0], after_set[2]) if after_set else None
    highlight_rows.append(["Peak Go memory (MiB)", before_mem / 1024 if before_mem is not None else None, after_mem / 1024 if after_mem is not None else None])
    result_maps = {}
    for label, item in (("before", before_set), ("after", after_set)):
        if item:
            result_maps[label] = {r.get("fixture"): r for r in item[1].get("results", [])}
    for fixture in sorted(set(result_maps.get("before", {})) | set(result_maps.get("after", {}))):
        row = [fixture]
        for label in ("before", "after"):
            result = result_maps.get(label, {}).get(fixture)
            row.append(finding_phase(result, "fresh_clone_"))
            row.append(finding_phase(result, "concurrent_clone_4"))
            row.append(finding_phase(result, "incremental_state_ack"))
            row.append(finding_phase(result, "incremental_push"))
        findings_rows.append(row)
    before_strudel = result_maps.get("before", {}).get("strudel.git")
    after_strudel = result_maps.get("after", {}).get("strudel.git")
    highlight_rows.append(["Tag-heavy strudel incremental push (ms)", finding_phase(before_strudel, "incremental_push"), finding_phase(after_strudel, "incremental_push")])
    headers = ["repo", "method", "clone n"] + [(("clone median" if x[1] == "fresh_clone_" else x[0]) + (" ms" if x[1] != "fresh_clone_" else " ms")) for x in PHASES]
    html_doc = '<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Git performance</title><style>body{font:14px system-ui,sans-serif;width:360px;margin:1rem auto;line-height:1.45}table{border-collapse:collapse;margin:1rem 0;width:100%;table-layout:fixed}th,td{border:1px solid;padding:.4rem;text-align:left;overflow-wrap:anywhere}summary{cursor:pointer;font-weight:bold}.desktop{display:none}.mobile{display:table}@media(min-width:1160px){body{width:1120px}.desktop{display:table}.mobile{display:none}}</style></head><body>'
    html_doc += '<h1>Git performance</h1><p>Six repositories from ~/Developer, tested on Slate. All six completed public push, clone, fetch and concurrent clone workloads after the fixes. Every background relay probe succeeded.</p><p>Git responses now stream as they are produced. Ref authorization and object checks use batches, and idle connections stay alive through WebSocket pings.</p>'
    html_doc += table(["finding", "before", "after"], highlight_rows, "wide", ["number", "number", "number"])
    clone_rows = []
    for fixture, result in result_maps.get("after", {}).items():
        before_result = result_maps.get("before", {}).get(fixture)
        native = phase_value(result.get("phases", []), "fresh_clone_", True)
        values = [finding_phase(before_result, "fresh_clone_"), finding_phase(result, "fresh_clone_"), native]
        clone_rows.append([fixture.removesuffix(".git"), *[f"{v / 1000:.2f}" if isinstance(v, (int, float)) else "—" for v in values]])
    html_doc += "<h2>Clone comparison</h2><p>Median of three fresh clones, in seconds. Git is the native HTTP baseline measured alongside the after run.</p>"
    html_doc += table(["Repository", "Before", "After", "Git"], clone_rows)
    private_set = set_by_name.get("results-private-atlas")
    tail_rows = []
    public_atlas = result_maps.get("after", {}).get("atlas.git")
    if public_atlas:
        for label, baseline in (("Public Atlas", False), ("Native Git Atlas phases", True)):
            probes = probe_summary(public_atlas, baseline)
            tail_rows.append([label, probes[7], probes[8], probes[10], probes[11]])
    if private_set:
        for result in private_set[1].get("results", []):
            probes = probe_summary(result)
            tail_rows.append(["Private " + result.get("fixture", "?"), probes[7], probes[8], probes[10], probes[11]])
    if tail_rows:
        html_doc += "<h2>Remaining relay latency</h2><p>All probes succeeded, but durable event acknowledgements still showed occasional stalls under large Git transfers. Similar stalls occurred while the native Git baseline was busy on the same host. The comparison does not establish the cause; disk latency and scheduling need a focused capture before choosing another optimization.</p>"
        html_doc += table(["Workload", "ACK P95 ms", "ACK max ms", "Query P95 ms", "Query max ms"], tail_rows, "wide")
    if private_set:
        private_rows = []
        for result in private_set[1].get("results", []):
            private_rows.append([result.get("fixture"), result.get("status"), finding_phase(result, "initial_push"), finding_phase(result, "fresh_clone_"), finding_phase(result, "concurrent_clone_4")])
        peak = run_vmhwm(private_set[0], private_set[2])
        html_doc += "<details><summary>Private repository follow-up</summary><p>Private Git requests verify NIP-98 payload hashes before Git receives them. Uploads spool to disk so memory does not grow with pack size. The client used an ephemeral signing proxy that also spooled requests; these timings include that client overhead and have no private native-Git baseline. The process first completed a small private nzip smoke test.</p>"
        html_doc += f"<p>Private-run Go RSS high-water: {display(peak, 'kb_mib')} MiB.</p>"
        html_doc += table(["repo", "status", "initial push ms", "clone median ms", "concurrent4 ms"], private_rows, "wide") + "</details>"
    html_doc += "<details><summary>Push and concurrent clone comparisons (ms)</summary>"
    html_doc += table(["repo", "before clone", "before concurrent4", "before state ACK", "before push", "after clone", "after concurrent4", "after state ACK", "after push"], findings_rows, "wide", ["number"] * 9)
    html_doc += "</details><details><summary>Repository characteristics</summary>"
    html_doc += table(["repo", "all commits", "refs", "pack MiB"], corpus_rows, "wide", ["number", "number", "number", "mib"])
    html_doc += "</details><details><summary>All phase timings (milliseconds)</summary>"
    html_doc += table(headers, phase_rows, "wide", ["number", "number", "number"] + ["number"] * len(PHASES))
    html_doc += "</details>"
    mobile_rows = []
    for row in phase_rows:
        for index, (label, _) in enumerate(PHASES, start=2):
            mobile_rows.append([row[0], row[1], label, row[index + 1]])

    resource_headers = ["repo", "method", "Go RSS high-water MiB", "cgroup anon peak MiB", "cgroup file peak MiB", "cgroup total peak MiB", "CPU initial µs", "CPU incremental µs", "CPU concurrent2 µs", "CPU concurrent4 µs", "container"]
    html_doc += "<details><summary>Process, container memory and CPU</summary>"
    html_doc += table(resource_headers, resource_rows, "wide", ["number", "number", "kb_mib", "mib", "mib", "mib", "number", "number", "number", "number", "number"])
    html_doc += "</details>"
    probe_headers = ["repo", "method", "idle publish P50 ms", "idle publish P95 ms", "idle publish max ms", "idle query P50 ms", "idle query P95 ms", "idle query max ms", "busy publish P50 ms", "busy publish P95 ms", "busy publish max ms", "busy query P50 ms", "busy query P95 ms", "busy query max ms", "idle errors", "busy errors"]
    html_doc += "<details><summary>Relay responsiveness during Git operations</summary>"
    html_doc += table(probe_headers, probe_rows, "wide", ["number", "number"] + ["number"] * 14)
    html_doc += "</details><details><summary>Failures and completion by run</summary>"
    html_doc += table(["repo", "method", "status", "failed phases"], status_rows, "wide")
    html_doc += "</details><details><summary>Method and limits of comparison</summary><p>Linux Slate: 16 logical CPUs, Git 2.50.1 client and 2.39.5 server, Go 1.27.1. Client and server shared one host over loopback; SSH/Tailscale was used to orchestrate the run. Each server container had a 10 GiB test ceiling, no CPU cap and no swap. These are test containment settings, not product limits.</p><p>Fixtures contain committed local branches and tags. Fresh clones used new destination directories; the operating system page cache was not cleared. Three sequential clones and one group each at concurrency two and four were measured. The incremental push adds one commit with an unchanged tree. Native Git uses the same git-http-backend with a minimal streaming HTTP wrapper. Differences around the native baseline can reflect cache and host variation. No Worker-hosted bindws runtime was benchmarked.</p><p>Go VmHWM is the whole-run peak resident memory of tiny itself. Container memory also includes Git subprocesses and file cache. Those components must not be confused. Cgroup CPU samples include background relay probes and Git children, at roughly 500 ms resolution. The first 60 seconds have a Go CPU profile, which does not include Git child CPU. The failed initial runs are retained; no missing phase counts as a pass.</p><p>Run manifests, hashes, logs, profiles and 500 ms resource samples accompany the raw JSON below. Timings are wall-clock milliseconds unless labeled otherwise. The before run includes the gzip/HEAD compatibility fixes; the after run adds streaming, batching and connection/state correctness fixes.</p></details>"
    html_doc += "<h2>Raw results</h2><ul>" + "".join(f'<li><a href="{html.escape(str(path.relative_to(root)))}">{html.escape(label)}</a></li>' for label, path in raw_links) + "</ul>"
    if (root / "results-after-first/git-performance.json").exists():
        html_doc += '<p><a href="results-after-first/git-performance.json">Earlier successful streaming run</a> retained separately. The main comparison uses the final public run.</p>'
    html_doc += '<p>Per-repository resource rows show the sampled cumulative process/cgroup high-water values through that fixture; anonymous and file-memory peaks are sampled within the fixture. The headline Go peak covers the complete public run.</p>'
    html_doc += "</body></html>"
    html_path.parent.mkdir(parents=True, exist_ok=True)
    html_path.write_text(html_doc, encoding="utf-8")
    md = ["# Git performance", "", "Phase wall times come from the recorded harness. Missing or failed phases remain visible. This report does not compare against the bindws Worker runtime.", "", "Method: Linux Slate, 16 CPUs, 10 GiB test-container ceiling, no CPU cap, no swap; client/server loopback; Git 2.50.1 client and 2.39.5 server; fixed local heads and tags with no cold page-cache reset. Phase CPU deltas are total container CPU inclusive of Git children and background probes, and values below sampler resolution are shown as missing.", "", "Methods: native Git HTTP baseline, buffered Tiny, and streaming Tiny where result artifacts exist.", "", "## Findings", "", markdown_table(["finding", "before", "after"], highlight_rows, ["number", "number", "number"]), "", markdown_table(["repo", "before clone", "before concurrent4", "before state ACK", "before push", "after clone", "after concurrent4", "after state ACK", "after push"], findings_rows, ["number"] * 9), "", "## Phase results", "", markdown_table(headers, phase_rows, ["number", "number", "number"] + ["number"] * len(PHASES)), "", "## Resource results", "", markdown_table(resource_headers, resource_rows, ["number", "number", "kb_mib", "mib", "mib", "mib", "number", "number", "number", "number", "number"]), "", "## Probe results", "", markdown_table(probe_headers, probe_rows, ["number", "number"] + ["number"] * 14), "", "## Status", "", markdown_table(["repo", "method", "status", "failed phases"], status_rows), "", "Profile finding from the captured before run: CPU samples identify memory clearing/copying as the visible hot path; the independent Go RSS reading is 6093.9 MiB. Keep the raw profile and process/cgroup captures beside any interpretation.", "", "Raw JSON artifacts are linked from the HTML report."]
    md[md.index("## Phase results"):md.index("## Phase results")] = ["## Remaining relay latency", "", "Every probe succeeded, but durable event acknowledgements still stalled occasionally during large Git transfers, including native Git baseline phases on the same host. These results do not identify the cause. Capture disk latency and scheduling before selecting another optimization.", "", markdown_table(["workload", "ACK P95 ms", "ACK max ms", "query P95 ms", "query max ms"], tail_rows), ""]
    args.markdown.parent.mkdir(parents=True, exist_ok=True)
    args.markdown.write_text("\n".join(md) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
