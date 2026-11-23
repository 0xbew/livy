#!/usr/bin/env bash
# Deploy Livy's OTEL collector to Google Cloud Run. It publishes every signal (logs,
# metrics, traces) to a Pub/Sub topic, and a Cloud Storage subscription writes them to
# the bucket as Avro files under <prefix>/dt=YYYY-MM-DD/.
#
#   deploy/otel/gcp.sh [--dry-run] up      create or update everything (idempotent)
#   deploy/otel/gcp.sh [--dry-run] check   refused without the token; with it, one record stored
#   deploy/otel/gcp.sh [--dry-run] down    remove everything `up` created except the bucket
#
# Settings (environment):
#   LIVY_GCP_PROJECT   required: the project to deploy into
#   LIVY_GCP_REGION    default us-central1: Cloud Run region, and the bucket's location if created
#   LIVY_OTEL_BUCKET   required: the bucket Livy's own uploads go to (created if absent)
#   LIVY_OTEL_PREFIX   default source=otel: objects land under <prefix>/dt=YYYY-MM-DD/
#   LIVY_OTEL_NAME     default livy-otel (6-30 characters): the service, topic, subscription
#                      and secret, and the service account that runs the collector (it may
#                      only publish to the topic)
#   LIVY_OTEL_WRITER_NAME  default NAME-writer (6-30 characters): the service account
#                      Pub/Sub writes the bucket as
#
# Before `up`: the deploying account needs to enable services, read the project, create
# service accounts, secrets, topics and subscriptions, deploy Cloud Run, act as the
# collector's service account (iam.serviceAccounts.actAs), and set IAM on the bucket, the
# topic, the secret and the writer account. collector.yaml and pubsub.yaml (beside this
# script) are what the service runs; see README.md in this folder.
#
# The writer may create objects anywhere in the bucket and list their names, and read,
# overwrite or delete none: Pub/Sub refuses a subscription whose writer an IAM condition limits to a prefix,
# so the prefix is held by the subscription's own config. Only Pub/Sub's service agent
# can act as the writer.
#
# Every gcloud command is printed (prefixed "+") before it runs. --dry-run prints them
# and runs none, not even reads, so it needs no credentials: a dry `up` shows every
# create as if nothing existed, a dry `down` every delete as if everything did. A read
# that fails for any reason but "not found" stops the script. Needs gcloud, curl,
# openssl and python3.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
usage() { sed -n '2,/^set /{/^#/p;}' "$0" >&2; exit 2; }
DRY=0
ACTION=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    up|check|down) [ -z "$ACTION" ] || usage; ACTION="$arg" ;;
    *) usage ;;
  esac
done
[ -n "$ACTION" ] || usage

PROJECT="${LIVY_GCP_PROJECT:-}"
REGION="${LIVY_GCP_REGION:-us-central1}"
BUCKET="${LIVY_OTEL_BUCKET:-}"
PREFIX="${LIVY_OTEL_PREFIX:-source=otel}"
# Optional: the separate bucket prompt text is uploaded to (livy prompts), never the ledger's.
PROMPTS_BUCKET="${LIVY_PROMPTS_BUCKET:-}"
NAME="${LIVY_OTEL_NAME:-livy-otel}"
if [ -z "$PROJECT" ] || [ -z "$BUCKET" ]; then
  echo "gcp.sh: set LIVY_GCP_PROJECT and LIVY_OTEL_BUCKET" >&2
  exit 2
fi
if [ "${#NAME}" -lt 6 ] || [ "${#NAME}" -gt 30 ]; then
  echo "gcp.sh: LIVY_OTEL_NAME must be 6 to 30 characters (a service account id)" >&2
  exit 2
fi
PREFIX="${PREFIX%/}"
SA="${NAME}@${PROJECT}.iam.gserviceaccount.com"
SECRET="${NAME}-token"
IMAGE="$(grep -v '^#' "$HERE/IMAGE" 2>/dev/null | head -n 1 || true)"
if [ -z "$IMAGE" ]; then
  echo "gcp.sh: no image in $HERE/IMAGE" >&2
  exit 2
fi
GCLOUD="${GCLOUD:-gcloud}"
WRITER_ID="${LIVY_OTEL_WRITER_NAME:-${NAME}-writer}"
WRITER="${WRITER_ID}@${PROJECT}.iam.gserviceaccount.com"
# A writer id longer than a service account id allows cannot exist: down skips it, up
# asks for LIVY_OTEL_WRITER_NAME.
if [ "${#WRITER_ID}" -gt 30 ] && [ "$ACTION" = up ]; then
  echo "gcp.sh: $WRITER_ID is over 30 characters; set LIVY_OTEL_WRITER_NAME to a shorter service account id" >&2
  exit 2
