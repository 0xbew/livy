"""Expected values for the hook's pure decisions, from the Python ledger (the oracle).

    LIVY_PY_REPO=/path/to/reference /usr/bin/python3 internal/hook/testdata/gen.py

Writes python_decisions.jsonl: one row per input, {"fn", "in", "out"}. Inputs are
synthetic: the commands, URLs and payload shapes below, never recorded text.
"""

import json
import os
import sys

sys.path.insert(0, os.path.join(os.environ["LIVY_PY_REPO"], "telemetry"))
import git_scope  # noqa: E402
import ledger  # noqa: E402
import tool_target  # noqa: E402

PR = [
    "gh pr create --title t --body b", "timeout 115 gh pr create --fill", "GH_TOKEN=x gh pr create --fill",
    "gh --repo acme-org/sample pr create --fill", "git push -u origin feature/x && gh pr create --fill",
    "(cd sub && gh pr create --fill)", "echo 'gh pr create'", "cat > s.json <<'EOF'\n\"Bash(gh pr create *)\"\nEOF",
    "git status --short", "gh pr view 12", "/opt/homebrew/bin/gh pr create --draft", "sudo -n gh pr create",
    "gh pr create", "gh pr created", "ghx pr create", "gh  pr\tcreate", "gh pr create\n", "nohup gh pr create &",
    "env -u X FOO=1 gh pr create", "timeout -s KILL 5 gh pr create", "timeout --signal=KILL 5 gh pr create",
    "if true; then gh pr create; fi", "for x in a; do gh pr create; done", "! gh pr create", "{ gh pr create; }",
    "echo $(gh pr create)", "echo `gh pr create`", "x=$(gh pr create)", "gh -R pr create", "gh --repo pr create",
    "gh --repo=pr create", "gh -R pr/x pr create", "gh -R prx pr create", "gh -R p pr create", "gh -R pz pr create",
    "gh -R pr-x pr create", "gh --a -b pr create", "gh --a=1 --b 2 pr create", "gh -- pr create",
    "cat <<EOF\ngh pr create\nEOF\ngh pr create", "cat <<-EOF\ngh pr create\n\tEOF", "cat <<<'gh pr create'",
    "cat <<'A'<<B\nx\nA\ngh pr create\nB\ngh pr view", "a|gh pr create", "a;(gh pr create)", "a ;(gh pr create)",
    "a gh pr create", "gh pr create", "command gh pr create", "exec time gh pr create",
    "else gh pr create", "elif gh pr create", "do gh pr create", "A=1 B=2 gh pr create", "A= gh pr create",
    "sudo env A=1 timeout 3 gh pr create", "gh pr create || true", "echo gh pr create", "\tgh pr create",
    "gh pr create --fill", "gh -R é pr create", "gh -R pré pr create",
    # Quoted text (go_overrides.json holds Go's answer where Python's is a bug).
    'git commit -m "fix; gh pr create later"', "echo 'a && gh pr create'", 'echo "x | gh pr create"',
    "printf '%s\\n' 'y; gh pr create'", 'git commit -m "$(gh pr create)"', 'echo "ok" && gh pr create',
    'git commit -m "wip; gh pr create --fill"', "echo 'x && gh pr create --draft'",
]
# Shell forms gh pr create detection must read as the shell does: a substitution inside
# quotes, line continuations, comments (python_shell_forms.jsonl).
SHELL_FORMS = [
    'git commit -m "$(date)"; gh pr create', "git commit -m \"$(cat <<'EOF'\nmsg\nEOF\n)\" && gh pr create --fill",
    'echo "run `date` now"; gh pr create', 'echo "today $(date) is; gh pr create"',
    "cd repo && \\\n  gh pr create --fill", "gh pr \\\n create", "# don't forget\ngh pr create --fill",
    "a && \\\nb && \\\ngh pr create", "echo '$(gh pr create)'", 'echo "today $(date) is; gh pr create now"',
]
URLS = [
    "git@github.com:acme-org/sample.git", "https://github.com/acme-org/sample-https.git/",
    "git@github.com:AcmeOrg/Upper.git", "git@gitlab.com:acme-org/elsewhere.git", "git@github-work:acme-org/a.git",
    "ssh://git@github.com:22/acme-org/x.git", "https://user:tok@GitHub.com/o/r", "https://github.com/o/r/extra",
    "https://github.com/o", "github.com/o/r", "/local/path/repo", "", "  git@github.com:o/r.git  ",
    "file:///tmp/o/r", "git@github.com:o//r", "https://github.com//o/r.git", "git@:o/r", "a:b/c",
]
DIRS = [
    "cd /tmp/wt && gh pr create", "cd wt; git status", "git -C /x/y status", "git -C a -C b status",
    "git --no-pager -C /x log", "git -c a=b -C /x log", "git --git-dir=/x log", "cd", "cd -", "cd ~/w",
    "cd \"$HOME\"/w", "S=/tmp/x; cd $S", "S=/tmp/x cd $S", "export S=/tmp/y; cd ${S}/z", "HOME=/x cd ~",
    "cd 'unclosed", "cd $(pwd)", "git -C", "cd -P /a", "ls; cd /a", "\ncd /a", "; cd /a", "echo hi",
    "S=~/x; cd $S", "S='~/x'; cd $S", "A=1 && cd /q", "A=1 || cd /q", "cd /a\\ b", "cd \"/a b\"",
    "git -C \"$(pwd)\" status", "export S; cd /a", "export A=1 B=$A; cd $B", "A=1 B=$A; cd $B", "( cd /p )",
    "cd /a # comment", "git -C '' status", "git -C /a -C '' x",
]
TOOLS = [
    ["Bash", {"command": "cd /tmp/wt && gh pr create"}, "/s"], ["Bash", {"command": "cd rel"}, "/s"],
    ["Bash", {"command": "cd rel"}, "rel"], ["Read", {"file_path": "/a/b/c.txt"}, "/s"],
    ["Edit", {"file_path": "rel/c.txt"}, "/s"], ["NotebookEdit", {"notebook_path": "/n/b.ipynb"}, "/s"],
    ["Grep", {"path": "/g"}, "/s"], ["mcp__x", {"path": "/g"}, "/s"], ["Glob", {"path": " "}, "/s"],
    ["Bash", {"command": "cd /a/../b/./c"}, "/s"], ["Bash", "notadict", "/s"], ["Write", {"file_path": "~/x"}, "/s"],
]

out = []
for c in PR:
    out.append({"fn": "runs_gh_pr_create", "in": c, "out": ledger.runs_gh_pr_create(c)})
for u in URLS:
    r = git_scope.remote_owner_repo(u)
    out.append({"fn": "remote_owner_repo", "in": u, "out": list(r) if r else None})
for d in DIRS:
    r = tool_target.leading_directory(d)
    out.append({"fn": "leading_directory", "in": d, "out": list(r) if r else None})
os.environ["HOME"] = "/home/dev"
for tool, inp, cwd in TOOLS:
    r = tool_target.named_directory(tool, inp, cwd)
    out.append({"fn": "named_directory", "in": [tool, inp, cwd], "out": list(r) if r else None})
here = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(here, "python_shell_forms.jsonl"), "w", encoding="utf-8") as fh:
    for c in SHELL_FORMS:
        fh.write(json.dumps({"fn": "runs_gh_pr_create", "in": c, "out": ledger.runs_gh_pr_create(c)}, sort_keys=True) + "\n")
with open(os.path.join(here, "python_decisions.jsonl"), "w", encoding="utf-8") as fh:
    for row in out:
        fh.write(json.dumps(row, sort_keys=True) + "\n")
print(f"{len(out)} rows", file=sys.stderr)
