#!/usr/bin/env bash
set -euo pipefail

remote_host="${SLATE_HOST:-slate}"
name="${SLATE_NAME:-tinyrelay-${USER:-user}-$$}"
safe_name="$(printf '%s' "$name" | tr -cs 'A-Za-z0-9_.-' '-' | sed 's/^-*//;s/-*$//')"
if [[ -z "$safe_name" ]]; then
  echo "invalid empty SLATE_NAME" >&2
  exit 2
fi
remote_root="/home/tank/tinyrelay-tests/${safe_name}"
image="tinyrelay-slate-${safe_name}"
packages="${SLATE_PACKAGES:-./internal/auth ./internal/blob ./internal/catalog ./internal/policy ./internal/relay ./internal/replication ./internal/storage ./internal/syncprotocol ./internal/telemetry ./internal/templates ./internal/work}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
local_artifacts="${SLATE_ARTIFACTS:-artifacts/slate/${safe_name}/${stamp}}"
mkdir -p "$local_artifacts"
packages_b64="$(printf '%s' "$packages" | base64 | tr -d '\n')"

echo "syncing worktree to ${remote_host}:${remote_root}"
COPYFILE_DISABLE=1 tar \
  --exclude=.git \
  --exclude=.DS_Store \
  --exclude=.playwright-cli \
  --exclude=output \
  --exclude=artifacts \
  --exclude=node_modules \
  --no-xattrs \
  -cf - . | ssh "$remote_host" "mkdir -p '$remote_root' && tar -xf - -C '$remote_root'"

echo "running Linux race tests in ${image}"
set +e
ssh "$remote_host" "SLATE_PACKAGES_B64='$packages_b64' sh -s -- '$remote_root' '$image' '$stamp'" <<'REMOTE'
set -eu
remote_root=$1
image=$2
stamp=$3
packages=$(printf '%s' "$SLATE_PACKAGES_B64" | base64 -d)
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
docker build --target test --build-arg TEST_PACKAGES="$packages" -t "$image" "$remote_root" > "$artifact_dir/docker-build.log" 2>&1
build_status=$?
cat "$artifact_dir/docker-build.log"
printf '%s\n' "$build_status" > "$artifact_dir/status"
if [ "$build_status" -eq 0 ]; then
  docker run --rm "$image" sh -c 'export PATH=/usr/local/go/bin:/usr/local/bin:$PATH; /usr/local/go/bin/go version; /usr/local/go/bin/go env GOOS GOARCH GOVERSION; sed -n "s/^.*modernc.org\/sqlite v//p" go.mod | head -1' > "$artifact_dir/runtime.txt"
fi
exit "$build_status"
REMOTE
remote_status=$?
set -e

scp -q "$remote_host:$remote_root/artifacts/$stamp/metadata.txt" "$local_artifacts/metadata.txt" || true
scp -q "$remote_host:$remote_root/artifacts/$stamp/runtime.txt" "$local_artifacts/runtime.txt" || true
scp -q "$remote_host:$remote_root/artifacts/$stamp/status" "$local_artifacts/status" || true
scp -q "$remote_host:$remote_root/artifacts/$stamp/docker-build.log" "$local_artifacts/docker-build.log" || true
printf 'artifacts: %s\n' "$local_artifacts"
exit "$remote_status"
