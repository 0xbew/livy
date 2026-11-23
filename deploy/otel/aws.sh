#!/usr/bin/env bash
# Deploy Livy's OTEL collector to AWS (Amazon ECS Express Mode on Fargate), writing to S3.
#
#   deploy/otel/aws.sh [--dry-run] up      create or update everything (idempotent)
#   deploy/otel/aws.sh [--dry-run] check   refused without the token; with it, one record stored
#   deploy/otel/aws.sh [--dry-run] down    remove everything `up` created except the bucket
#
# Settings (environment):
#   LIVY_AWS_REGION    default us-east-1
#   LIVY_OTEL_BUCKET   required: the bucket Livy's own uploads go to (created if absent)
#   LIVY_OTEL_PREFIX   default source=otel: objects land under <prefix>/dt=YYYY-MM-DD/
#   LIVY_OTEL_NAME     default livy-otel: the cluster, service, roles, secret and log group
#
# Before `up`: the deploying account needs IAM (create roles, put and attach policies,
# iam:PassRole on the three roles), ECS, Elastic Load Balancing (describe and modify
# target groups), Secrets Manager, CloudWatch Logs and S3 rights, and the region needs a
# default VPC with public subnets (where ECS Express Mode puts the load balancer).
# collector.yaml and s3.yaml (beside this script) are what the task runs; see README.md
# in this folder.
#
# Every aws command is printed (prefixed "+") before it runs. --dry-run prints them and
# runs none, not even reads, so it needs no credentials: a dry `up` shows every create
# as if nothing existed, a dry `down` every delete as if everything did. A read that
# fails for any reason but "not found" stops the script. Needs the AWS CLI v2 (with ECS
# Express Mode), curl, openssl and python3.
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

REGION="${LIVY_AWS_REGION:-us-east-1}"
BUCKET="${LIVY_OTEL_BUCKET:-}"
PREFIX="${LIVY_OTEL_PREFIX:-source=otel}"
# Optional: the separate bucket prompt text is uploaded to (livy prompts), never the ledger's.
PROMPTS_BUCKET="${LIVY_PROMPTS_BUCKET:-}"
NAME="${LIVY_OTEL_NAME:-livy-otel}"
if [ -z "$BUCKET" ]; then
  echo "aws.sh: set LIVY_OTEL_BUCKET" >&2
  exit 2
fi
PREFIX="${PREFIX%/}"
SECRET="${NAME}-token"
LOGS="/livy/${NAME}"
IMAGE="$(grep -v '^#' "$HERE/IMAGE" 2>/dev/null | head -n 1 || true)"
if [ -z "$IMAGE" ]; then
  echo "aws.sh: no image in $HERE/IMAGE" >&2
  exit 2
fi
AWS="${AWS:-aws}"
TASK_ROLE="${NAME}-task"
EXEC_ROLE="${NAME}-execution"
INFRA_ROLE="${NAME}-infrastructure"
EXEC_POLICY=arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy
INFRA_POLICY=arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRoleforExpressGatewayServices
NOT_FOUND='NotFound|NoSuchEntity|not found|does not exist|\(404\)'
WAIT_SECONDS="${LIVY_AWS_WAIT_SECONDS:-600}"
PENDING="${XDG_STATE_HOME:-$HOME/.local/state}/livy/otel-${NAME}-${REGION}-target-groups"

say() { printf '%s\n' "$*" >&2; }
show() { { printf '+'; printf ' %q' "$@"; printf '\n'; } >&2; }
run() { show "$@"; [ "$DRY" = 1 ] || "$@"; }
a() { run "$AWS" --region="$REGION" "$@"; }
# A read that decides whether to create or delete something: 0 present, 1 not found;
# any other failure (permissions, expired credentials, an old CLI) stops the script. A
# dry run shows it and answers "absent" for up and "present" for down.
exists() {
  local err
  show "$@"
  if [ "$DRY" = 1 ]; then [ "$ACTION" = down ]; return; fi
  if err="$("$@" 2>&1 >/dev/null)"; then return 0; fi
  if printf '%s' "$err" | grep -Eq "$NOT_FOUND"; then return 1; fi
  say "aws.sh: $err"
  exit 1
}
# A removal that may find nothing to remove; anything else stops the script.
remove() {
  local err
  show "$@"
  [ "$DRY" = 1 ] && return 0
  if err="$("$@" 2>&1 >/dev/null)"; then return 0; fi
  if printf '%s' "$err" | grep -Eq "$NOT_FOUND"; then return 0; fi
  say "aws.sh: $err"
  exit 1
}
# A read whose answer later commands need; a dry run prints the placeholder instead.
value() {
  local placeholder="$1"; shift
  show "$@"
  if [ "$DRY" = 1 ]; then printf '%s' "$placeholder"; else "$@"; fi
}

