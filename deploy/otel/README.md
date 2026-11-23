# OTEL: Claude Code and Codex telemetry into your bucket

Claude Code and Codex CLI can export OpenTelemetry: logs (an event per API call, prompt,
tool decision and result, with tokens, cost, model and latency), metrics (token, cost and
activity counters) and traces (spans per prompt, API call and tool call). Livy can send
all three to a place you choose. Nothing about what is then used is decided here: every
signal from every client is kept, and only identity and free text are removed.

## Two ways to set it up

### A. Send to an OTLP endpoint you already have (nothing deployed)

Use this when you already run an OpenTelemetry Collector, or use a vendor that takes
OTLP over HTTP (Grafana Cloud, Honeycomb, Datadog, New Relic and others).

```sh
export OTEL_AUTH="Bearer <your token>"       # whatever header value your endpoint expects
livy install --otel https://otlp.example.com --otel-header-env OTEL_AUTH
```

- Claude Code gets the header through its `otelHeadersHelper`, which install points at
  `livy otel-headers OTEL_AUTH`. That command reads the variable from Claude Code's own
  environment, so no secret is written to its settings. Export it in your shell profile:
  if it is unset where Claude Code starts, no header is sent and the endpoint answers 401.
- Codex reads headers only literally, so install writes the value it sees at install time
  into `~/.codex/config.toml` and makes that file mode 0600. After a token change, run
  `livy install` again so Codex gets the new one.
- Install turns on every signal for both clients: logs, metrics and traces. Turn one off
  for an endpoint that does not take it with `--otel-logs none`, `--otel-metrics none` or
  `--otel-traces none` (each applies to Claude Code and Codex alike).
- Identity and content removal is then your endpoint's job. To get the same removal as
  option B, run this folder's `collector.yaml` in your own collector. Give it a second
  `--config` that defines your exporter and lists it under `service.pipelines` for
  `logs`, `metrics` and `traces` (as `s3.yaml` does), and set `LIVY_OTEL_TOKEN`: the
  collector will not start without one.

### B. Deploy a fresh collector into your own cloud (this folder)

One script per cloud: Google Cloud (Cloud Run, then Pub/Sub, then Cloud Storage) or AWS
(ECS Express Mode on Fargate, then S3). Data lands in the bucket Livy's own uploads use,
beside `source=agent-hooks/`. Each script has an idempotent `up`; a `check` that is
refused without the token and stores one `livy-check` record with it; and a one-command
`down` that keeps the bucket. `--dry-run` prints every command and runs none. Run a
script with no arguments for its settings and the permissions the deploying account needs.

```sh
# Google Cloud
export LIVY_GCP_PROJECT=my-project LIVY_GCP_REGION=us-central1 LIVY_OTEL_BUCKET=my-livy-bucket
deploy/otel/gcp.sh --dry-run up      # read the plan
deploy/otel/gcp.sh up
deploy/otel/gcp.sh check

# AWS
export LIVY_AWS_REGION=us-east-1 LIVY_OTEL_BUCKET=my-livy-bucket
deploy/otel/aws.sh --dry-run up
deploy/otel/aws.sh up
deploy/otel/aws.sh check
```

`up` ends by printing the commands that read the endpoint and the token. Then point the
clients at it exactly as in option A:

```sh
# Google Cloud
export OTEL_AUTH="Bearer $(gcloud secrets versions access latest --secret=livy-otel-token --project=my-project)"
# AWS
export OTEL_AUTH="Bearer $(aws secretsmanager get-secret-value --secret-id livy-otel-token --query SecretString --output text)"

livy install --otel https://<endpoint> --otel-header-env OTEL_AUTH
```

(The secret is `<LIVY_OTEL_NAME>-token`; `livy-otel-token` by default.) On Google Cloud,
`check` publishes one record and then waits about 75 s for a new object in the bucket.

