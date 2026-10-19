#!/usr/bin/env python3
"""Fail if any non-ignored file in the repository contains a real sandbox ID.

Usage (from the repository root):
    python3 -I configs/demo/check_no_sandbox_ids.py LOCAL_ID_FILE...

The IDs are collected from the given local files (JSON values of keys ending in
"id", also when the JSON is escaped inside a string) and are never printed.
Exit codes: 0 ok, 1 an ID was found in a repository file, 2 usage or wrong path.
"""
import json
import re
import subprocess
import sys

ID_RE = re.compile(r"^[0-9A-Za-z_-]{20,}$")
# Text fallback: "...id": "<value>", also with backslash-escaped quotes.
TEXT_RE = re.compile(r'\\*"[^"\\]*?[iI][dD]\\*"\s*:\s*\\*"([0-9A-Za-z_-]{20,})\\*"')
MIN_IDS = 10


def walk(node, found):
    if isinstance(node, dict):
        for key, value in node.items():
            if isinstance(value, str) and str(key).lower().endswith("id") and ID_RE.match(value):
                found.add(value)
            walk(value, found)
    elif isinstance(node, list):
        for value in node:
            walk(value, found)
    elif isinstance(node, str) and node[:1] in "{[":
        try:
            walk(json.loads(node), found)
        except ValueError:
            pass


def collect(path):
    with open(path, encoding="utf-8", errors="replace") as fh:
        text = fh.read()
    found = set(TEXT_RE.findall(text))
    try:
        walk(json.loads(text), found)
    except ValueError:
        pass
    return found


def main(argv):
    if not argv:
        print("usage: check_no_sandbox_ids.py LOCAL_ID_FILE...", file=sys.stderr)
        return 2
    ids = set()
    for path in argv:
        try:
            ids |= collect(path)
        except OSError as exc:
            print("cannot read %s: %s" % (path, exc.strerror), file=sys.stderr)
            return 2
    if len(ids) < MIN_IDS:
        print("only %d ID-like tokens found, expected at least %d: wrong file?" % (len(ids), MIN_IDS), file=sys.stderr)
        return 2
    listing = subprocess.run(
        ["git", "ls-files", "-co", "--exclude-standard", "-z"],
        check=True, capture_output=True,
    ).stdout.decode("utf-8", errors="replace")
    needles = [i.encode() for i in ids]
    files = [p for p in listing.split("\0") if p]
    offenders = []
    for path in files:
        try:
            with open(path, "rb") as fh:
                data = fh.read()
        except OSError:
            continue  # deleted or unreadable entries cannot hold an ID
        if any(n in data for n in needles):
            offenders.append(path)
    if offenders:
        print("sandbox IDs found in:")
        for path in offenders:
            print("  " + path)
        return 1
    print("ok: no sandbox IDs in %d files" % len(files))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