ACCOUNT="$(value '<account-id>' "$AWS" sts get-caller-identity --query Account --output text)"
SERVICE_ARN="arn:aws:ecs:${REGION}:${ACCOUNT}:service/${NAME}/${NAME}"

# ACTIVE, DRAINING, INACTIVE (deleted, still listed for a while) or ABSENT.
service_status() {
  local out err
  show "$AWS" --region="$REGION" ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
    --query service.status.statusCode --output text
  if [ "$DRY" = 1 ]; then
    if [ "$ACTION" = down ]; then echo ACTIVE; else echo ABSENT; fi
    return
  fi
  local errfile
  errfile="$(mktemp)"
  if out="$("$AWS" --region="$REGION" ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
      --query service.status.statusCode --output text 2>"$errfile")"; then
    rm -f "$errfile"
    echo "$out"
    return
  fi
  err="$(cat "$errfile")"; rm -f "$errfile"
  if printf '%s' "$err" | grep -Eq "$NOT_FOUND"; then echo ABSENT; return; fi
  say "aws.sh: $err"
  exit 1
}

# The target groups of the service's live revision: the one in use and the alternate one
# Express Mode swaps to on a deployment. An Express Gateway service has no
# loadBalancers on describe-services; they are on its revision. Empty if not made yet.
current_groups() {
  local rev
  rev="$("$AWS" --region="$REGION" ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
    --query 'service.activeConfigurations[0].serviceRevisionArn' --output text 2>/dev/null || true)"
  case "$rev" in ""|None) return 0 ;; esac
  "$AWS" --region="$REGION" ecs describe-service-revisions --service-revision-arns "$rev" \
    --query 'serviceRevisions[0].loadBalancers[].[targetGroupArn,advancedConfiguration.alternateTargetGroupArn][]' \
    --output text 2>/dev/null | tr '\t' '\n' | { grep '^arn:' || true; } | sort -u | tr '\n' ' ' || true
}

# Wait for ECS to make them (create returns before it has); fail the script if it does not.
target_groups() {
  local groups waited=0
  show "$AWS" --region="$REGION" ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
    --query 'service.activeConfigurations[0].serviceRevisionArn' --output text
  show "$AWS" --region="$REGION" ecs describe-service-revisions --service-revision-arns '<revision-arn>' \
    --query 'serviceRevisions[0].loadBalancers[].[targetGroupArn,advancedConfiguration.alternateTargetGroupArn][]' --output text
  if [ "$DRY" = 1 ]; then echo '<target-group-arn>'; return; fi
  while :; do
    groups="$(current_groups)"
    if [ -n "${groups// /}" ]; then echo "$groups"; return; fi
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      say "aws.sh: no target group for $NAME after ${WAIT_SECONDS}s"
      exit 1
    fi
    sleep 10; waited=$((waited + 10))
  done
}

# Wait until one of the target groups has a healthy target; fail the script if none does.
wait_healthy() {
  local waited=0 group state
  show "$AWS" --region="$REGION" elbv2 describe-target-health --target-group-arn "<each target group>" \
    --query 'TargetHealthDescriptions[].TargetHealth.State' --output text
  [ "$DRY" = 1 ] && return 0
  while :; do
    for group in "$@"; do
      state="$("$AWS" --region="$REGION" elbv2 describe-target-health --target-group-arn "$group" \
        --query 'TargetHealthDescriptions[].TargetHealth.State' --output text)"
      case " $state " in *[[:space:]]healthy[[:space:]]*|*" healthy "*) say "  target healthy in ${group##*/}"; return 0 ;; esac
    done
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      say "aws.sh: no healthy target for $NAME after ${WAIT_SECONDS}s (last state: ${state:-none}); see the service events and $LOGS"
      exit 1
    fi
    sleep 10; waited=$((waited + 10))
  done
}

