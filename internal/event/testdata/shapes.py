"""Sanitised shapes of real ledger lines: the first line per event that adds a key not seen yet.

    /usr/bin/python3 shapes.py SCHEMA LEDGER.jsonl... > python_lines.jsonl

Keeps keys, enum values, ts, numbers and booleans; replaces every other string with "x",
renames score keys, and re-encodes with the Python ledger's own json.dumps call, so each
line is Python's bytes. The six lines after the real shapes were added by hand, encoded
the same way, for shapes the real sample lacks.
"""
import json, sys
schema = json.load(open(sys.argv[1]))
props = schema["properties"]
def clean(key, v, spec):
    if isinstance(v, str):
        if key in ("ts", "event") or "enum" in (spec or {}):
            return v
        return "x"
    if isinstance(v, list):
        it = (spec or {}).get("items", {})
        return [clean(key, e, it) for e in v]
    if isinstance(v, dict):
        sub = (spec or {}).get("properties")
        if sub is None:  # scores: question ids -> numbers
            return {f"q{i}": clean(key, x, {}) for i, (_, x) in enumerate(sorted(v.items()))}
        return {k: clean(k, x, sub.get(k)) for k, x in v.items()}
    return v
seen, out = {}, []
for path in sys.argv[2:]:
    for line in open(path):
        rec = json.loads(line)
        ev = rec.get("event")
        new = set(rec) - seen.setdefault(ev, set())
        if not new:
            continue
        seen[ev] |= set(rec)
        out.append(json.dumps({k: clean(k, v, props.get(k)) for k, v in rec.items()}, separators=(",", ":"), sort_keys=True))
sys.stdout.write("\n".join(out) + "\n")
