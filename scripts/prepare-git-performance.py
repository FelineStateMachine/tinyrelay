#!/usr/bin/env python3
"""Prepare immutable bare Git benchmark fixtures and inventory corpus metadata.

Usage: prepare-git-performance.py --source-directory ~/Developer --out /tmp/tinyrelay-corpus
The output directory must not already exist. Each selected repository is cloned
with --bare --no-local; the source working trees are never modified.
"""
from __future__ import annotations
import argparse, json, os, shutil, subprocess, tempfile
from pathlib import Path

NAMES = ("nzip", "bindws", "diagramzip", "strudel", "doorbearer", "atlas")

def run(*args: str, cwd: Path | None = None, input: str | None = None) -> str:
    p = subprocess.run(args, cwd=cwd, input=input, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if p.returncode:
        raise RuntimeError(f"{' '.join(args)}: {p.stderr.strip()}")
    return p.stdout.strip()

def inventory(repo: Path, name: str) -> dict:
    head_branch = run("git", "--git-dir", str(repo), "symbolic-ref", "HEAD")
    head = run("git", "--git-dir", str(repo), "rev-parse", head_branch)
    refs_lines = run("git", "--git-dir", str(repo), "for-each-ref", "--format=%(refname) %(objectname)").splitlines()
    tags = [line for line in refs_lines if line.startswith("refs/tags/")]
    head_commits = int(run("git", "--git-dir", str(repo), "rev-list", "--count", head_branch))
    all_commits = int(run("git", "--git-dir", str(repo), "rev-list", "--all", "--count"))
    stats = {}
    for line in run("git", "--git-dir", str(repo), "count-objects", "-v").splitlines():
        k, _, v = line.partition(" ")
        if k and v.isdigit(): stats[k] = int(v)
    object_ids = run("git", "--git-dir", str(repo), "rev-list", "--objects", "--all").splitlines()
    ids = [line.split()[0] for line in object_ids]
    objects = {}
    object_bytes = 0
    checked = run("git", "--git-dir", str(repo), "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
    sizes = {}
    for line in checked.splitlines():
        fields = line.split()
        if len(fields) != 3: continue
        oid, kind, size = fields[0], fields[1], int(fields[2])
        objects[kind] = objects.get(kind, 0) + 1
        object_bytes += size
        sizes[oid] = (kind, size)
    blob_sizes = []
    for line in object_ids:
        fields = line.split(maxsplit=1)
        if len(fields) == 2 and fields[0] in sizes and sizes[fields[0]][0] == "blob":
            blob_sizes.append((sizes[fields[0]][1], fields[1]))
    blob_sizes.sort(reverse=True)
    return {
        "name": name, "head": head, "head_branch": head_branch,
        "head_commits": head_commits, "all_commits": all_commits,
        "refs": len(refs_lines), "tags": len(tags),
        "objects": objects,
        "object_bytes": object_bytes,
        "pack_bytes": stats.get("size-pack", 0) * 1024,
        "largest_blobs": [{"bytes": size, "path": path} for size, path in blob_sizes[:10]],
        "blobs_over_1MiB": sum(size > 1024 * 1024 for size, _ in blob_sizes),
        "blobs_over_10MiB": sum(size > 10 * 1024 * 1024 for size, _ in blob_sizes),
    }

def prepare(source: Path, out: Path) -> list[dict]:
    if out.exists(): raise FileExistsError(f"refusing existing destination: {out}")
    out.mkdir(parents=True)
    rows = []
    try:
        for name in NAMES:
            src = source / name
            if not (src / ".git").exists() and not (src / "HEAD").exists(): raise FileNotFoundError(f"missing source repository: {src}")
            dest = out / (name + ".git")
            run("git", "clone", "--bare", "--no-local", str(src), str(dest))
            rows.append(inventory(dest, name))
        (out / "corpus.json").write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")
        return rows
    except Exception:
        shutil.rmtree(out, ignore_errors=True)
        raise

def self_test() -> None:
    with tempfile.TemporaryDirectory(prefix="prepare-git-performance-") as d:
        root, src, out = Path(d), Path(d) / "src", Path(d) / "out"
        src.mkdir(); run("git", "init", "-b", "trunk", str(src)); run("git", "-C", str(src), "config", "user.email", "test@example.invalid"); run("git", "-C", str(src), "config", "user.name", "test")
        (src / "tracked").write_text("tracked\n"); run("git", "-C", str(src), "add", "tracked"); run("git", "-C", str(src), "commit", "-m", "one"); run("git", "-C", str(src), "branch", "feature"); run("git", "-C", str(src), "tag", "v1")
        (src / "uncommitted").write_text("must not be copied\n")
        # Exercise prepare() itself without copying the real corpus: six source
        # names all point at the same tiny synthetic repository.
        for name in NAMES: os.symlink(src, root / name, target_is_directory=True)
        rows = prepare(root, out)
        assert len(rows) == len(NAMES)
        row = rows[0]
        assert row["head_branch"] == "refs/heads/trunk" and row["head"] == run("git", "-C", str(src), "rev-parse", "HEAD")
        assert row["tags"] == 1 and row["all_commits"] == 1
        assert "uncommitted" not in run("git", "--git-dir", str(out / "nzip.git"), "ls-tree", "-r", "--name-only", "HEAD")

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-directory", type=Path, default=Path.home() / "Developer")
    parser.add_argument("--out", type=Path)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test: self_test()
    else:
        if args.out is None: parser.error("--out is required unless --self-test is used")
        rows = prepare(args.source_directory.expanduser(), args.out.expanduser())
        print(json.dumps(rows, indent=2))