# Wait until the public endpoint answers 401 without the token: a healthy target can
# still be waiting for the load balancer to send it traffic (a blue/green switch), and
# until then the endpoint answers 503.
wait_serving() {
  local waited=0 host code
  show curl -sS -o /dev/null -w '%{http_code}' -X POST 'https://<endpoint>/v1/logs'
  [ "$DRY" = 1 ] && return 0
  host="$("$AWS" --region="$REGION" ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
    --query 'service.activeConfigurations[0].ingressPaths[0].endpoint' --output text)"
  while :; do
    code="$(curl -sS --connect-timeout 5 --max-time 15 -o /dev/null -w '%{http_code}' -X POST "https://${host#https://}/v1/logs" 2>/dev/null || true)"
    if [ "$code" = 401 ]; then say "  endpoint answers 401 without the token"; return 0; fi
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      say "aws.sh: $NAME's endpoint answers ${code:-nothing} (want 401) after ${WAIT_SECONDS}s"
      exit 1
    fi
    sleep 10; waited=$((waited + 10))
  done
}

trust() {  # $1: the service that may assume the role, $2: "account" to pin aws:SourceAccount
  if [ "$2" = account ]; then
    printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"%s"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"%s"}}}]}' "$1" "$ACCOUNT"
  else
    printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"%s"},"Action":"sts:AssumeRole"}]}' "$1"
  fi
}

role() {  # $1: name, $2: trusted service, $3: account|any
  if exists "$AWS" iam get-role --role-name "$1"; then
    run "$AWS" iam update-assume-role-policy --role-name "$1" --policy-document "$(trust "$2" "$3")"
  else
    run "$AWS" iam create-role --role-name "$1" --assume-role-policy-document "$(trust "$2" "$3")"
  fi
}

# Create the service; a role created moments ago can take about a minute to become
# assumable, so only that error is retried.
create_service() {
  local err try
  show "$@"
  [ "$DRY" = 1 ] && return 0
  for try in 1 2 3 4 5 6; do
    if err="$("$@" 2>&1 >/dev/null)"; then return 0; fi
    if ! printf '%s' "$err" | grep -qi 'assume'; then say "aws.sh: $err"; exit 1; fi
    say "  the new roles are not assumable yet (attempt $try); retrying in 20s"
    sleep 20
  done
  say "aws.sh: $err"
  exit 1
}

new_token() { openssl rand -base64 32 | tr -d '\n'; }