| File | What it is |
|---|---|
| `IMAGE` | The pinned upstream image, `otel/opentelemetry-collector-contrib`, by digest. No image of ours. |
| `collector.yaml` | Receiver, token check and every redaction step. Shared by both clouds. |
| `pubsub.yaml`, `s3.yaml` | The storage overlay for each cloud: `--config=collector.yaml --config=s3.yaml`. |
| `gcp.sh`, `aws.sh` | Deploy, check and tear down. |
| `prove_redaction.py` | Runs the config in the pinned image and fails if any identity or content gets through. |

## What it stores

Every signal, from every client, on both clouds:

| | Logs | Metrics | Traces |
|---|---|---|---|
| Claude Code | every event | every data point | every span (beta tracing) |
| Codex | every event | every data point | every span |

Measured on real runs (one trivial prompt each, through the emulators): Claude Code
sent 13 log records, 7 data points and 2 spans; Codex sent 19, 209 and 980. The same
counts landed on both paths.

Where it lands:

```
Google Cloud: <bucket>/source=otel/dt=2026-11-06/hh_mm_ssZ_<id>.avro
              Avro, one record per export request: the OTLP protobuf bytes plus Pub/Sub
              attributes; ce-type names the signal. A file closes after 60 s or 10 MB.
AWS:          <bucket>/source=otel/dt=2026-11-06/{logs,metrics,traces}_<uuidv7>.json.gz
              gzipped OTLP JSON, one object per export request.
```

Every resource attribute that is not identity is kept, including the `engineer.handle`
that `livy install` sets. Set `LIVY_OTEL_PREFIX` if your Livy sink writes under a prefix (for example
`team-a/source=otel`).

## What it removes, before anything is stored

- **Identity.** It is removed everywhere an attribute can sit (resource, scope, record,
  data point, exemplar, span, span event), with one exception: span links (below).
  - Every `user.*` attribute: email, account uuid and id, the client's random user id,
    groups.
  - Every `enduser.*`, `host.*` and `process.*` attribute: machine name, OS user name,
    command line.
  - `organization.id`, `identity.source`, and the local paths `workspace.host_paths` and
    `cwd`.
  - Both clients send the email by default. It never reaches the bucket.
  - Span links keep their attributes, because the collector cannot edit them. Claude
    Code's links carry only `link.type`.
- **Claude Code content.** Livy never turns on Claude Code's content flags
  (`OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_ASSISTANT_RESPONSES`, `OTEL_LOG_TOOL_DETAILS`,
  `OTEL_LOG_RAW_API_BODIES`). The collector deletes what they would add anyway:
  - on log records: prompt and response text, tool parameters and input, free error
    text, raw API bodies;
  - on spans: prompt, context, system prompt, model output, tool input, file path,
    command, hook definitions, error text and status message;
  - every span event except the retry marker.
- **Codex free text.** Codex attaches tool arguments (shell commands, patches), paths and
  error messages to its records whatever its config says. So records from any client but
  Claude Code are cut down before storage:
  - each log record, span, span event, data point and exemplar keeps only the attributes
    on an allowlist, built from the keys real Codex 0.167 sends, with free text taken
    out;
  - log bodies are emptied.

  A key Codex adds later is dropped until it is listed in `collector.yaml`.

A redaction step that cannot run fails the request (the client retries), rather than
letting data through. `prove_redaction.py` checks all of this against the pinned image:

```sh
python3 deploy/otel/prove_redaction.py
# prove_redaction: ok. no, empty, wrong and unprefixed tokens refused (401); pubsub.yaml valid;
# logs, metrics, traces accepted; 15 identity keys and all content dropped everywhere; 17 metadata keys kept
```

## How it is built, and why

**Auth: a bearer token from the cloud's secret manager.** Every request needs
`Authorization: Bearer <token>`. Without it the collector answers 401, and it refuses to
start with no token configured.
- IAM auth (a short-lived Google ID token) would need a helper on every laptop. Codex
  reads its headers once at start, so a long-running Codex would export with an expired
  token.
- One static token works for both clients on both clouds.
- Rotate it by adding a new secret version and running `up` again, then `livy install`
  again on each machine (Codex holds the token literally).

**Synchronous writes.** There is no batch processor and no sending queue. A request is
answered only after storage accepted it: the S3 bucket, or the Pub/Sub topic. A storage
failure is therefore a 503, which the client retries. A queue would answer 200 first and
lose the data when the instance stops.

