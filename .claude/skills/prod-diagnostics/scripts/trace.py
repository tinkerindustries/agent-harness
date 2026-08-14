#!/usr/bin/env python3
"""Read the provider HTTP trace for a session in a running stack.

Files are gzipped JSON Lines at /data/http/<day>/<session-id>/exchanges.jsonl.gz
inside the harness container. A live session's file is an unterminated gzip
stream: gzip writes every flushed line and then exits non-zero with
"corrupted data". Those lines are good, so the error is tolerated here.

Bodies carry the whole conversation so far and get very large. The default is
a summary line per exchange; ask for a single --seq before printing bodies.
"""

import argparse
import json
import subprocess
import sys

PROD_PROJECT = "deepseek-harness-prod"
DEV_PROJECT = "deepseek-harness"
ROOT = "/data/http"


def sh(project, command, binary=False):
    proc = subprocess.run(
        ["docker", "compose", "-p", project, "exec", "-T", "harness", "sh", "-c", command],
        capture_output=True, text=not binary,
    )
    return proc


def find_dirs(project, session):
    """Every day directory holding this session. A run can cross midnight."""
    proc = sh(project, f"ls -d {ROOT}/*/{session} 2>/dev/null")
    if proc.returncode != 0 or not proc.stdout.strip():
        return []
    return [p for p in proc.stdout.split() if p.strip()]


def load(project, session):
    dirs = find_dirs(project, session)
    if not dirs:
        sys.exit(
            f"trace: no capture for {session}.\n"
            "Check the id with --list. A session that made no provider call has "
            "no file; calls outside any session are under the 'harness' directory."
        )
    exchanges = []
    for d in dirs:
        # stderr dropped on purpose: a live file always reports corrupted data
        # at the end, after writing the lines that are actually there.
        proc = sh(project, f"gzip -dc {d}/exchanges.jsonl.gz 2>/dev/null")
        for line in proc.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                e = json.loads(line)
            except json.JSONDecodeError:
                continue  # a torn final line on a live file
            e["_day"] = d.split("/")[-2]
            exchanges.append(e)
    if not exchanges:
        sys.exit(f"trace: {session} has a capture directory but no readable exchanges yet")
    exchanges.sort(key=lambda e: (e["_day"], e.get("seq", 0), e.get("attempt", 0)))
    return exchanges


def listing(project):
    proc = sh(project, f"ls -d {ROOT}/*/*/ 2>/dev/null")
    if proc.returncode != 0 or not proc.stdout.strip():
        sys.exit("trace: nothing captured, or the stack is not up")
    for path in sorted(proc.stdout.split()):
        parts = path.rstrip("/").split("/")
        print(f"{parts[-2]}  {parts[-1]}")


def split_url(url):
    """Host and path, kept apart.

    A run can call more than one provider — a vision tool goes elsewhere —
    and truncating a long url hides exactly the part that says which.
    """
    rest = url.split("://", 1)[-1]
    host, _, path = rest.partition("/")
    return host, "/" + path


def summarise(exchanges):
    print(f"{'seq':>5} {'try':>3} {'status':>6} {'ttfb':>7} {'total':>8}  {'host':<28} path")
    hosts = {}
    for e in exchanges:
        status = e.get("status") or "-"
        host, path = split_url(e.get("url", ""))
        hosts[host] = hosts.get(host, 0) + 1
        if len(path) > 34:
            path = path[:33] + "…"
        note = f"  ERROR: {e['error']}" if e.get("error") else ""
        print(f"{e.get('seq', 0):>5} {e.get('attempt', 0):>3} {str(status):>6} "
              f"{str(e.get('ttfb_ms', '-')):>7} {str(e.get('total_ms', '-')):>8}  "
              f"{host:<28} {path}{note}")
    bad = [e for e in exchanges if e.get("error") or (e.get("status") or 200) >= 400]
    retried = [e for e in exchanges if e.get("attempt", 0) > 0]
    print(f"\n{len(exchanges)} exchanges, {len(bad)} failed, {len(retried)} retries")
    if len(hosts) > 1:
        breakdown = ", ".join(f"{n} {h}" for h, n in sorted(hosts.items(), key=lambda kv: -kv[1]))
        print(f"across {len(hosts)} hosts: {breakdown}")
    if bad:
        print("failed at seq: " + ", ".join(str(e.get("seq")) for e in bad))


def show(exchange, field, limit):
    if field:
        # Asking for one field is asking for it whole; --limit still caps it
        # if the caller set one deliberately.
        value = exchange.get(field)
        if value is None:
            sys.exit(f"trace: no field {field!r}. Have: {', '.join(sorted(exchange))}")
        text = value if isinstance(value, str) else json.dumps(value, indent=2)
        print(text if limit <= 0 else text[:limit])
        return
    for key, value in exchange.items():
        if key.startswith("_"):
            continue
        text = value if isinstance(value, str) else json.dumps(value)
        if limit > 0 and len(text) > limit:
            text = text[:limit] + f"… [{len(text)} bytes; --field {key} for all]"
        print(f"{key}: {text}")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("session", nargs="?", help="session id, or 'harness' for non-session calls")
    ap.add_argument("--list", action="store_true", help="days and sessions with a capture")
    ap.add_argument("--seq", type=int, help="show one exchange instead of the summary")
    ap.add_argument("--attempt", type=int, default=None, help="with --seq, pick a retry")
    ap.add_argument("--field", help="with --seq, print one field whole (e.g. req_body)")
    ap.add_argument("--limit", type=int, default=None,
                    help="truncate at N chars (0 = no limit). Default 2000 for the "
                         "whole exchange, unlimited when --field names one field.")
    ap.add_argument("--out", help="copy the raw .gz files to this local directory")
    ap.add_argument("--dev", action="store_true", help="dev stack instead of prod")
    args = ap.parse_args()

    project = DEV_PROJECT if args.dev else PROD_PROJECT

    if args.list:
        listing(project)
        return
    if not args.session:
        ap.error("give a session id, or --list")

    if args.out:
        import os
        os.makedirs(args.out, exist_ok=True)
        for d in find_dirs(project, args.session):
            day = d.split("/")[-2]
            dest = os.path.join(args.out, f"{day}-{args.session}.jsonl.gz")
            subprocess.run(["docker", "compose", "-p", project, "cp",
                            f"harness:{d}/exchanges.jsonl.gz", dest], check=True)
            print(dest)
        return

    exchanges = load(project, args.session)

    if args.seq is None:
        summarise(exchanges)
        return

    matches = [e for e in exchanges if e.get("seq") == args.seq]
    if args.attempt is not None:
        matches = [e for e in matches if e.get("attempt") == args.attempt]
    if not matches:
        sys.exit(f"trace: no exchange with seq {args.seq}")
    if len(matches) > 1 and args.attempt is None:
        print(f"# seq {args.seq} has {len(matches)} attempts; showing "
              f"attempt {matches[0].get('attempt')} — use --attempt N for another\n")
    limit = args.limit if args.limit is not None else (0 if args.field else 2000)
    show(matches[0], args.field, limit)


if __name__ == "__main__":
    main()