fi
TOPIC="$NAME"
SUBSCRIPTION="${NAME}-to-bucket"
# The grants an earlier version of this recipe gave the collector itself (it wrote the
# bucket directly); up removes them, since the collector now only publishes.
OLD_CONDITION="expression=resource.name.startsWith(\"projects/_/buckets/${BUCKET}/objects/${PREFIX}/\"),title=${NAME}-create-under-prefix"
# ONE copy of the delivery config for create and update: an update replaces the whole
# Cloud Storage config, and a field left out reverts to its default (text output,
# which would corrupt the OTLP protobuf that Avro keeps byte-exact). Files close after
# 60 s (Pub/Sub's minimum) or 10 MB.
STORAGE_CONFIG=(
  --cloud-storage-bucket="$BUCKET"
  --cloud-storage-file-prefix="${PREFIX}/dt="
  --cloud-storage-file-datetime-format="YYYY-MM-DD/hh_mm_ssZ"
  --cloud-storage-file-suffix=".avro"
  --cloud-storage-output-format=avro --cloud-storage-write-metadata
  --cloud-storage-max-duration=60s --cloud-storage-max-bytes=10MB
  --cloud-storage-service-account-email="$WRITER"
)
# Never expire: Pub/Sub deletes a subscription idle for 31 days by default, and every
# export after that would reach no subscription. Retention bounds redelivery.
LIFETIME=(--expiration-period=never --message-retention-duration=7d)
NOT_FOUND='not.?found|NOT_FOUND|does not exist|Cannot find|404'

say() { printf '%s\n' "$*" >&2; }
show() { { printf '+'; printf ' %q' "$@"; printf '\n'; } >&2; }
run() { show "$@"; [ "$DRY" = 1 ] || "$@"; }
g() { run "$GCLOUD" --project="$PROJECT" --quiet "$@"; }
# A read that decides whether to create or delete something: 0 present, 1 not found;
# any other failure (permissions, an expired login) stops the script. A dry run shows
# it and answers "absent" for up and "present" for down, so every step is printed.
exists() {
  local err
  show "$@"
  if [ "$DRY" = 1 ]; then [ "$ACTION" = down ]; return; fi
  if err="$("$@" 2>&1 >/dev/null)"; then return 0; fi
  if printf '%s' "$err" | grep -Eq "$NOT_FOUND"; then return 1; fi
  say "gcp.sh: $err"
  exit 1
}
# A removal that may find nothing to remove; anything else stops the script.
remove() {
  local err
  show "$@"
  [ "$DRY" = 1 ] && return 0
  if err="$("$@" 2>&1 >/dev/null)"; then return 0; fi
  if printf '%s' "$err" | grep -Eq "$NOT_FOUND"; then return 0; fi
  say "gcp.sh: $err"
  exit 1
}
# A read whose answer later commands need; a dry run prints the placeholder instead.
value() {
  local placeholder="$1"; shift
  show "$@"
  if [ "$DRY" = 1 ]; then printf '%s' "$placeholder"; else "$@"; fi
}
# A grant to an identity created moments ago can fail until IAM has propagated it.
retry() {
  local try
  for try in 1 2 3 4 5 6; do
    if "$@"; then return 0; fi
    [ "$try" = 6 ] && return 1
    say "  failed (attempt $try); a new service account can take a minute to propagate, retrying in 10s"
    sleep 10
  done
}
new_token() { openssl rand -base64 32 | tr -d '\n'; }

