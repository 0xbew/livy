#!/usr/bin/env bash
# Path A: the dispatch program for `amp orb provider`, on Docker. It is Amp's documented Docker
# example (https://ampcode.com/docs/orbs/self-hosted, "Dispatch Contract") plus Livy: on pause
# and delete it ships the orb's Livy record to a local sink inside the orb, copies it out to
# LIVY_ORB_COLLECT on this host, and only then stops or removes the orb. The orb never holds
# a credential; this host's own sink ships the collected folder onward.
#
# The job arrives as environment variables (documented): ORB_OPERATION (spawn|pause|delete),
# ORB_ID, and on spawn ORB_IMAGE, ORB_TOKEN, optional AMP_URL, ORB_TYPE and ORB_SNAPSHOT_ID.
# Ours: LIVY_ORB_COLLECT (absolute folder), LIVY_ORB_PLATFORM (default linux/amd64).
set -euo pipefail

: "${ORB_OPERATION:?}" "${ORB_ID:?}"
: "${LIVY_ORB_COLLECT:?set LIVY_ORB_COLLECT to the folder that collects orb records}"
platform="${LIVY_ORB_PLATFORM:-linux/amd64}"
# The local sink's folder inside the orb, as profile.json names it.
outbox=/home/user/.livy-outbox

running() { [ "$(docker inspect -f '{{.State.Running}}' "$ORB_ID" 2>/dev/null)" = true ]; }
exists() { docker inspect "$ORB_ID" >/dev/null 2>&1; }

# collect ships what the orb recorded into its outbox, copies the outbox here write-once (the
# names are hashes of the bytes, so a repeat is a no-op), then deletes inside exactly the files
# that arrived. A failure is said on stderr, which the provider logs, and never blocks the job.
collect() {
  local ok=0 tmp
  if running && ! docker exec -u user "$ORB_ID" livy emit >&2; then
    echo "livy: emit failed in $ORB_ID" >&2
    ok=1
  fi
  # Errexit is off in here (collect runs in a condition), so every step that can lose data is
  # checked by hand.
  tmp="$(mktemp -d)" || { echo "livy: no temporary folder to copy $ORB_ID's outbox into" >&2; return 1; }
  # The image makes the outbox, so a copy that fails is a real failure, never "nothing yet".
  if docker cp "$ORB_ID:$outbox/." "$tmp/" >/dev/null; then
    collect_from "$tmp" || ok=1
  else
    echo "livy: could not copy the outbox out of $ORB_ID" >&2
    ok=1
  fi
  rm -rf "$tmp"
  return "$ok"
}

# collect_from copies each object in tmp to LIVY_ORB_COLLECT, then removes those from the orb.
# It fails when any object stayed behind.
collect_from() {
  local tmp="$1" rel dest left=0
  local copied=()
  while IFS= read -r -d '' rel; do
    rel="${rel#./}"
    dest="$LIVY_ORB_COLLECT/$rel"
    if [ -e "$dest" ]; then
      cmp -s "$tmp/$rel" "$dest" || { echo "livy: $dest exists with other bytes; left in the orb" >&2; left=1; continue; }
    elif ! { mkdir -p "$(dirname "$dest")" && cp "$tmp/$rel" "$dest.part" && mv "$dest.part" "$dest"; }; then
      rm -f "$dest.part"
      echo "livy: could not write $dest; left in the orb" >&2
      left=1
      continue
    fi
    copied+=("$rel")
  done < <(cd "$tmp" && find . -type f -name '*.jsonl.gz' -print0)
  echo "livy: collected ${#copied[@]} object(s) from $ORB_ID" >&2
  if [ "${#copied[@]}" -gt 0 ] && running; then
    printf '%s\0' "${copied[@]}" | docker exec -i -u user -w "$outbox" "$ORB_ID" xargs -0 rm -f --
  fi
  return "$left"
}

case "$ORB_OPERATION" in
spawn)
  image="$ORB_IMAGE"
  docker image inspect "${ORB_SNAPSHOT_ID-}" >/dev/null 2>&1 && image="$ORB_SNAPSHOT_ID"
  docker start "$ORB_ID" 2>/dev/null ||
    docker run --init --platform "$platform" -d --name "$ORB_ID" \
      --cpus 4 --memory 8g --shm-size=1g "$image" sleep infinity
  docker exec -d --env ORB_TOKEN --env AMP_URL "$ORB_ID" amp orb bootstrap
  ;;
pause)
  if exists; then
    collect || echo "livy: $ORB_ID paused with part of its record still inside; the next pause or delete retries" >&2
    docker stop "$ORB_ID"
    [ -z "${ORB_SNAPSHOT_ID-}" ] || docker commit "$ORB_ID" "$ORB_SNAPSHOT_ID"
  fi
  ;;
delete)
  if exists; then
    # A paused orb is started (without Amp) only to ship its last lines before it goes.
    running || docker start "$ORB_ID" >/dev/null
    if ! collect; then
      # Removing it would destroy what did not come out: keep it stopped, say so, fail the job.
      docker stop "$ORB_ID" >/dev/null
      echo "livy: $ORB_ID kept (stopped), not deleted: part of its record did not come out; collect it, then docker rm it" >&2
      exit 1
    fi
    docker rm -f "$ORB_ID"
  fi
  ;;
*)
  exit 1
  ;;
esac
