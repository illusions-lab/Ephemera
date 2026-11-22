#!/usr/bin/env bash
set -euo pipefail

candidate=$1
: "${EPHE_IMAGE_TEST_CONFIG:?dedicated host must provide a test config}"
: "${EPHE_IMAGE_TEST_KEY:?dedicated host must provide a test key}"
: "${EPHE_IMAGE_TEST_CACHE:?dedicated host must provide an image cache}"
: "${EPHE_IMAGE_TEST_SCALE_SET:?dedicated host must provide a Scale Set label}"
: "${IMAGE_MIN_VERSION:?set the image channel minimum daemon version}"
work=$(mktemp -d /tmp/ephe-img.XXXXXX)
runs=()
pid=
cleanup() {
  for run in ${runs[@]+"${runs[@]}"}; do
    gh run cancel "$run" >/dev/null 2>&1 || true
  done
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" || true
  fi
  cat "$work/daemon.log" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -m 700 "$work/state" "$work/bin" "$work/faults"
case "$GUEST_OS/$GUEST_ARCH" in
  linux/amd64) vm_tool=qemu-system-x86_64 ;;
  linux/arm64) vm_tool=qemu-system-aarch64 ;;
  macos/arm64)
    vm_tool=tart
    # The dedicated test account must select the PATH wrapper.
    for installed in "$HOME/Library/Application Support/Ephemera/tools/tart.app" \
                     "$HOME/Library/Application Support/Ephemera/tools/tart"; do
      [[ ! -e "$installed" ]] || { echo "Remove installed Tart bindings from the test account; use PATH" >&2; exit 1; }
    done
    ;;
  *) echo "Unsupported test platform" >&2; exit 1 ;;
esac
export EPHE_TEST_REAL_VM_TOOL
EPHE_TEST_REAL_VM_TOOL=$(command -v "$vm_tool")
export EPHE_TEST_FAULTS="$work/faults"
cat > "$work/bin/$vm_tool" <<'WRAPPER'
#!/bin/bash
set -eu
if [[ -f "$EPHE_TEST_FAULTS/armed" ]]; then
  case " $* " in
    " run "*ephemera-*|*" -name ephemera-"*)
      for attempt in 1 2 3; do
        if mkdir "$EPHE_TEST_FAULTS/$attempt" 2>/dev/null; then
          exit 73
        fi
      done
      ;;
  esac
fi
exec "$EPHE_TEST_REAL_VM_TOOL" "$@"
WRAPPER
chmod 700 "$work/bin/$vm_tool"
export PATH="$work/bin:$PATH"
go build -o "$work/ephemerad" -ldflags "-X github.com/illusions-lab/Ephemera/internal/version.Version=$IMAGE_MIN_VERSION" ./cmd/ephemerad
go build -o "$work/ephemera" ./cmd/ephemera
printf '{"schema":1,"images":{}}\n' > "$work/channel.json"
"$work/ephemerad" --create --config "$EPHE_IMAGE_TEST_CONFIG" --github-app-key "$EPHE_IMAGE_TEST_KEY" \
  --image-cache "$EPHE_IMAGE_TEST_CACHE" --state-dir "$work/state" --status-socket "$work/state/status.sock" \
  --image-channel-file "$work/channel.json" >"$work/daemon.log" 2>&1 &
pid=$!
status_json() { "$work/ephemera" status --json --socket "$work/state/status.sock" 2>/dev/null | jq -e .daemon; }
wait_for() {
  local seconds=$1
  shift
  local deadline=$((SECONDS + seconds))
  until "$@"; do
    kill -0 "$pid"
    (( SECONDS < deadline )) || return 1
    sleep 2
  done
}
ready() { status_json | jq -e '.phase == "Healthy" and .image_update.enabled and .max_concurrent >= 2' >/dev/null; }
image_active() { status_json | jq -e --arg image "$1" '.image_update.current == $image' >/dev/null; }
job_running() { status_json | jq -e '.instances.running >= 1' >/dev/null; }
all_clean() { status_json | jq -e '.instances == {starting:0,waiting:0,running:0,reclaiming:0}' >/dev/null; }

dispatch() {
  local run_key=$1 hold=$2
  gh workflow run execution-acceptance.yml --ref "$GITHUB_REF_NAME" \
    -f correlation="$run_key" -f scale_set="$EPHE_IMAGE_TEST_SCALE_SET" \
    -f guest_os="$GUEST_OS" -f arch="$GUEST_ARCH" -f hold_seconds="$hold"
  local deadline=$((SECONDS + 90)) run=
  until [[ -n "$run" ]]; do
    run=$(gh run list --workflow execution-acceptance.yml --event workflow_dispatch \
      --json displayTitle,databaseId --jq ".[] | select(.displayTitle == \"Dedicated Scale Set acceptance $run_key\") | .databaseId")
    (( SECONDS < deadline )) || return 1
    [[ -n "$run" ]] || sleep 2
  done
  runs+=("$run")
}

wait_for 2400 ready
platform="$GUEST_OS/$GUEST_ARCH"
[[ "$GUEST_OS" != macos ]] || platform="darwin/$GUEST_ARCH"
revision=$(jq -er --arg platform "$platform" '.images[$platform].revision' "$candidate")

# Prepare the candidate while the baseline daemon stays available. Run the baseline
# job before publishing the candidate to the daemon so it is pinned to the old image.
dispatch "$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$GUEST_OS-$GUEST_ARCH-base" 3600
wait_for 300 job_running
cp "$candidate" "$work/channel.next"
mv "$work/channel.next" "$work/channel.json"
wait_for 3300 image_active "$revision"
job_running
dispatch "$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$GUEST_OS-$GUEST_ARCH-candidate" 5
gh run watch "${runs[1]}" --exit-status --interval 5
job_running
gh run watch "${runs[0]}" --exit-status --interval 5
wait_for 120 all_clean
grep -F "image_revision=$revision " "$work/daemon.log"


# Inject three failed VM starts after B has run successfully. The same queued
# job must eventually run on A after the daemon rolls back.
touch "$work/faults/armed"
dispatch "$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$GUEST_OS-$GUEST_ARCH-rollback" 5
gh run watch "${runs[2]}" --exit-status --interval 5
wait_for 120 all_clean
status_json | jq -e '.image_update.current == "bundled" and .image_update.rolled_back' >/dev/null
test -d "$work/faults/3"
grep -F 'image_revision="" ' "$work/daemon.log" | tail -n 1 | grep -F 'Job started'