up() {
  g services enable run.googleapis.com secretmanager.googleapis.com storage.googleapis.com iam.googleapis.com \
    pubsub.googleapis.com

  if ! exists "$GCLOUD" --project="$PROJECT" storage buckets describe "gs://$BUCKET"; then
    g storage buckets create "gs://$BUCKET" --location="$REGION" \
      --uniform-bucket-level-access --public-access-prevention
  fi

  # Prompt text is kept 180 days, then the bucket deletes it. The rule covers the key prefix
  # livy writes under, so prompts.sink.prefix must stay empty.
  if [ -n "$PROMPTS_BUCKET" ]; then
    if [ "$PROMPTS_BUCKET" = "$BUCKET" ]; then
      echo "gcp.sh: LIVY_PROMPTS_BUCKET must not be the ledger's bucket" >&2
      exit 2
    fi
    if ! exists "$GCLOUD" --project="$PROJECT" storage buckets describe "gs://$PROMPTS_BUCKET"; then
      g storage buckets create "gs://$PROMPTS_BUCKET" --location="$REGION" \
        --uniform-bucket-level-access --public-access-prevention
    fi
    LIFECYCLE="$(mktemp)"
    printf '%s\n' '{"rule":[{"action":{"type":"Delete"},"condition":{"age":180,"matchesPrefix":["source=prompt-text/"]}}]}' >"$LIFECYCLE"
    g storage buckets update "gs://$PROMPTS_BUCKET" --lifecycle-file="$LIFECYCLE"
    rm -f "$LIFECYCLE"
  fi

  # The collector's identity: publish to the one topic, read its token, nothing else.
  if ! exists "$GCLOUD" --project="$PROJECT" iam service-accounts describe "$SA"; then
    g iam service-accounts create "$NAME" --display-name="Livy OTEL collector"
  fi
  if ! exists "$GCLOUD" --project="$PROJECT" pubsub topics describe "$TOPIC"; then
    g pubsub topics create "$TOPIC"
  fi
  # Adding a binding that is already there changes nothing, so these re-run safely.
  retry g pubsub topics add-iam-policy-binding "$TOPIC" --member="serviceAccount:$SA" --role=roles/pubsub.publisher

  # The writer: Pub/Sub's service agent writes the bucket as this account.
  if ! exists "$GCLOUD" --project="$PROJECT" iam service-accounts describe "$WRITER"; then
    g iam service-accounts create "$WRITER_ID" --display-name="Livy OTEL bucket writer (Pub/Sub)"
  fi
  retry g storage buckets add-iam-policy-binding "gs://$BUCKET" --member="serviceAccount:$WRITER" \
    --role=roles/storage.objectCreator --condition=None
  # Pub/Sub checks that the writer can read the bucket's metadata before it writes.
  retry g storage buckets add-iam-policy-binding "gs://$BUCKET" --member="serviceAccount:$WRITER" \
    --role=roles/storage.legacyBucketReader --condition=None
  local number agent
  number="$(value '<project-number>' "$GCLOUD" projects describe "$PROJECT" --format='value(projectNumber)')"
  agent="service-${number}@gcp-sa-pubsub.iam.gserviceaccount.com"
  # Pub/Sub's service agent is created on first use; make sure it exists to be granted.
  g beta services identity create --service=pubsub.googleapis.com
  retry g iam service-accounts add-iam-policy-binding "$WRITER" --member="serviceAccount:$agent" \
    --role=roles/iam.serviceAccountTokenCreator

  if ! exists "$GCLOUD" --project="$PROJECT" pubsub subscriptions describe "$SUBSCRIPTION"; then
    retry g pubsub subscriptions create "$SUBSCRIPTION" --topic="$TOPIC" "${STORAGE_CONFIG[@]}" "${LIFETIME[@]}"
  else
    local attached
    attached="$(value "projects/$PROJECT/topics/$TOPIC" "$GCLOUD" --project="$PROJECT" pubsub subscriptions describe \
      "$SUBSCRIPTION" --format='value(topic)')"
    if [ "$attached" != "projects/$PROJECT/topics/$TOPIC" ]; then
      say "gcp.sh: subscription $SUBSCRIPTION reads $attached, not $TOPIC; Pub/Sub cannot change that. Delete it and run up again."
      exit 1
    fi
    g pubsub subscriptions update "$SUBSCRIPTION" "${STORAGE_CONFIG[@]}" "${LIFETIME[@]}"
  fi

  # The token goes in on stdin and is never printed or put on a command line.
  if ! exists "$GCLOUD" --project="$PROJECT" secrets describe "$SECRET"; then
    show "$GCLOUD" --project="$PROJECT" --quiet secrets create "$SECRET" --replication-policy=automatic --data-file=-
    say "  (stdin: a new random token, openssl rand -base64 32)"
    [ "$DRY" = 1 ] || new_token | "$GCLOUD" --project="$PROJECT" --quiet secrets create "$SECRET" \
      --replication-policy=automatic --data-file=-
  else
    # The service reads the "latest" version, which is the newest one whatever its
    # state; a secret whose newest version is missing, disabled or destroyed (an
    # interrupted first run, a hand edit) gets a new one.
    local state=""
    if exists "$GCLOUD" --project="$PROJECT" secrets versions describe latest --secret="$SECRET"; then
      state="$("$GCLOUD" --project="$PROJECT" secrets versions describe latest --secret="$SECRET" --format='value(state)')"
    fi
    if [ "$DRY" = 0 ] && [ "$state" != ENABLED ]; then
      show "$GCLOUD" --project="$PROJECT" --quiet secrets versions add "$SECRET" --data-file=-
      new_token | "$GCLOUD" --project="$PROJECT" --quiet secrets versions add "$SECRET" --data-file=-
    fi
  fi
  retry g secrets add-iam-policy-binding "$SECRET" --member="serviceAccount:$SA" --role=roles/secretmanager.secretAccessor

  # The configuration travels as environment variables, so the running service is
  # the upstream image plus these two reviewable files.
  local envfile
  envfile="$(mktemp)"
  trap "rm -f '$envfile'" EXIT
  python3 - "$HERE" "$PROJECT" "$TOPIC" > "$envfile" <<'PY'
import json, pathlib, sys
here, project, topic = sys.argv[1:]
env = {"LIVY_OTEL_BASE": pathlib.Path(here, "collector.yaml").read_text(),
       "LIVY_OTEL_OVERLAY": pathlib.Path(here, "pubsub.yaml").read_text(),
       "LIVY_OTEL_PROJECT": project, "LIVY_OTEL_TOPIC": topic}
for key, value in env.items():
    print(f"{key}: {json.dumps(value)}")
PY
  say "  (env file, written locally: LIVY_OTEL_BASE=collector.yaml, LIVY_OTEL_OVERLAY=pubsub.yaml, LIVY_OTEL_PROJECT=$PROJECT, LIVY_OTEL_TOPIC=$TOPIC)"
  # --no-invoker-iam-check: the collector itself requires the bearer token, so
  # Cloud Run does not also ask for a Google identity (laptops and Codex have none).
  g run deploy "$NAME" --region="$REGION" --image="$IMAGE" --service-account="$SA" \
    --args=--config=env:LIVY_OTEL_BASE,--config=env:LIVY_OTEL_OVERLAY \
    --env-vars-file="$envfile" --set-secrets="LIVY_OTEL_TOKEN=${SECRET}:latest" \
    --port=8080 --cpu=1 --memory=512Mi --min-instances=0 --max-instances=2 --concurrency=80 \
    --no-invoker-iam-check --ingress=all
  # Only now that the new revision publishes: an earlier version of this recipe let the
  # collector write the bucket itself; those grants go.
  remove "$GCLOUD" --project="$PROJECT" --quiet storage buckets remove-iam-policy-binding "gs://$BUCKET" \
    --member="serviceAccount:$SA" --role=roles/storage.objectCreator --condition="$OLD_CONDITION"
  remove "$GCLOUD" --project="$PROJECT" --quiet storage buckets remove-iam-policy-binding "gs://$BUCKET" \
    --member="serviceAccount:$SA" --role=roles/storage.bucketViewer

  say ""
  say "Endpoint:  $GCLOUD run services describe $NAME --project=$PROJECT --region=$REGION --format='value(status.url)'"
  say "Token:     $GCLOUD secrets versions access latest --secret=$SECRET --project=$PROJECT"
  say "Then:      LIVY_GCP_PROJECT=$PROJECT LIVY_OTEL_BUCKET=$BUCKET $0 check"
}