up() {
  a ecs create-cluster --cluster-name "$NAME"  # returns the existing cluster if there is one

  if ! exists "$AWS" --region="$REGION" s3api head-bucket --bucket "$BUCKET"; then
    if [ "$REGION" = us-east-1 ]; then
      a s3api create-bucket --bucket "$BUCKET"
    else
      a s3api create-bucket --bucket "$BUCKET" --create-bucket-configuration "LocationConstraint=$REGION"
    fi
    a s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
      BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  fi

  # Prompt text is kept 180 days, then the bucket deletes it. The rule covers the key prefix
  # livy writes under, so prompts.sink.prefix must stay empty.
  if [ -n "$PROMPTS_BUCKET" ]; then
    if [ "$PROMPTS_BUCKET" = "$BUCKET" ]; then
      echo "aws.sh: LIVY_PROMPTS_BUCKET must not be the ledger's bucket" >&2
      exit 2
    fi
    if ! exists "$AWS" --region="$REGION" s3api head-bucket --bucket "$PROMPTS_BUCKET"; then
      if [ "$REGION" = us-east-1 ]; then
        a s3api create-bucket --bucket "$PROMPTS_BUCKET"
      else
        a s3api create-bucket --bucket "$PROMPTS_BUCKET" --create-bucket-configuration "LocationConstraint=$REGION"
      fi
      a s3api put-public-access-block --bucket "$PROMPTS_BUCKET" --public-access-block-configuration \
        BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
    fi
    a s3api put-bucket-lifecycle-configuration --bucket "$PROMPTS_BUCKET" --lifecycle-configuration \
      '{"Rules":[{"ID":"prompt-text-180-days","Status":"Enabled","Filter":{"Prefix":"source=prompt-text/"},"Expiration":{"Days":180}}]}'
  fi

  # The token goes in on stdin and is never printed or put on a command line.
  if ! exists "$AWS" --region="$REGION" secretsmanager describe-secret --secret-id "$SECRET"; then
    show "$AWS" --region="$REGION" secretsmanager create-secret --name "$SECRET" --secret-string file:///dev/stdin
    say "  (stdin: a new random token, openssl rand -base64 32)"
    [ "$DRY" = 1 ] || new_token | "$AWS" --region="$REGION" secretsmanager create-secret \
      --name "$SECRET" --secret-string file:///dev/stdin >/dev/null
  fi
  local secret_arn
  secret_arn="$(value "arn:aws:secretsmanager:${REGION}:${ACCOUNT}:secret:${SECRET}-<suffix>" \
    "$AWS" --region="$REGION" secretsmanager describe-secret --secret-id "$SECRET" --query ARN --output text)"

  local found
  found="$(value '' "$AWS" --region="$REGION" logs describe-log-groups --log-group-name-prefix "$LOGS" \
    --query "logGroups[?logGroupName=='$LOGS'] | [0].logGroupName" --output text)"
  if [ "$found" != "$LOGS" ]; then
    a logs create-log-group --log-group-name "$LOGS"
  fi
  a logs put-retention-policy --log-group-name "$LOGS" --retention-in-days 30

  # The collector's own identity: it may put objects under the prefix and do nothing
  # else (no read, list or delete). S3 has no create-only action; object names are
  # fresh UUIDv7s.
  role "$TASK_ROLE" ecs-tasks.amazonaws.com account
  run "$AWS" iam put-role-policy --role-name "$TASK_ROLE" --policy-name write-otel-prefix --policy-document \
    "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"s3:PutObject\",\"Resource\":\"arn:aws:s3:::${BUCKET}/${PREFIX}/*\"}]}"
  # What ECS needs to start the task: pull the image, write logs, read this one secret.
  role "$EXEC_ROLE" ecs-tasks.amazonaws.com account
  run "$AWS" iam attach-role-policy --role-name "$EXEC_ROLE" --policy-arn "$EXEC_POLICY"
  run "$AWS" iam put-role-policy --role-name "$EXEC_ROLE" --policy-name read-token --policy-document \
    "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"secretsmanager:GetSecretValue\",\"Resource\":\"${secret_arn}\"}]}"
  # What ECS Express Mode needs to build the load balancer, target group and scaling.
  # Trust as AWS documents it, without a SourceAccount condition.
  role "$INFRA_ROLE" ecs.amazonaws.com any
  run "$AWS" iam attach-role-policy --role-name "$INFRA_ROLE" --policy-arn "$INFRA_POLICY"

  # The configuration travels as environment variables, so the running task is the
  # upstream image plus these two reviewable files.
  local container
  container="$(mktemp)"
  trap "rm -f '$container'" EXIT
  python3 - "$HERE" "$IMAGE" "$BUCKET" "$REGION" "$PREFIX" "$secret_arn" "$LOGS" > "$container" <<'PY'
import json, pathlib, sys
here, image, bucket, region, prefix, secret_arn, logs = sys.argv[1:]
env = {"LIVY_OTEL_BASE": pathlib.Path(here, "collector.yaml").read_text(),
       "LIVY_OTEL_OVERLAY": pathlib.Path(here, "s3.yaml").read_text(),
       "LIVY_OTEL_BUCKET": bucket, "LIVY_OTEL_REGION": region, "LIVY_OTEL_PREFIX": prefix}
print(json.dumps({
    "image": image,
    "containerPort": 8080,
    "command": ["--config=env:LIVY_OTEL_BASE", "--config=env:LIVY_OTEL_OVERLAY"],
    "environment": [{"name": k, "value": v} for k, v in env.items()],
    "secrets": [{"name": "LIVY_OTEL_TOKEN", "valueFrom": secret_arn}],
    "awsLogsConfiguration": {"logGroup": logs, "logStreamPrefix": "collector"},
}))
PY
  say "  (container file, written locally: $IMAGE, command --config=env:LIVY_OTEL_BASE --config=env:LIVY_OTEL_OVERLAY, env LIVY_OTEL_BASE=collector.yaml LIVY_OTEL_OVERLAY=s3.yaml LIVY_OTEL_BUCKET=$BUCKET LIVY_OTEL_REGION=$REGION LIVY_OTEL_PREFIX=$PREFIX, secret LIVY_OTEL_TOKEN from $SECRET)"
  local roles=(--execution-role-arn "arn:aws:iam::${ACCOUNT}:role/${EXEC_ROLE}"
               --task-role-arn "arn:aws:iam::${ACCOUNT}:role/${TASK_ROLE}")
  # Smallest Fargate size, ARM (Graviton, cheaper); one task always, a second under load.
  local shape=(--primary-container "file://$container" --cpu 256 --memory 512 --cpu-architecture ARM64
               --scaling-target minTaskCount=1,maxTaskCount=2 --health-check-path /)
  local status
  status="$(service_status)"
  case "$status" in
    ACTIVE)
      a ecs update-express-gateway-service --service-arn "$SERVICE_ARN" "${roles[@]}" "${shape[@]}" ;;
    DRAINING)
      say "aws.sh: $NAME is still being deleted; run up again once it is gone"
      exit 1 ;;
    *)
      create_service "$AWS" --region="$REGION" ecs create-express-gateway-service --cluster "$NAME" \
        --service-name "$NAME" --infrastructure-role-arn "arn:aws:iam::${ACCOUNT}:role/${INFRA_ROLE}" \
        "${roles[@]}" "${shape[@]}" ;;
  esac

  # The collector answers every request without the token with 401, the load
  # balancer's health probe included, and serves nothing on a public path without
  # it. So "healthy" is "answers 401 on /": the process is up and refusing.
  local groups group
  groups="$(target_groups)"
  for group in $groups; do
    a elbv2 modify-target-group --target-group-arn "$group" --health-check-path / --matcher HttpCode=401
  done

  wait_healthy $groups
  wait_serving

  say ""
  say "Endpoint:  $AWS --region=$REGION ecs describe-express-gateway-service --service-arn $SERVICE_ARN --query 'service.activeConfigurations[0].ingressPaths[0].endpoint' --output text"
  say "Token:     $AWS --region=$REGION secretsmanager get-secret-value --secret-id $SECRET --query SecretString --output text"
  say "Then:      LIVY_OTEL_BUCKET=$BUCKET LIVY_AWS_REGION=$REGION $0 check"
}

