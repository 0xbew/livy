# Security policy

Livy runs inside every agent session on an engineer's machine, so a flaw in it can leak
what it was built never to record. Please report one privately.

## Reporting

Use GitHub's private vulnerability reporting: the **Security** tab of this repository,
then **Report a vulnerability**. Do not open a public issue for a security problem.

We aim to acknowledge a report within 7 days. A confirmed issue is fixed on `main`, and
you are credited unless you ask not to be.

## In scope

- Text Livy promised never to record reaching the ledger: prompt or reply text, tool
  inputs or outputs, shell commands, working folders, transcript paths.
- A repo outside the configured orgs leaving any trace in the ledger.
- A hook that blocks the agent session or exits non-zero.
- Anything that makes Livy write, execute or delete outside its own data folder, or
  lets hook input inject a command.

## Out of scope

- What other tools on the same machine record.
- Findings that need an attacker who can already write to your home folder or your
  Claude Code settings.