**Google Cloud: Pub/Sub, then a Cloud Storage subscription.** The contrib Cloud Storage
exporter cannot write metrics, so the collector publishes every request to a Pub/Sub
topic. A Cloud Storage subscription writes the messages to the bucket as Avro.
- The collector's account may only publish to that topic and read its token.
- The subscription writes as a second account. That account may create objects and list
  names in the bucket, and may read, overwrite or delete nothing. Only Pub/Sub's service
  agent can act as it.
- An IAM condition cannot hold that writer to the prefix: Pub/Sub refuses to create the
  subscription. The prefix is held by the subscription's own config instead.
- The subscription never expires. Pub/Sub's default deletes it after 31 idle days.

**AWS: the contrib S3 exporter, on ECS Express Mode (Fargate).** The S3 exporter writes
all three signals.
- App Runner, the scale-to-zero option, has been closed to new customers since 30 April
  2026. AWS points to ECS Express Mode instead.
- ECS Express Mode gives one HTTPS URL (`*.ecs.<region>.on.aws`, on AWS's certificate, so
  no domain is needed) from one API call.
- The task runs as its own role, allowed `s3:PutObject` under the prefix and nothing
  else. Object names are fresh UUIDv7s.

## Cost

Rough list prices, us-central1 and us-east-1, November 2026. An active user exports about
one request every few seconds while working.

| | Idle | Per active user, per month |
|---|---|---|
| Google Cloud | about $0.10 (the secret; Cloud Run scales to zero, Pub/Sub has no idle charge) | under $1 (Cloud Run requests within the free tier for a few users; Pub/Sub's per-message minimum; one object per minute of activity) |
| AWS | **about $35 to $50**: load balancer $16, its public IPv4 addresses $7 to $22 ($3.65 each, one per availability zone), one 0.25 vCPU / 0.5 GB ARM task $7, the task's public IPv4 $4. Neither ECS nor the load balancer scales to zero. | about $1 (one PUT per request, $0.005 per 1,000) |

Raising the clients' export interval (`OTEL_LOGS_EXPORT_INTERVAL`, default 5 s) cuts the
request count.

## What install turns on in the clients

`livy install --otel <endpoint>` turns on logs, metrics and traces for both Claude Code and
Codex. For Claude Code that includes its beta tracing (`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA`).
To leave a signal off, pass `--otel-logs none`, `--otel-metrics none` or `--otel-traces
none`; each applies to both clients. Nothing needs to be set by hand.

## Try it locally

```sh
docker run --rm -p 127.0.0.1:8080:8080 -e LIVY_OTEL_TOKEN=local-token \
  -e LIVY_OTEL_BUCKET=livy -e LIVY_OTEL_REGION=us-east-1 \
  -e LIVY_OTEL_S3_ENDPOINT=http://<minio>:9000 -e LIVY_OTEL_S3_PATH_STYLE=true \
  -e AWS_ACCESS_KEY_ID=<minio user> -e AWS_SECRET_ACCESS_KEY=<minio password> \
  -v "$PWD/deploy/otel:/cfg:ro" "$(grep -v '^#' deploy/otel/IMAGE)" \
  --config=/cfg/collector.yaml --config=/cfg/s3.yaml
```

For the Pub/Sub overlay against the emulator (`gcloud beta emulators pubsub start`),
replace the S3 variables with these:
- `LIVY_OTEL_PROJECT` and `LIVY_OTEL_TOPIC`
- `LIVY_OTEL_PUBSUB_ENDPOINT=<host>:8085`
- `LIVY_OTEL_PUBSUB_INSECURE=true`

The emulator has no Cloud Storage subscriptions. Read the topic back with a pull
subscription.

## Upgrading the collector

Change `IMAGE` to a new release's multi-arch digest, run `prove_redaction.py`, and run the
recipe's `up` again.
- A new release can rename a processor option or change an exporter's output. Read its
  changelog for `transform`, `filter`, `awss3` and `googlecloudpubsub`.
- A new Codex release can add attributes. They are dropped until added to the allowlists
  in `collector.yaml`.