# POST one OTLP/JSON body; the token, if any, reaches curl on stdin, never in argv.
post() {  # $1 url, $2 body, $3 token or empty
  local headers=""
  [ -z "$3" ] || headers="$(printf 'header = "Authorization: Bearer %s"\n' "$3")"
  printf '%s\n' "$headers" | curl -sS --connect-timeout 5 --max-time 30 -K - -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' -d "$2" "$1/v1/logs"
}

check() {
  local host url token without with record
  # One record from service "livy-check": a 200 means it is stored, which proves the
  # task role's grant as well as the token. It stays in the bucket; derivation can drop it.
  record="{\"resourceLogs\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"livy-check\"}}]},\"scopeLogs\":[{\"logRecords\":[{\"timeUnixNano\":\"$(date +%s)000000000\",\"attributes\":[{\"key\":\"event.name\",\"value\":{\"stringValue\":\"livy.check\"}}]}]}]}]}"
  host="$(value '<endpoint>' "$AWS" --region="$REGION" ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
    --query 'service.activeConfigurations[0].ingressPaths[0].endpoint' --output text)"
  show "$AWS" --region="$REGION" secretsmanager get-secret-value --secret-id "$SECRET" --query SecretString --output text
  show curl -sS -X POST -H 'Content-Type: application/json' -d '<one livy-check record>' 'https://<endpoint>/v1/logs'
  show curl -sS -K '<the token on stdin>' -X POST -H 'Content-Type: application/json' -d '<one livy-check record>' 'https://<endpoint>/v1/logs'
  [ "$DRY" = 1 ] && return 0
  token="$("$AWS" --region="$REGION" secretsmanager get-secret-value --secret-id "$SECRET" --query SecretString --output text)"
  url="https://${host#https://}"
  url="${url%/}"
  without="$(post "$url" "$record" "")"
  with="$(post "$url" "$record" "$token")"
  say "without token: HTTP $without (want 401); with token: HTTP $with (want 200: one record stored under s3://$BUCKET/$PREFIX/)"
  [ "$without" = 401 ] && [ "$with" = 200 ]
}

