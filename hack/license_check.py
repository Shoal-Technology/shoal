#!/usr/bin/env python3
"""Dependency license gate for Shoal.

Lists every module in the build and test graph (`go list -deps -test ./...`),
classifies the license files shipped in each module's source directory, and
fails on GPL/LGPL/AGPL or on any license it cannot classify. Optionally writes
the NOTICE file, which carries Shoal's notice plus every NOTICE file that a
dependency ships (Apache-2.0 section 4(d)).

Usage (from the repository root, with Go on PATH):
  python3 hack/license_check.py                   # gate; exit 1 on violations
  python3 hack/license_check.py --table OUT.tsv   # also write the table
  python3 hack/license_check.py --notice NOTICE   # also regenerate NOTICE
"""
import argparse
import os
import re
import subprocess
import sys

ALLOWED = {"Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC", "MPL-2.0", "Public-domain"}
FORBIDDEN = {"GPL", "LGPL", "AGPL-3.0"}

# Files that are not license texts for compiled code. Each entry needs a reason
# and is reviewed by hand when the dependency changes.
ALLOWLIST = {
    ("github.com/opencontainers/go-digest", "LICENSE.docs"):
        "CC-BY-SA-4.0 covers the documentation only; the code is Apache-2.0",
    ("modernc.org/memory", "LICENSE-LOGO"):
        "attribution for a logo image, not a license for code",
    ("modernc.org/sqlite", "LICENSE-3RD-PARTY.md"):
        "aggregate notice file; its GNU mentions are the MPL-2.0 Secondary License definition",
}

SHOAL_NOTICE = """Shoal
Copyright 2026 the Shoal authors
https://shoal-technology.solutions

This product includes software developed by the Shoal authors.

Shoal is licensed under the Apache License, Version 2.0. See the LICENSE file.

The sections below reproduce the NOTICE files shipped by dependencies in
Shoal's build and test graph, as the Apache License, Version 2.0 requires.
"""


def classify(text):
    t = re.sub(r"\s+", " ", text.lower())
    # MPL-2.0 names the GNU licenses as Secondary Licenses, so check it first.
    if "mozilla public license" in t and "2.0" in t:
        return "MPL-2.0"
    if "apache license" in t and "version 2.0" in t:
        return "Apache-2.0"
    if "permission is hereby granted, free of charge" in t:
        return "MIT"
    if "permission to use, copy, modify, and/or distribute this software for any purpose with or without fee" in t:
        return "ISC"
    if "redistribution and use in source and binary forms" in t:
        return "BSD-3-Clause" if ("neither the name" in t or "endorse" in t) else "BSD-2-Clause"
    if "gnu affero general public license" in t:
        return "AGPL-3.0"
    if "gnu lesser general public license" in t:
        return "LGPL"
    if "gnu general public license" in t:
        return "GPL"
    if "public domain" in t or "unlicense" in t:
        return "Public-domain"
    return "UNKNOWN"


def graph_modules(repo):
    out = subprocess.run(
        ["go", "list", "-deps", "-test", "-f",
         "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}", "./..."],
        cwd=repo, capture_output=True, text=True, check=True).stdout
    mods = {}
    for line in out.splitlines():
        parts = line.split("\t")
        if len(parts) == 3 and parts[0]:
            mods[parts[0]] = (parts[1], parts[2])
    return mods


def license_files(d):
    if not d or not os.path.isdir(d):
        return []
    return sorted(f for f in os.listdir(d)
                  if os.path.isfile(os.path.join(d, f))
                  and re.match(r"(?i)^(license|licence|copying)", f))


def notice_files(d):
    if not d or not os.path.isdir(d):
        return []
    return sorted(f for f in os.listdir(d)
                  if os.path.isfile(os.path.join(d, f)) and re.match(r"(?i)^notice", f))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", default=os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    ap.add_argument("--table")
    ap.add_argument("--notice")
    args = ap.parse_args()

    mods = graph_modules(args.repo)
    rows, problems, notices = [], [], []
    for path in sorted(mods):
        ver, d = mods[path]
        kinds = {}
        for f in license_files(d):
            with open(os.path.join(d, f), encoding="utf-8", errors="replace") as fh:
                kinds[f] = classify(fh.read())
        for f in notice_files(d):
            with open(os.path.join(d, f), encoding="utf-8", errors="replace") as fh:
                notices.append((path, ver, f, fh.read()))
        if not kinds:
            problems.append(f"{path} {ver}: no license file found")
        for f, k in kinds.items():
            if (path, f) in ALLOWLIST:
                k = "allowlisted"
            elif k in FORBIDDEN:
                problems.append(f"{path} {ver}: {f} is {k}")
            elif k not in ALLOWED:
                problems.append(f"{path} {ver}: {f} is unclassified ({k})")
        summary = " + ".join(sorted(set(kinds.values()))) or "NO-FILE"
        rows.append((path, ver, summary, ",".join(f"{k}={v}" for k, v in kinds.items()) or "-"))

    if args.table:
        with open(args.table, "w", encoding="utf-8", newline="\n") as fh:
            fh.write("module\tversion\tlicense\tfiles\n")
            for r in rows:
                fh.write("\t".join(r) + "\n")
    if args.notice:
        with open(args.notice, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(SHOAL_NOTICE)
            for path, ver, f, text in notices:
                fh.write(f"\n==== {path} {ver} ({f}) ====\n\n{text.strip()}\n")

    print(f"modules checked: {len(rows)}; NOTICE files found: {len(notices)}")
    if problems:
        print("license problems:")
        for p in problems:
            print("  " + p)
        return 1
    print("license check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
