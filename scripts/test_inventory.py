#!/usr/bin/env python3
"""Check test-inventory.json against source or refresh measured results."""

import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / "test-inventory.json"
TEST_RE = re.compile(r"^func\s+(?:\([^)]*\)\s+)?(Test[A-Za-z0-9_]*)\s*\(", re.M)


def source_tests():
    found = {}
    for path in ROOT.rglob("*_test.go"):
        if ".git" in path.parts or "vendor" in path.parts:
            continue
        for name in TEST_RE.findall(path.read_text(encoding="utf-8")):
            found.setdefault(name, path.relative_to(ROOT).as_posix())
    return found


def check():
    inventory = json.loads(MANIFEST.read_text(encoding="utf-8"))
    recorded = inventory.get("tests", {})
    current = source_tests()
    missing = sorted(set(current) - set(recorded))
    stale = sorted(set(recorded) - set(current))
    moved = sorted(name for name in set(current) & set(recorded)
                   if recorded[name].get("file") != current[name])
    if missing or stale or moved:
        if missing:
            print("missing from test-inventory.json:")
            print("\n".join(f"  {name} ({current[name]})" for name in missing))
        if stale:
            print("no longer present in source:")
            print("\n".join(f"  {name}" for name in stale))
        if moved:
            print("owner file changed:")
            print("\n".join(f"  {name}: {recorded[name].get('file')} -> {current[name]}" for name in moved))
        return 1
    print(f"test inventory matches {len(current)} source tests")
    return 0


def refresh():
    cmd = ["go", "test", "-json", "-count=1", "-buildvcs=false", "./..."]
    env = os.environ.copy()
    env.setdefault("GOCACHE", "/tmp/td-gocache")
    proc = subprocess.Popen(cmd, cwd=ROOT, env=env, stdout=subprocess.PIPE, text=True)
    durations = {}
    statuses = {}
    failures = []
    test_output = {}
    assert proc.stdout is not None
    for line in proc.stdout:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if event.get("Action") in {"pass", "skip"} and event.get("Test"):
            durations[event["Test"]] = event.get("Elapsed") or 0
            statuses[event["Test"]] = event["Action"]
        if event.get("Action") == "output" and event.get("Test"):
            test_output.setdefault(event["Test"], []).append(event.get("Output", ""))
        if event.get("Action") == "fail":
            key = event.get("Test") or event.get("Package", "package")
            failures.append((key, event.get("Elapsed", 0)))
    code = proc.wait()
    if code:
        sys.stderr.write("full suite failed; inventory was not changed\n")
        for name, elapsed in failures:
            sys.stderr.write(f"FAIL {name} ({elapsed:.3f}s)\n")
            for output in test_output.get(name, []):
                sys.stderr.write(output)
        return code

    current = source_tests()
    old = json.loads(MANIFEST.read_text(encoding="utf-8"))
    tests = {}
    for name, owner in sorted(current.items()):
        prior = old.get("tests", {}).get(name, {})
        measured = durations.get(name)
        if measured is None:
            measured = prior.get("seconds", 0) or 0
        tests[name] = {
            "file": owner,
            "seconds": round(float(measured), 3),
            "status": statuses.get(name, prior.get("status", "not_measured")),
            "deps": prior.get("deps", []),
        }
    old["measured_from"] = "go test -json -count=1 -buildvcs=false ./... (refreshed by scripts/test_inventory.py)"
    old["tests"] = tests
    MANIFEST.write_text(json.dumps(old, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"refreshed {len(tests)} test entries")
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in {"check", "refresh"}:
        print("usage: test_inventory.py check|refresh", file=sys.stderr)
        raise SystemExit(2)
    raise SystemExit(check() if sys.argv[1] == "check" else refresh())
