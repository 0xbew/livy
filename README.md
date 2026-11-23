# Livy

<!-- Brand placeholder: the wordmark and icon go here once the brand is settled. -->

Livy records the facts about agent-driven engineering that exist only at the moment
they happen: session and model timelines, which instruction files loaded, PRs opened,
turn ends, and its own errors. Claude Code hooks append them, one JSON line per event,
to a local ledger.

**Status: v0.1 candidate.** Livy is a test-first Go rewrite of an earlier Python recorder.
This build records (`livy hook`, `livy git post-checkout`), ships (`livy emit` to a local
folder, Google Cloud Storage or S3), installs itself (`livy install` / `uninstall`) and
proves it is alive (`livy doctor`). It extracts token use from Claude Code and Codex
transcripts (`livy extract`). Optional spot-checks (`livy spotcheck`, off without a key) log
what a fast judgment model would decide about the agent's next move.

## What it records, and what it never records

- **Records facts only:** event name, UTC time, repo (`owner/name`), branch, session and
  prompt ids, model names, the hook and tool names, the size of the hook's input, and the
  paths of instruction files that loaded (repo-relative inside the repo, `~/...` under
  your home folder, otherwise as given).
- **Ticket key:** `work.started`, `prompt.captured` and `commit.recorded` carry `ticket_key`: the
  first match of `ticket.regex` in the branch name (for a commit, then in its message's subject),
  upper-cased (a capture group, when the pattern has one), or `no-ticket` when none matches. A
  pattern that does not compile is said in a `ledger.error` and the default decides.
- **Codex:** through Codex's hooks (`livy hook --harness codex <event>`), session start and end,
  each prompt's id (its turn id), each tool call, subagent launches and turn ends, every line
  `harness: codex` with its model. A subagent's brief is not taken for a prompt, and a
  `gh pr create` counts as a PR opened only when its call exited 0 in the rollout. Codex reports
  no loaded instruction file, model switch or skill load, so none is recorded for it.
- **Commits:** `commit.recorded` for each commit (its SHA, and for an agent's commit the
  harness, the session and that session's newest prompt id), and `commit.rewritten` for each
  old -> new SHA pair after an amend or a rebase. A pick a rebase replays is recorded too, and
  its `commit.rewritten` rebase line marks it as a replay. A Codex commit's prompt id is the
  turn it was made in (Codex has no prompt id; its turn id stands for one).
- **Skills:** `skill.loaded` each time Claude Code or Amp loads a skill, `trigger` `agent` (the model
  called the Skill tool) or `slash` (a person typed `/name`), with its scope, the sha256 of its
  SKILL.md as read from disk right after the load (`hash_method` `disk-after-load`) and of the
  whole folder (`tree_sha256`, the shasum listing of every file, sorted). Never the skill's text,
  the words typed after `/name`, a path, or a folder or plugin name: a folder-scoped skill's
  folder goes out as `scope_dir_hash`, and a plugin's name as its sha256 unless it is public.
  A Skill tool load Livy cannot hash (a built-in skill, a plugin's files) is recorded without a
  hash, and a `ledger.error` says why. A typed `/name` is recorded only when a SKILL.md for it is
  on disk: otherwise it is a built-in command or ordinary text. Amp names the folder it loaded a
  skill from, so its loads are hashed from that folder, built-in ones too (`scope` `builtin`).
- **Never records** prompt or reply text, tool inputs or outputs, shell commands, the
  working folder, transcript paths, or a PR number read from `gh pr create` output.
  (Prompt text is kept only in the local vault below, never in the ledger, and never sent from it.)
  `work.pr_opened` is written only when a Bash command, read as the shell reads it,
  actually runs `gh pr create`.
- **Only the orgs you name.** A repo is in scope only when its `origin` is on
  a configured host (`github.com` unless `scope.hosts` says otherwise; SSH aliases resolved)
  under an org you configured. No org is built in:
  with none configured nothing is recorded. An out-of-scope repo leaves no trace in the ledger.
  OTEL is not scoped this way: see [OTEL export](#otel-export-on-by-default) and `otel.ignore`.

## The guarantees

- **Never blocks.** Every hook path exits 0, within Claude Code's hook timeout.
- **Never fails silently.** When an event cannot be written, or its input is malformed,
  Livy writes a `ledger.error` line with the reason instead.
- **No network.** The hook appends to local files and nothing else. A line that cannot
  take the ledger's lock in time goes to its own file under `spill/` in the data folder
  rather than being written unlocked.
- **Additive-only schema.** `schema/ledger-event.schema.json` never removes or retypes a
  field; CI checks every change against `main`.

## Install

There are no release binaries yet: build from source. That needs **Go 1.27 or later** (the
`go` line in `go.mod`; check with `go version`) and git.

```sh
git clone https://github.com/0xbew/livy && cd livy
go build -o livy ./cmd/livy
cat > profile.json <<EOF2
{"scope":    {"orgs": ["your-org"]},
 "identity": {"handle": "your-github-handle"},
 "sink":     {"type": "local", "path": "$HOME/livy-data"}}
EOF2
./livy install --config profile.json --repo ~/src/your-repo   # --dry-run first if you like
~/.livy/bin/livy doctor                                        # exit 0 = capture is alive
```

`sink.path` must be absolute: Livy does not expand `~`, and install (not its dry run) fails
on a path it cannot use. The example's unquoted `EOF2` lets the shell write your home folder in.

`livy install` copies the binary to `~/.livy/bin/livy`, merges the profile into
`~/.livy/config.json`, adds Livy's hooks to `~/.claude/settings.json` (every other hook and
setting is left as it was), adds them to Codex's `~/.codex/hooks.json` (`$CODEX_HOME`) when
Codex is on the machine (Codex runs a hook only once you trust it: run `/hooks` in Codex after
installing), writes `post-checkout`, `post-commit` and `post-rewrite` hooks into
each in-scope repo (named with `--repo`, or found from the folders Claude Code has worked in), and schedules a daily
flush (launchd on macOS, a systemd user timer or a crontab line on Linux). Running it again
changes nothing. `livy uninstall` removes exactly that and keeps the captured data
(`--purge` deletes it). `--otel <endpoint>` also turns on Claude Code's and Codex's own OTEL
export to that collector, every signal (logs, metrics and traces) for both; `--otel-logs none`,
`--otel-metrics none` and `--otel-traces none` turn a signal off for both, and prompt, reply
and tool text are never turned on; `--otel-header-env NAME` makes Claude Code send an Authorization
header read from `$NAME` at export time, so no secret is written to its settings. Codex reads
export headers only literally, so for Codex the value is written into `~/.codex/config.toml`,
which install makes mode 0600 (doctor shows it, and which signals are on).

When `~/.config/amp` exists, install also writes an Amp plugin, `~/.config/amp/plugins/livy.ts`
(`--no-amp` skips it, and removes one an earlier install wrote). Its handlers return at once and
hand each event to `livy amp`, run in the thread's folder (Amp's workspace root, else the folder
the thread's first shell command ran in; a thread's events wait until one is known, and are
counted in a `ledger.error` if none ever is), which writes `session.started`, `prompt.submitted`, `tool.call`,
`skill.loaded`, `work.pr_opened`, `turn.ended` and `usage.api_call` lines with `harness: amp`. Amp reports no
loaded instruction files, model switch or session end, so none is written, and every Amp
`session.started` lists them in `not_observable`. A `livy.ts` without Livy's marker line is left
alone. Restart Amp after installing. In an orb (`AMP_ORB=1`), the plugin's last upload runs 3
minutes after a turn, unthrottled, before the orb pauses; `deploy/amp-orbs/` records orb threads.

## Where the data goes

`livy emit` rotates the ledger into segments and ships each as
`source=agent-hooks/dt=<UTC day>/handle=<handle>/<sha256[:16] of its bytes>.jsonl.gz`,
create-only: a retry targets the same key, "already there" counts as shipped, and a
segment leaves `segments/` only once the sink confirms it. A shipped ledger segment is then
kept under `uploaded/` in the data folder for `emit.keep_uploaded_days` (30 by default, counted
from the upload) so `livy why` can still read it, and pruned after that. Emit runs at session end,
at most every 5 minutes mid-session, and daily. The otel stream's shipped segments are kept the
same way, under `otel/uploaded/`, so `livy profile` can still read their spans. A refused upload keeps everything, writes one
`ledger.error`, and does not grow the backlog. The sink is one setting.

**When no sink is set, install sets a local one:** `{"type": "local", "path": "<home>/livy-data"}` in
`~/.livy/config.json`, with the folder made owner-only (0700), so segments and OTEL ship there
instead of piling up in the data folder (where the OTEL stream would fill its cap and refuse
exports). A sink set anywhere (the managed file, `LIVY_SINK_*`, the config, a profile) always
wins, and a re-install keeps the sink already recorded. `livy doctor` shows it as
`sink local · ~/livy-data`.

| `sink.type` | Keys | Credentials (never stored by Livy) |
|---|---|---|
| `local` | `path` (absolute folder) | none |
| `gcs` | `bucket`, `prefix`, `auth` (`gcloud` / `adc` / `command`), `writer_sa`, `account`, `token_command`, `endpoint` | a short-lived token per pass: `gcloud auth print-access-token` (impersonating `writer_sa`), Application Default Credentials, or a command that prints a token |
| `s3` | `bucket`, `prefix`, `region`, `profile`, `credential_command`, `endpoint` | the AWS chain: env, `credential_command` (credential_process JSON), the aws CLI (SSO, profiles), container or instance role |
| `git` | `remote` (an https URL with no user or token, or an absolute path), `branch` (default `main`) | git's own credential helper; each object is one commit, pushed |

`endpoint` points either at an emulator or an S3-compatible server (MinIO, R2, RustFS).

## Settings

Highest first: an admin's managed file, then `LIVY_*` environment variables, then
`~/.livy/config.json`, then built-in defaults. A key the managed file sets cannot be
changed by a lower layer, and a managed `sink` key locks the whole `sink` section.

| Setting | File key | Environment |
|---|---|---|
| Orgs in scope | `scope.orgs` (list) | `LIVY_SCOPE_ORGS` (comma-separated) |
| Hosts in scope | `scope.hosts` (list; default `github.com`) | `LIVY_SCOPE_HOSTS` |
| Handle | `identity.handle` | `LIVY_IDENTITY_HANDLE` |
| Sink | `sink.*` (above) | `LIVY_SINK_TYPE`, `LIVY_SINK_BUCKET` |
| Ticket key pattern | `ticket.regex` (Go RE2; default `(?:^\|[^A-Za-z0-9])([A-Z][A-Z0-9]+-\d+)`, case-sensitive) | `LIVY_TICKET_REGEX` |
| Days a shipped segment is kept locally | `emit.keep_uploaded_days` (default 30; 0 keeps none) | |
| Projects whose OTEL the receiver drops | `otel.ignore` (list; default none; see [OTEL export](#otel-export-on-by-default)) | |
| Data folder | | `LIVY_DIR` (default `~/.livy`) |

The managed file is `/etc/livy/managed.json` on Linux, `/Library/Application Support/Livy/managed.json`
on macOS and `%ProgramData%\Livy\managed.json` on Windows. A file that repeats a key is refused
as a whole.

## Prompt capture and upload (optional, off by default)

Livy records one text-free `prompt.captured` line for every new human prompt in an in-scope
repo, from Claude Code transcripts and Codex rollouts: its id, session, repo, branch, whether
it was a relay of another session's agent output, and a `text_status`. No text.

Prompt text leaves the machine only if the engineer turns it on, on that machine:

```sh
livy prompts --upload on        # off again with --upload off
livy prompts --backfill-days 30 # with upload on: send earlier prompts once, at most 120 days back (as far as the prompt id store remembers)
livy prompts --dry-run          # counts only
```

- Every field is scrubbed with [`github.com/0xbew/scrub`](https://github.com/0xbew/scrub) and the local
  denylist before it is cut, then audited when written to the local queue and again, with the
  current denylist, just before it leaves. A record the audit refuses is never sent: its event says
  `refused_audit`, a `ledger.error` names the prompt id and rule (never the text), and the record
  is held in `prompt-rejected/` while the rest of its segment ships. No denylist, no text.
- Text from a session outside your orgs is withheld: a context field that came from one is replaced,
  and a prompt that is a relay of one is not uploaded (`outside_relay`).
- Upload goes through its own sink, `prompts.sink` (same keys as `sink`, no default). It is refused
  if it names the same bucket as `sink` (whatever the prefix) or a folder that contains, or is inside, the ledger's. A managed
  `sink` also locks `prompts.sink`: set it in the managed file too. Nothing is sent without it; the text stays queued.
- Context from another project's session is withheld, judged on the source's own words as well as on what would leave; a
  prompt that cannot be shown to be in scope is not sent.

```json
{"prompts": {"sink": {"type": "gcs", "bucket": "my-prompts-bucket"}}}
```

- **Retention is 180 days.** Set it on the bucket, on the `source=prompt-text/` prefix (so keep
  `prompts.sink.prefix` empty). `deploy/otel/gcp.sh` and `deploy/otel/aws.sh` create the bucket and
  set that rule when `LIVY_PROMPTS_BUCKET` is given.
- `livy doctor` shows the opt-in, the queue, held records and the last prompt upload.

## Prompt text kept on this machine (`livy show`)

Transcripts are cleaned up by the harness, so Livy keeps each new prompt's text in a local vault,
`prompt-vault/` in the data folder: one owner-only (0600) file per UTC day, the folder 0700, kept
**90 days** from when the prompt was sent. A day's file is deleted whole once its last possible
prompt is 90 days old, so a prompt sent early in a day can stay up to a day longer. It fills whether or not upload is on,
from the first pass after install onwards, and nothing reads it to send anywhere. To copy what the
transcripts on disk still hold from before that, run once:

```sh
livy vault backfill            # the last 90 days (--days N for fewer)
```

It reads every transcript from its start, copies in-scope prompts sent inside the window, skips any
prompt already kept (a second run copies nothing new), and prints its counts. It exits 1, with a
`ledger.error`, when anything was not copied: a transcript held at a user message Livy cannot read
(the file stops there until Livy reads it), a transcript or line it could not read, a git
timeout scoping a folder, or a prompt with no session, time or text; run it again after the
cause is fixed. A record with no id, time or text is never written, and a transcript user message of a
shape Livy does not recognise stops that file's prompt capture before it, with one `ledger.error`.

```sh
livy show --prompt <id>     # prompt capture's id (prompt:...) or Claude Code's own prompt id
livy show --commit <sha>    # every prompt `livy why` finds behind that commit
```

`livy show` prints the text, repo and branch exactly as they would be shared: scrubbed whole with
[`github.com/0xbew/scrub`](https://github.com/0xbew/scrub) and the local denylist, then audited; a
text the audit still refuses is not shown. It refuses to run without a usable denylist. Each reveal
first writes one `prompt.revealed` ledger line per repo with the prompt ids shown (never the text);
if that line cannot be written, nothing is shown. When the vault, or a folder `livy why` reads for
`--commit`, exists but cannot be read, `livy show` names it, shows what it could read, and exits 1.
`uninstall --purge` deletes the vault.


## Which agent work is behind a commit (`livy why`)

```sh
livy why <sha>          # one commit
livy why A..B           # every commit in a range, e.g. between two deploys
livy why --data DIR A..B  # also read ledger files copied from the bucket
livy why --per-prompt <sha>  # also one line per prompt
livy why --json A..B    # one JSON document instead of text (below)
```

For each commit it prints the agent sessions behind it (from its `commit.recorded` line and its
`Agent-Session` trailers) and, per session, the work since the session's previous commit on the
same branch: the prompt ids, the tools run, the calls and tokens per model, and the ticket key.
A prompt is one a person typed. Claude Code's prompt hook also fires for a subagent's hand-back, a
task notification and a scheduled or meta prompt, so a `prompt.submitted` line counts only when
transcript extraction wrote a prompt `interaction` line within 5 s of it, or has not read that far
into the session yet (nor ever, for a harness it does not read). A prompt queued while a turn was
running can land further from its transcript record than that, and is then not counted.
The opening prompt of a session another session launched (its `session.started` line names a
parent session, from the launcher's `LIVY_PARENT_SESSION`) was written by that launcher's agent,
not a person: it is listed apart, as `agent briefs` (`agent_brief_ids` in `--json`), and is not one
of the window's prompts.
The first commit a session makes on a branch takes the work since the branch was last checked out (its newest
`work.started` line) or since the session's previous commit by trailers, whichever is later; with
neither, since the session's previous commit on any branch; only with none of these, the whole session. A commit a
rebase replayed is read as the commit it replays, and never ends another commit's window.
Each window line names its start in a fixed word, the same as `--json`'s `window_basis_kind`:
`recorded` (the session's previous commit on the branch), `trailer` (its previous commit by
`Agent-Session` trailers), `checkout` (the branch's last checkout), `any_branch` (the session's
previous commit on any branch) or `none` (the session's start), then the detail. `livy profile`
prints the same word when a turn was already running at the window's start.
With `--per-prompt` it adds one line per prompt in each window: the tools it ran, how they ended
(`ran`, `blocked:<category>` such as `classifier_denied` or `user_rejected`, `not_run:<category>`,
`tool_error`; or `ok`/`error` where `tool.call` lines exist), its tool decisions by decision and
source (`accept:config`, `accept:user_temporary`, `reject:user_reject`; from the OTEL receiver), the
permission mode it was sent in, and calls and tokens per model. Every line between a prompt and the session's next one is that prompt's;
an API call goes to the prompt its `call.detail` names, and lines before a window's first prompt
(work a prompt from before the previous commit was still doing) go to `(a prompt before this window)`. These come from transcript extraction, so a
prompt shows them once `livy extract` has read it. It prints ids, names and counts only, never prompt text. It reads what is still on this
machine: the ledger, unshipped segments, the uploaded segments emit keeps (30 days by default),
and a `local` sink's folder. Older segments are not read; pass `--data` with a downloaded copy.
A folder it should read but cannot list (a `--data` folder, the sink's, or a spill or segments
folder that exists but is unreadable), or a ledger that exists but cannot be opened, is named under the report, and `livy why` exits 1. A file or
folder below one that cannot be read is counted in the report's "unreadable" tally.

## Where a commit's time went (`livy profile`)

```sh
livy profile <sha>              # one commit
livy profile A..B               # every commit in a range, then totals
livy profile --top 10 --all <sha>  # ten longest spans; every span in the tree
livy profile --json A..B        # one JSON document instead of text (below)
```

For each session `livy why` finds behind a commit (a squash merge is traced to its pull request's
commits the same way), it splits the wall time, from the first of the prompts or agent briefs `livy why` lists (or the previous commit,
when a turn was still running) to the commit, into agent time (model calls and tools), time
waiting on the human (between turns, or for a tool approval) and the rest. It prints the span
tree, each turn with its model calls, tools and subagents and their durations, the longest of
them, and the rework it can see: a `fixup!`/`squash!`/`amend!` commit, a commit rewritten by
amend or rebase, and files the commit changes that earlier commits of its session changed. It
reads the OTEL spans Livy's receiver keeps (on by default, Claude Code and Codex) and the ledger,
on this machine only, and prints names, durations and counts, never prompt or tool text. A window
with no turn span (OTEL off, or the receiver down) gets its wall time only.

## Machine-readable output (`--json`)

`livy why --json` and `livy profile --json` write one JSON document to stdout instead of text, for
tools that would otherwise parse the text. Its `schema` field names its shape: `livy.why/1` and
`livy.profile/1`. Fields are only ever added; a change that removes or retypes one gets a new
schema value. Times are RFC 3339 UTC (a span's may carry fractional seconds); a window that runs
from the session's start has `window_from: null`; durations are whole milliseconds (`*_ms`).
Every field is always present: lists and count maps are `[]` and `{}` when empty, strings `""`.
Sentences for people (`window_basis`, `label`, `summary`, `problem`) sit beside fixed words and
counts for code (`window_basis_kind`, `kind`, `read.events`); read the latter. The exit status is
the text report's: 1 when `read.unread` names something that could not be read.

- `livy.why/1`: `commits[]` (`sha`, `committed`, `trailers` (`agent_sessions`, `agent_harness`,
  `agent_model`), `sessions[]` with `session_id`, `harness`, `ticket_key`, `window_from`,
  `window_to`, `window_basis_kind` (`recorded`, `trailer`, `checkout`, `any_branch`, `none` or
  `other`) and `window_basis_sha` (the commit `recorded`, `trailer` and `any_branch` name; `""`
  for the others), every `prompt_ids`, `tools`, `models` (calls and tokens per model) and
  `per_prompt[]`; `squash` (null unless the commit's pull request was looked up): its `pr` and the same
  for each of its `commits`), and `read` (`events`, `files` by source, `oldest_event`,
  `duplicate_lines`, `unreadable_files_or_lines`, `unread`, and `summary`, the text's last lines).
- `livy.profile/1`: `commits[]` (`sha`, `committed`, `rework`, `sessions[]` with the window and its `window_basis_kind` and `window_basis_sha`,
  `start`, `wall_ms`, `timed`, `agent_ms`, `model_ms`, `waiting_on_human_ms`, `other_ms`,
  `turns`, span counts, the `longest` spans (`kind`: `model`, `tool` or `subagent`; `name`;
  `agent`: `main` or the subagent's id, `agent_type` when the ledger names it; `starts_agent`, the
  subagent a `subagent` span ran; `outcome`: `ok`, `failed`, `rejected` or `""` when the spans do not
  say), `turn_times[]` (`start`, `end`) and `waits[]` (`kind`: `between_turns` or `approval`;
  `start`, `end`), which never overlap and whose `end - start` sum to `waiting_on_human_ms` (each
  `duration_ms` is rounded down on its own); `squash` as above),
  `totals`, and `read` (`spans`, `span_files`, `unread`, and `summary`, the ledger lines read).
  Each session also counts the window's `prompts` and `agent_briefs`, the lengths of `livy.why/1`'s
  `prompt_ids` and `agent_brief_ids`.
  `--top N` sets how many spans `longest` lists. The span tree is in the text report only.

## An evidence pack for an incident (`livy incident`)

```sh
livy incident INC-1 --range A..B              # Markdown on stdout
livy incident INC-1 --range A..B --out INC-1.md --data DIR
livy incident INC-1 --range A..B --excerpts   # plus scrubbed prompt text, through livy show
```

`A..B` is the deploy range (the commits after the last good deploy, up to the bad one). The pack
is `livy why` over every commit in it, as one file to hand an incident review: totals at a glance
(commits, how many have an agent session, sessions, distinct prompts, tickets), one row per
commit, each session's window, prompt ids, tools, models and tokens, the commits no agent session
was found for, and what was read. Text-free: ids, names and counts only (a trailer or ledger value
that is not an id or a name prints as `(not a name)`); `--out` writes a new 0600 file and never
overwrites one. The "what was read" line names any `--data` folder as you gave it.

`--excerpts` adds each prompt's text, only by `livy show`'s rules: the kept text from this machine's
vault, scrubbed with the denylist and audited (a prompt the audit refuses is listed, not shown), one
`prompt.revealed` ledger line per commit and repo written before any text, and no pack at all
without a denylist or when that line cannot be written. Excerpts sit in fenced blocks no prompt can
close. Text not in this machine's vault (another author's) is listed as missing; ask its author.

## OTEL export (on by default)

Claude Code and Codex can export their own telemetry: logs, metrics and traces, with
tokens, cost and timing per API call. Livy sends it to one of three places
(`otel.destination` in the config is `sink`, `collector` or `none`). **`livy install` turns Livy's
own receiver on by default**, so a machine records OTEL from its first install; `livy install
--otel-to none` turns it off and records `otel.destination: none`, which later installs keep.
With `--no-schedule`, or where there is no launch agent or systemd user service to keep it
running, it stays off and install says so.

- **Livy's own receiver** (the default; `livy install --otel-to sink`). The OTEL stays on this
  machine unless the sink you configured is remote: it leaves only through that sink. A background
  service on `127.0.0.1:14319` takes both clients' exports (OTLP/JSON, with a key from the
  data folder), removes identity and content by the same rules as the collector below, and
  writes them to disk. `livy emit` ships them through the sink you configured, under
  `source=otel/dt=<day>/handle=<handle>/`. An export is accepted once it is on disk, so a
  network outage loses nothing. **An export made while the receiver is down is lost**,
  because neither client queues one across a refused connection. The service restarts
  itself, and `livy doctor` fails while it is not answering.
  The receiver also keeps each tool decision (OTEL's `tool_decision`: accepted or rejected, and by
  config, a hook or the user) as one text-free line under `tool-decisions/` in the data folder,
  kept 90 days and never shipped, so `livy why --per-prompt` can count what each prompt had
  approved, not only what was refused.
- **What the receiver keeps:** every session's export, in any folder or repository, a personal
  project's included: token counts, cost, model names, tool names and decisions, timings and
  session ids, with identity and prompt, reply and tool text removed. To leave projects out,
  list them in `otel.ignore`, in `~/.livy/config.json` or the managed file (a managed list
  replaces the user's): a folder (`"~/personal"`, `"/abs/path"`, a glob allowed in any
  segment), which covers it and everything under it, or a repository as `"owner/repo"`
  (`"me/*"`), matched against the origin of the repository the session works in, on any host.
  Anything else, or a malformed glob, matches nothing (doctor says so), though it still counts as a list.
  The receiver drops a listed project's records before anything is written: nothing of them is
  stored or shipped, and no ledger line names them. It places a Claude Code session by the
  folder Claude Code runs its header helper in (`livy otel-headers`, sent with each export and
  never stored), and a Codex record by the folder its conversation's rollout records. While the
  list names anything, a record whose project cannot be told is dropped too: Codex's metrics
  and most of its spans name no conversation, so they are dropped for every project, and a
  Claude Code session started before this version loses its exports until Claude Code runs the
  helper again (about every 29 minutes) or restarts. The list is read on each export, so a
  change needs no restart. `livy doctor` shows it and counts the exports it cut.
- **An OTLP endpoint you already have** (your own collector, or a vendor): `livy install
  --otel-to <endpoint> --otel-header-env NAME` (`--otel <endpoint>` is the same). Nothing is
  deployed. `otel.ignore` does not apply: Claude Code exports straight to the endpoint and Livy
  never sees it (doctor warns when the list names anything).
- **A fresh collector in your own cloud**: `deploy/otel/gcp.sh up` or
  `deploy/otel/aws.sh up` runs the upstream OpenTelemetry Collector. It stores every signal
  in the bucket Livy's uploads use, with identity and free text removed first. See
  `deploy/otel/README.md`. `otel.ignore` does not apply here either: only Livy's own receiver
  applies it (doctor warns when the list names anything).

## Spot-checks (optional)

Spot-checks are optional setup, off until you add a key. They are log-only: a `PreToolUse`
hook (`livy spotcheck`, async, so Claude Code never waits for it) asks
[Jev](https://docs.typesafe.ai), TypeSafe's typed-decision model, a fixed set of questions about
two kinds of tool call, and writes the answer as a `check.decision` receipt. Nothing the agent
does changes.

- **`guard`**, every shell command (`guard@1`): how reversible is it, and does it delete,
  rewrite history, reach outside this computer, touch production or touch secrets? The receipt
  says `allow` or `ask` under a frozen policy, with every score.
- **`route`**, every subagent task (`route@1`): which model tier is enough? The receipt names
  the tier when Jev is confident (0.8 or more), otherwise `not_confident`.

To turn it on, get a key from TypeSafe and put it in `JEV_API_KEY` (or `TYPESAFE_API_KEY`) in
the environment Claude Code runs in, or in `jev.key` in the data folder (mode `600`). Remove
the key to turn it off.

What leaves the machine: the command or brief, scrubbed by
[`github.com/0xbew/scrub`](https://github.com/0xbew/scrub) (secrets, addresses, phone numbers
and your denylist's names), sent to `api.typesafe.ai` and nowhere else. The exact request body
is audited before it is sent, and a refusal sends nothing: the receipt is `skipped` with the
rule that fired. A subagent brief is also not sent if it repeats one of your own prompts. The
ledger keeps verdicts, scores and a hash of what was sent, never the text. A check that cannot
be made is a receipt with verdict `error` and the reason, and checks then pause on this
machine for an hour after a rejected key and five minutes after anything else.

The Jev model is pinned (`jev-1.13.0`), and the two question sets are frozen: a change is a new
contract id.

## Denylist

The denylist names the people (and vendors) whose names are scrubbed from any text Livy sends.
Livy keeps a local copy at `denylist.json` in the data folder (`denylist.path` moves it), mode
`600`. No source is built in: name yours under `denylist.source` in the config, and Livy
refreshes the copy from it.

| `denylist.source.type` | Keys | Holds |
|---|---|---|
| `file` | `path` (absolute) | the copy's own shape: `{"identities": [...], "vendors": [...], "version": N}` |
| `gcs` | `bucket`, `object`, plus the gcs sink's `auth`, `writer_sa`, `account`, `token_command`, `endpoint`, `gcloud` | the same shape |
| `s3` | `bucket`, `object`, plus the s3 sink's `region`, `profile`, `credential_command`, `endpoint` | the same shape |
| `secretmanager` | `secret`, `project`, plus the gcs sink's credential keys | the payload without `version`; the secret version's number becomes the version |

```json
{"denylist": {"source": {"type": "secretmanager", "secret": "my-denylist", "project": "my-project",
                         "auth": "gcloud", "writer_sa": "writer@my-project.iam.gserviceaccount.com"}}}
```

Credentials work as they do for the sinks and are never stored. With `auth: gcloud` the secret
is read with one `gcloud secrets versions access latest` call, so the identity needs only
`secretAccessor`. `livy install` refreshes the copy, `livy install --refresh-denylist` refreshes
only the copy, and the daily flush refreshes it once a day. A hook never fetches it. A fetch that
fails, finds no version, or gets something that is not a whole valid denylist keeps the copy as
it was and says why. The copy is replaced atomically, and only after it has been read back the
way every sender reads it. `livy doctor`'s `denylist` row shows the copy's version and age,
and warns when the last refresh failed or the copy has not been confirmed for 3 days. A copy kept
by hand (no source) or copied from a local file is never called stale.

Without a copy, names are not scrubbed: spot-checks still remove secrets, addresses and phone
numbers, but send names as written. A copy that cannot be used stops spot-checks sending
anything until it is replaced. The list's text never appears in output, the ledger or the refresh
record (`denylist-refresh.json`), only its version and why a refresh failed.

## Build and test

```sh
go vet ./...
go test ./...
go build -o bin/livy ./cmd/livy
```

The module's one dependency is [`github.com/0xbew/scrub`](https://github.com/0xbew/scrub) (public).


## Security

Report a vulnerability privately; see `SECURITY.md`.

## License

Apache-2.0. See `LICENSE` and `NOTICE`.