# Wait until ECS has removed a target group it made, so the role it uses is still
# there while it cleans up.
wait_gone() {  # $1 target group arn
  local waited=0 err
  show "$AWS" --region="$REGION" elbv2 describe-target-groups --target-group-arns "$1"
  [ "$DRY" = 1 ] && return 0
  while err="$("$AWS" --region="$REGION" elbv2 describe-target-groups --target-group-arns "$1" 2>&1 >/dev/null)"; do
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      say "aws.sh: $1 still exists after ${WAIT_SECONDS}s; the roles are kept so ECS can finish. Run down again later (it remembers what to wait for in $PENDING)."
      exit 1
    fi
    sleep 10; waited=$((waited + 10))
  done
  if ! printf '%s' "$err" | grep -Eq "$NOT_FOUND"; then say "aws.sh: $err"; exit 1; fi
}

down() {
  # The bucket and everything in it stay: they hold Livy's own uploads too.
  local status groups="" group
  status="$(service_status)"
  # The target groups ECS made are recorded before anything is deleted, and kept in
  # PENDING until they are gone, so a down that times out and is run again still waits
  # for them before deleting the role ECS cleans up with, even once ECS has forgotten
  # the service.
  [ "$DRY" = 1 ] || [ ! -f "$PENDING" ] || groups="$(cat "$PENDING")"
  if [ "$status" = ACTIVE ] || [ "$status" = DRAINING ] || [ "$status" = INACTIVE ]; then
    show "$AWS" --region="$REGION" ecs describe-service-revisions --service-revision-arns '<revision-arn>' \
      --query 'serviceRevisions[0].loadBalancers[].[targetGroupArn,advancedConfiguration.alternateTargetGroupArn][]' --output text
    if [ "$DRY" = 1 ]; then groups="$groups <target-group-arn>"; else groups="$groups $(current_groups)"; fi
  fi
  groups="$(printf '%s\n' $groups | { grep -v '^None$' || true; } | sort -u | tr '\n' ' ')"
  if [ "$DRY" = 0 ] && [ -n "${groups// /}" ]; then
    mkdir -p "$(dirname "$PENDING")"
    printf '%s\n' $groups > "$PENDING"
  fi
  if [ "$status" = ACTIVE ] || [ "$status" = DRAINING ]; then
    [ "$status" = ACTIVE ] && a ecs delete-express-gateway-service --service-arn "$SERVICE_ARN"
    a ecs wait services-inactive --cluster "$NAME" --services "$NAME"
  fi
  for group in $groups; do
    wait_gone "$group"
  done
  [ "$DRY" = 1 ] || rm -f "$PENDING"
  local cluster
  cluster="$(value ACTIVE "$AWS" --region="$REGION" ecs describe-clusters --clusters "$NAME" \
    --query 'clusters[0].status' --output text)"
  [ "$cluster" != ACTIVE ] || a ecs delete-cluster --cluster "$NAME" >/dev/null
  if exists "$AWS" --region="$REGION" secretsmanager describe-secret --secret-id "$SECRET"; then
    a secretsmanager delete-secret --secret-id "$SECRET" --force-delete-without-recovery >/dev/null
  fi
  remove "$AWS" --region="$REGION" logs delete-log-group --log-group-name "$LOGS"
  if exists "$AWS" iam get-role --role-name "$TASK_ROLE"; then
    remove "$AWS" iam delete-role-policy --role-name "$TASK_ROLE" --policy-name write-otel-prefix
    run "$AWS" iam delete-role --role-name "$TASK_ROLE"
  fi
  if exists "$AWS" iam get-role --role-name "$EXEC_ROLE"; then
    remove "$AWS" iam delete-role-policy --role-name "$EXEC_ROLE" --policy-name read-token
    remove "$AWS" iam detach-role-policy --role-name "$EXEC_ROLE" --policy-arn "$EXEC_POLICY"
    run "$AWS" iam delete-role --role-name "$EXEC_ROLE"
  fi
  if exists "$AWS" iam get-role --role-name "$INFRA_ROLE"; then
    remove "$AWS" iam detach-role-policy --role-name "$INFRA_ROLE" --policy-arn "$INFRA_POLICY"
    run "$AWS" iam delete-role --role-name "$INFRA_ROLE"
  fi
  [ "$DRY" = 1 ] || say "Removed the service, cluster, token, log group and roles. s3://$BUCKET is unchanged."
}

"$ACTION"