# POST one OTLP/JSON body; the token, if any, reaches curl on stdin, never in argv.
post() {  # $1 url, $2 body, $3 token or empty
  local headers=""
  [ -z "$3" ] || headers="$(printf 'header = "Authorization: Bearer %s"\n' "$3")"
  printf '%s\n' "$headers" | curl -sS -K - -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' -d "$2" "$1/v1/logs"
}

check() {
  local url token without with record
  # One record from service "livy-check": a 200 means Pub/Sub accepted it, which proves
  # the token and the publish grant; the subscription writes it to the bucket within
  # about a minute. It stays there; derivation can drop it.
  record="{\"resourceLogs\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"livy-check\"}}]},\"scopeLogs\":[{\"logRecords\":[{\"timeUnixNano\":\"$(date +%s)000000000\",\"attributes\":[{\"key\":\"event.name\",\"value\":{\"stringValue\":\"livy.check\"}}]}]}]}]}"
  show "$GCLOUD" --project="$PROJECT" run services describe "$NAME" --region="$REGION" --format='value(status.url)'
  show "$GCLOUD" --project="$PROJECT" secrets versions access latest --secret="$SECRET"
  show curl -sS -X POST -H 'Content-Type: application/json' -d '<one livy-check record>' '<url>/v1/logs'
  show curl -sS -K '<the token on stdin>' -X POST -H 'Content-Type: application/json' -d '<one livy-check record>' '<url>/v1/logs'
  show "$GCLOUD" --project="$PROJECT" storage ls "gs://$BUCKET/$PREFIX/dt=<today>/"
  say "  (listed before the record and again 75s after it)"
  [ "$DRY" = 1 ] && return 0
  url="$("$GCLOUD" --project="$PROJECT" run services describe "$NAME" --region="$REGION" --format='value(status.url)')"
  token="$("$GCLOUD" --project="$PROJECT" secrets versions access latest --secret="$SECRET")"
  local day before after
  day="$(date -u +%Y-%m-%d)"
  before="$({ "$GCLOUD" --project="$PROJECT" storage ls "gs://$BUCKET/$PREFIX/dt=$day/" 2>/dev/null || true; } | wc -l | tr -d ' ')"
  without="$(post "$url" "$record" "")"
  with="$(post "$url" "$record" "$token")"
  say "without token: HTTP $without (want 401); with token: HTTP $with (want 200: published)"
  [ "$without" = 401 ] && [ "$with" = 200 ] || return 1
  # The subscription closes a file after 60 s; a new object proves the writer's grants.
  say "waiting 75s for the subscription to write gs://$BUCKET/$PREFIX/dt=$day/ ..."
  sleep 75
  after="$({ "$GCLOUD" --project="$PROJECT" storage ls "gs://$BUCKET/$PREFIX/dt=$day/" 2>/dev/null || true; } | wc -l | tr -d ' ')"
  say "objects under dt=$day: $before before, $after after (want more after)"
  [ "$after" -gt "$before" ]
}

