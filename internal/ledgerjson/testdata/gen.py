"""Writes python_dumps.jsonl: inputs and what Python's ledger encoder makes of them.

Run with the interpreter the Python hooks run on (macOS system python3, 3.9):
    /usr/bin/python3 internal/ledgerjson/testdata/gen.py > internal/ledgerjson/testdata/python_dumps.jsonl
The ledger writes each line with json.dumps(record, separators=(",", ":"), sort_keys=True).
"""
import json
import sys


def dumps(v):
    return json.dumps(v, separators=(",", ":"), sort_keys=True)


values = [
    None, True, False, "", "plain", 'quote" back\\ slash/', "\n\r\t\b\f", "\x00\x01\x1f\x7f",
    "<>&'", "é", "日本語", "\U0001f600", "mixed é\U0001f600\n", "  ",
    "﻿", 0, -1, 2**53 + 1, 2**63 - 1, 2**64 + 7, -(2**70),
    [], {}, [1, "a", None, [True, {}]],
    {"b": 1, "a": 2, "B": 3, "_x": 4, "é": 5, "a1": 6, "a10": 7, "a2": 8, "": 9, "~": 10},
    {"nested": {"z": [{"y": 1, "x": 2}], "a": None}, "scores": {"q2": 0.25, "q1": 1.0}},
]
floats = [
    0.0, -0.0, 1.0, -2.5, 0.1, 100.0, 0.30000000000000004, 1e-05, 0.0001, 0.00012345, 1.5e-07,
    123456789012345.67, 999999999999999.9, 1e15, 1e16, 1.2345e16, 1e22, 1e100, 5e-324,
    1.7976931348623157e308, 2.0 / 3.0, 1234.5, 0.87, float("nan"), float("inf"), float("-inf"),
]
for v in values:
    sys.stdout.write(json.dumps({"kind": "value", "in": v, "out": dumps(v)}) + "\n")
for f in floats:
    sys.stdout.write(json.dumps({"kind": "float", "hex": f.hex(), "out": dumps(f)}) + "\n")
