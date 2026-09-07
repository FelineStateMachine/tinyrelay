#!/usr/bin/env bash
set -euo pipefail

remote_host="${LINUX_HOST:?set LINUX_HOST to the SSH name of the Linux Docker host}"
name="${LINUX_NAME:-tinyrelay-bench-${USER:-user}-$$}"
safe_name="$(printf '%s' "$name" | tr -cs 'A-Za-z0-9_.-' '-' | sed 's/^-*//;s/-*$//')"
remote_root="tinyrelay-tests/${safe_name}"
image="tinyrelay-bench-${safe_name}"
packages="${BENCH_PACKAGES:-./internal/storage ./internal/relay}"
benchtime="${BENCHTIME:-5s}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
local_artifacts="${BENCH_ARTIFACTS:-artifacts/bench/${safe_name}/${stamp}}"
mkdir -p "$local_artifacts"
packages_b64="$(printf '%s' "$packages" | base64 | tr -d '\n')"

COPYFILE_DISABLE=1 tar --no-xattrs \
  --exclude=.git --exclude=.DS_Store --exclude=.playwright-cli \
  --exclude=output --exclude=artifacts --exclude=node_modules \
  -cf - . | ssh "$remote_host" "mkdir -p '$remote_root' && tar -xf - -C '$remote_root'"

set +e
ssh "$remote_host" "LINUX_PACKAGES_B64='$packages_b64' sh -s -- '$remote_root' '$image' '$stamp' '$benchtime'" <<'REMOTE'
set -eu
remote_root=$1
image=$2
stamp=$3
benchtime=$4
packages=$(printf '%s' "$LINUX_PACKAGES_B64" | base64 -d)
artifact_dir="$remote_root/artifacts/$stamp"
mkdir -p "$artifact_dir"
{
  date -u +%Y-%m-%dT%H:%M:%SZ
  printf 'host='; hostname
  printf 'kernel='; uname -a
  printf 'cpu='; nproc
  printf 'docker='; docker version --format '{{.Server.Version}}'
} > "$artifact_dir/metadata.txt"
set +e
docker build --target bench --build-arg BENCH_PACKAGES="$packages" --build-arg BENCHTIME="$benchtime" -t "$image" "$remote_root" > "$artifact_dir/bench.log" 2>&1
status=$?
cat "$artifact_dir/bench.log"
printf '%s\n' "$status" > "$artifact_dir/status"
exit "$status"
REMOTE
status=$?
set -e

scp -q "$remote_host:$remote_root/artifacts/$stamp/metadata.txt" "$local_artifacts/metadata.txt" || true
scp -q "$remote_host:$remote_root/artifacts/$stamp/status" "$local_artifacts/status" || true
scp -q "$remote_host:$remote_root/artifacts/$stamp/bench.log" "$local_artifacts/bench.log" || true
printf 'artifacts: %s\n' "$local_artifacts"
exit "$status"