down() {
  # The bucket and everything in it stay: they hold Livy's own uploads too.
  if exists "$GCLOUD" --project="$PROJECT" run services describe "$NAME" --region="$REGION"; then
    g run services delete "$NAME" --region="$REGION"
  fi
  # The subscription goes before the topic, and both before the writer, so nothing is
  # left delivering as an account that no longer exists. Deleting it drops what it has
  # not yet written (at most its last minute), so stop the clients first.
  if exists "$GCLOUD" --project="$PROJECT" pubsub subscriptions describe "$SUBSCRIPTION"; then
    g pubsub subscriptions delete "$SUBSCRIPTION"
  fi
  if exists "$GCLOUD" --project="$PROJECT" pubsub topics describe "$TOPIC"; then
    g pubsub topics delete "$TOPIC"
  fi
  if exists "$GCLOUD" --project="$PROJECT" secrets describe "$SECRET"; then
    g secrets delete "$SECRET"
  fi
  # Grants go before the accounts: deleting one first leaves deleted:serviceAccount bindings.
  local writer_possible=1
  [ "${#WRITER_ID}" -le 30 ] || writer_possible=0
  if exists "$GCLOUD" --project="$PROJECT" storage buckets describe "gs://$BUCKET"; then
    if [ "$writer_possible" = 1 ]; then
      remove "$GCLOUD" --project="$PROJECT" --quiet storage buckets remove-iam-policy-binding "gs://$BUCKET" \
        --member="serviceAccount:$WRITER" --role=roles/storage.objectCreator --condition=None
      remove "$GCLOUD" --project="$PROJECT" --quiet storage buckets remove-iam-policy-binding "gs://$BUCKET" \
        --member="serviceAccount:$WRITER" --role=roles/storage.legacyBucketReader --condition=None
    fi
    remove "$GCLOUD" --project="$PROJECT" --quiet storage buckets remove-iam-policy-binding "gs://$BUCKET" \
      --member="serviceAccount:$SA" --role=roles/storage.objectCreator --condition="$OLD_CONDITION"
    remove "$GCLOUD" --project="$PROJECT" --quiet storage buckets remove-iam-policy-binding "gs://$BUCKET" \
      --member="serviceAccount:$SA" --role=roles/storage.bucketViewer
  fi
  if [ "$writer_possible" = 1 ] && exists "$GCLOUD" --project="$PROJECT" iam service-accounts describe "$WRITER"; then
    g iam service-accounts delete "$WRITER"
  fi
  if exists "$GCLOUD" --project="$PROJECT" iam service-accounts describe "$SA"; then
    g iam service-accounts delete "$SA"
  fi
  [ "$DRY" = 1 ] || say "Removed the service, topic, subscription, token, bucket grants and both service accounts. gs://$BUCKET is unchanged."
}

"$ACTION"
