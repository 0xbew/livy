# Recording Amp orb threads

An Amp orb is a cloud machine that runs a thread's tools, and Amp loads plugins there, so the
same `livy.ts` that records a laptop's Amp threads can record an orb's. What an orb lacks is the
`livy` binary, a config, and a way to ship the record. There are two ways to give it those. Both
write the same lines, with the same scope rule and the same plugin, as `livy install` on a laptop.

| | Path A: self-hosted orb | Path B: hosted orb |
|---|---|---|
| Where the orb runs | your Docker host, via `amp orb provider` | Amp's cloud |
| How Livy gets in | baked into the orb image (`Dockerfile`) | a personal Amp plugin carrying the binary (`orb-plugin.ts`) |
| How the record leaves | `dispatch.sh` copies it out on pause and delete | the orb pushes it to a private sink repo (`sink.type: git`) |
| Credentials in the orb | none | the git credentials Amp already gives the orb |
| When it ships | every pause and delete | after the first turn, at most every 5 minutes, and 3 minutes after the last turn |
| Records the git hooks (`work.started`) | yes (`livy install` runs in the image) | no |

## Path A: a self-hosted orb image

```sh
deploy/amp-orbs/build.sh image profile.json livy-orb:1 [base image]
```

`profile.json` is `livy install --config` input with a local sink at `/home/user/.livy-outbox`
(see `profile.example.json`). Run the provider with this folder's `dispatch.sh`, pointing
`LIVY_ORB_COLLECT` at the folder this host ships from:

```sh
ORB_IMAGE=livy-orb:1 LIVY_ORB_COLLECT=/abs/folder AMP_ORB_PROVIDER_TOKEN=… amp orb provider /abs/path/dispatch.sh
```

On pause and delete, `dispatch.sh` runs `livy emit` in the orb, copies the outbox here
write-once and removes from the orb exactly what arrived. The collected folder has the same
layout as a bucket (`source=agent-hooks/dt=…/handle=…/<hash>.jsonl.gz`). If anything stays
behind on delete, the orb is kept stopped and the job fails, rather than destroying it. The
agent controls the orb, so it could plant objects in its outbox: the collected record is only as
trustworthy as the orb. On an ARM64 host, pass
Amp's ARM64 base image and set `LIVY_ORB_PLATFORM=linux/arm64`.

## Path B: a personal plugin for hosted orbs

```sh
deploy/amp-orbs/build.sh bundle config.json out/livy
```

`config.json` becomes `~/.livy/config.json` in each orb: `scope.orgs`, `identity.handle`, and
`{"sink": {"type": "git", "remote": "https://github.com/<owner>/<private repo>.git"}}`. Commit
`out/livy/` to your personal plugin repository as the plugin `livy`. Nothing checks that the
sink repository is private: a public one publishes the ledger. On a laptop the system
plugin of the same name, written by `livy install`, takes precedence, and the bundle does nothing
outside an orb either way.

