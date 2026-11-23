#!/usr/bin/env python3
"""Run collector.yaml in the pinned image and prove what reaches storage.

Sends one OTLP request per signal through the REAL processors, with the S3 overlay's
pipelines writing to a file instead of a bucket. The requests carry every identity
attribute (user.*, enduser.*, host.*, organization.id, identity.source,
process.*, workspace.host_paths) everywhere an attribute can sit (metric exemplars
included; span links are not, see collector.yaml), every content
attribute Claude Code documents, Codex's free-text fields and metrics, and an unlisted
span event. Fails if any of it comes out, if a request without the right token is
accepted, if pubsub.yaml does not validate, or if the metadata the export is for (handle,
session, model, tokens, cost, tool name, error class) does not come out.

    python3 deploy/otel/prove_redaction.py

Needs docker and python3 (stdlib only). Run it after any change to collector.yaml or
IMAGE, and before a release.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
PORT = int(os.environ.get("OTEL_PROOF_PORT", "18090"))
STARTUP_SECONDS = int(os.environ.get("OTEL_PROOF_STARTUP_SECONDS", "60"))
TOKEN = "proof-token"
MARK = "LEAKED-"

# Merged over collector.yaml + s3.yaml: lists replace, so every pipeline writes the file.
OVERRIDE = {
    "exporters": {"file": {"path": "/out/export.jsonl"}},
    "service": {"pipelines": {name: {"exporters": ["file"]} for name in ("logs", "metrics", "traces")}},
}

IDENTITY = ("user.email", "user.id", "user.account_uuid", "user.account_id", "user.groups", "user.name",
            "enduser.id", "host.name", "host.id", "process.owner", "process.command_args", "organization.id",
            "identity.source", "workspace.host_paths", "cwd")
CLAUDE_LOG_CONTENT = ("prompt", "prompt_text", "response", "tool_parameters", "tool_input", "body",
                      "body_ref", "system_prompt")
CLAUDE_SPAN_CONTENT = ("user_prompt", "new_context", "system_reminders", "system_prompt_preview",
                       "user_system_prompt", "response.model_output", "tool_input", "file_path",
                       "full_command", "hook_definitions")
KEPT = {"livy.handle": "octocat", "service.name": "claude-code", "session.id": "s-1", "model": "m-1",
        "event.name": "api_request", "cost_usd": "0.01", "input_tokens": "12", "tool_name": "Bash",
        "error_type": "Error_ENOENT", "error_class": "Error_ENOENT", "attempt": "1",
        "input_token_count": "34", "conversation.id": "c-1", "type": "input",
        "turn.id": "t-1", "call_id": "call-1", "outcome": "ok"}


def _kv(key: str, value: str) -> dict[str, Any]:
    return {"key": key, "value": {"stringValue": value}}


def _identity() -> list[dict[str, Any]]:
    return [_kv(k, MARK + k) for k in IDENTITY]


def _resource(service: str | None) -> dict[str, Any]:
    attrs = _identity() + [_kv("livy.handle", "octocat")]
    if service:
        attrs.append(_kv("service.name", service))
    return {"attributes": attrs}


def _scope(name: str) -> dict[str, Any]:
    return {"name": name, "attributes": _identity()}


def logs() -> dict[str, Any]:
    api_request = {"timeUnixNano": "1", "body": {"stringValue": "claude_code.api_request"}, "attributes":
                   _identity() + [_kv("event.name", "api_request"), _kv("session.id", "s-1"), _kv("model", "m-1"),
                                  _kv("cost_usd", "0.01"), _kv("input_tokens", "12")]}
    prompt = {"timeUnixNano": "1", "body": {"stringValue": "claude_code.user_prompt"}, "attributes":
              [_kv(k, MARK + k) for k in CLAUDE_LOG_CONTENT] + [_kv("event.name", "user_prompt")]}
    tool_result = {"timeUnixNano": "1", "body": {"stringValue": "claude_code.tool_result"}, "attributes":
                   [_kv("event.name", "tool_result"), _kv("tool_name", "Bash"), _kv("error_type", "Error_ENOENT"),
                    _kv("error", MARK + "ENOENT: /Users/someone/file")]}
    api_error = {"timeUnixNano": "1", "body": {"stringValue": "claude_code.api_error"}, "attributes":
                 [_kv("event.name", "api_error"), _kv("error_code", "529"), _kv("error", MARK + "api error text")]}
    claude = {"resource": _resource("claude-code"),
              "scopeLogs": [{"scope": _scope("com.anthropic.claude_code.events"),
                             "logRecords": [api_request, prompt, tool_result, api_error]}]}
    # Codex: free text on its records whatever its config says; only the allowlist passes.
    codex_record = {"timeUnixNano": "1", "body": {"stringValue": MARK + "body"}, "attributes":
                    _identity() + [_kv("event.name", "codex.tool_result"), _kv("arguments", MARK + "arguments"),
                                   _kv("output", MARK + "output"), _kv("error.message", MARK + "error"),
                                   _kv("input_token_count", "34"), _kv("conversation.id", "c-1")]}
    codex = {"resource": _resource("codex_exec"),
             "scopeLogs": [{"scope": _scope("codex_otel"), "logRecords": [codex_record]}]}
    # No service.name at all: treated like Codex, never trusted as Claude Code.
    anonymous = {"resource": _resource(None),
                 "scopeLogs": [{"scope": _scope("unknown"), "logRecords": [dict(codex_record)]}]}
    return {"resourceLogs": [claude, codex, anonymous]}


def metrics() -> dict[str, Any]:
    point = {"timeUnixNano": "1", "asDouble": 12,
             "attributes": _identity() + [_kv("session.id", "s-1"), _kv("type", "input"), _kv("model", "m-1")],
             "exemplars": [{"timeUnixNano": "1", "asDouble": 12, "filteredAttributes": _identity()}]}
    metric = {"name": "claude_code.token.usage", "unit": "tokens",
              "sum": {"aggregationTemporality": 1, "isMonotonic": True, "dataPoints": [point]}}
    codex_point = {"timeUnixNano": "1", "asDouble": 1,
                   "attributes": [_kv("command", MARK + "codex metric"), _kv("secure_error", MARK + "error"),
                                  _kv("tool.arguments", MARK + "arguments"), _kv("outcome", "ok")] + _identity(),
                   "exemplars": [{"timeUnixNano": "1", "asDouble": 1,
                                  "filteredAttributes": [_kv("stdout", MARK + "stdout")]}]}
    codex_metric = {"name": "codex.tool.call", "sum": {"aggregationTemporality": 1, "isMonotonic": True,
                                                       "dataPoints": [codex_point]}}
    return {"resourceMetrics": [{"resource": _resource("claude-code"),
                                 "scopeMetrics": [{"scope": _scope("com.anthropic.claude_code"),
                                                   "metrics": [metric]}]},
                                {"resource": _resource("codex_exec"),
                                 "scopeMetrics": [{"scope": _scope("codex_otel"), "metrics": [codex_metric]}]}]}


def _span(span_id: str, name: str, attrs: list[dict[str, Any]], events: list[dict[str, Any]]) -> dict[str, Any]:
    return {"traceId": "01" * 16, "spanId": span_id, "name": name, "kind": 1,
            "startTimeUnixNano": "1", "endTimeUnixNano": "2", "attributes": attrs, "events": events}


def traces() -> dict[str, Any]:
    tool = _span("02" * 8, "claude_code.tool",
                 [_kv(k, MARK + k) for k in CLAUDE_SPAN_CONTENT] + _identity() +
                 [_kv("tool_name", "Bash"), _kv("session.id", "s-1")],
                 [{"name": "tool.output", "timeUnixNano": "1",
                   "attributes": [_kv(k, MARK + k) for k in ("content", "output", "diff", "bash_command")]},
                  {"name": "gen_ai.request.attempt", "timeUnixNano": "1",
                   "attributes": [_kv("attempt", "1")] + _identity()},
                  {"name": "claude_code.future_event", "timeUnixNano": "1",
                   "attributes": [_kv("detail", MARK + "future_event")]}])
    execution = _span("03" * 8, "claude_code.tool.execution",
                      [_kv("error", MARK + "ENOENT: /Users/someone/file"), _kv("error_class", "Error_ENOENT")],
                      [{"name": "exception", "timeUnixNano": "1",
                        "attributes": [_kv("exception.message", MARK + "exception")]}])
    execution["status"] = {"code": 2, "message": MARK + "status"}
    claude = {"resource": _resource("claude-code"),
              "scopeSpans": [{"scope": _scope("com.anthropic.claude_code"), "spans": [tool, execution]}]}

    def foreign(service: str | None, span_id: str) -> dict[str, Any]:
        # Another client's spans are stored, minus free text and identity.
        span = _span(span_id, "codex.tool_call",
                     [_kv("arguments", MARK + "arguments"), _kv("error.message", MARK + "error"),
                      _kv("code.file.path", MARK + "path"), _kv("workdir", MARK + "workdir"),
                      _kv("gen_ai.input.messages", MARK + "messages"), _kv("turn.id", "t-1")] + _identity(),
                     [{"name": "codex.tool_result", "timeUnixNano": "1",
                       "attributes": [_kv("call_id", "call-1"), _kv("arguments", MARK + "event arguments")]},
                      # A tracing event named after its formatted message.
                      {"name": MARK + "failed to read /Users/someone/file", "timeUnixNano": "1"}])
        return {"resource": _resource(service), "scopeSpans": [{"scope": {"name": "codex"}, "spans": [span]}]}

    return {"resourceSpans": [claude, foreign("codex_exec", "04" * 8), foreign(None, "05" * 8)]}


def _post(path: str, payload: dict[str, Any], authorization: str | None) -> int:
    headers = {"Content-Type": "application/json"}
    if authorization is not None:
        headers["Authorization"] = authorization
    request = urllib.request.Request(f"http://127.0.0.1:{PORT}{path}", data=json.dumps(payload).encode(),
                                     method="POST", headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=10) as resp:
            return int(resp.status)
    except urllib.error.HTTPError as err:
        return int(err.code)


def _wait_ready() -> None:
    deadline = time.monotonic() + STARTUP_SECONDS
    while True:
        try:
            _post("/v1/logs", {"resourceLogs": []}, f"Bearer {TOKEN}")
            return
        except (urllib.error.URLError, ConnectionError, OSError):
            if time.monotonic() > deadline:
                raise
            time.sleep(1)


def _read_export(path: Path, want: int) -> list[Any]:
    """The exported lines, once the file exporter has written `want` complete ones."""
    deadline = time.monotonic() + STARTUP_SECONDS
    lines: list[str] = []
    while time.monotonic() < deadline:
        lines = [ln for ln in (path.read_text().splitlines(keepends=True) if path.exists() else [])
                 if ln.endswith("\n")]
        if len(lines) >= want:
            break
        time.sleep(1)
    return [json.loads(ln) for ln in lines]


def _flatten(node: Any) -> list[Any]:
    """Every key/value pair in the export, wherever it sits."""
    found: list[Any] = []
    if isinstance(node, dict):
        if set(node) == {"key", "value"}:
            found.append(node)
        for value in node.values():
            found.extend(_flatten(value))
    elif isinstance(node, list):
        for item in node:
            found.extend(_flatten(item))
    return found


def _scalar(value: dict[str, Any]) -> str:
    for kind in ("stringValue", "intValue", "doubleValue", "boolValue"):
        if kind in value:
            return str(value[kind])
    return json.dumps(value)


def main() -> int:
    image = next(ln.strip() for ln in (HERE / "IMAGE").read_text().splitlines() if ln.strip() and not ln.startswith("#"))
    with tempfile.TemporaryDirectory() as tmp:
        out = Path(tmp)
        out.chmod(0o777)  # the image runs as a non-root user
        (out / "override.json").write_text(json.dumps(OVERRIDE))
        name = f"livy-otel-proof-{os.getpid()}"
        subprocess.run(["docker", "run", "-d", "--name", name, "-p", f"127.0.0.1:{PORT}:8080",
                        "-e", f"LIVY_OTEL_TOKEN={TOKEN}", "-e", "LIVY_OTEL_BUCKET=unused",
                        "-e", "LIVY_OTEL_REGION=unused", "-v", f"{HERE}:/cfg:ro", "-v", f"{out}:/out", image,
                        "--config=/cfg/collector.yaml", "--config=/cfg/s3.yaml", "--config=/out/override.json"],
                       check=True, stdout=subprocess.DEVNULL)
        try:
            _wait_ready()
            refused = {label: _post("/v1/logs", logs(), header) for label, header in
                       (("no header", None), ("empty token", "Bearer "), ("wrong token", "Bearer wrong-token"),
                        ("bare token", TOKEN))}
            statuses = {path: _post(path, body, f"Bearer {TOKEN}")
                        for path, body in (("/v1/logs", logs()), ("/v1/metrics", metrics()), ("/v1/traces", traces()))}
            exported = _read_export(out / "export.jsonl", want=3)
        finally:
            container_logs = subprocess.run(["docker", "logs", name], capture_output=True, text=True).stderr
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, check=False)
    problems = [f"{label}: HTTP {status}, not 401" for label, status in refused.items() if status != 401]
    gcs = subprocess.run(["docker", "run", "--rm", "-e", f"LIVY_OTEL_TOKEN={TOKEN}", "-e", "LIVY_OTEL_PROJECT=unused",
                          "-e", "LIVY_OTEL_TOPIC=unused", "-v", f"{HERE}:/cfg:ro",
                          image, "validate", "--config=/cfg/collector.yaml", "--config=/cfg/pubsub.yaml"],
                         capture_output=True, text=True)
    if gcs.returncode != 0:
        problems.append(f"pubsub.yaml does not validate: {gcs.stderr.strip()}")
    problems += [f"{path}: HTTP {status}" for path, status in statuses.items() if status != 200]
    if len(exported) < 3:
        problems.append(f"{len(exported)} of 3 exports written\n{container_logs}")
    text = json.dumps(exported)
    pairs = {(kv["key"], _scalar(kv["value"])) for kv in _flatten(exported)}
    problems += sorted({f"leaked: {k}" for k, v in pairs if v.startswith(MARK)})
    if MARK in text and not any(p.startswith("leaked") for p in problems):
        problems.append("leaked: a marked value outside any attribute (a body or status message)")
    problems += [f"identity key present: {k}" for k in sorted({k for k, _ in pairs if k in IDENTITY})]
    problems += [f"dropped wanted attribute {k}={v}" for k, v in KEPT.items() if (k, v) not in pairs]
    if problems:
        print("prove_redaction: FAILED\n  " + "\n  ".join(problems), file=sys.stderr)
        return 1
    print(f"prove_redaction: ok. no, empty, wrong and unprefixed tokens refused (401); pubsub.yaml valid; "
          f"logs, metrics, traces accepted; "
          f"{len(IDENTITY)} identity keys and all content dropped everywhere; {len(KEPT)} metadata keys kept",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
