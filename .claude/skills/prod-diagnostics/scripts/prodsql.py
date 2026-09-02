#!/usr/bin/env python3
"""Run a read-only SELECT against the harness store in a running stack.

The database is reached inside the container, the image has no sqlite3 CLI,
and the file has a live writer. This wraps the one invocation that handles
all three: python3 in the container, opened mode=ro over a URI.

Only SELECT, WITH and PRAGMA are accepted. Diagnosis does not write to prod.
"""

import argparse
import json
import subprocess
import sys

PROD_PROJECT = "deepseek-harness-prod"
DEV_PROJECT = "deepseek-harness"

# Run inside the container. Reads the query from stdin so quoting in the
# caller's shell never has to survive a round trip through -c.
RUNNER = r"""
import json, sqlite3, sys

query = sys.stdin.read()
db = sqlite3.connect("file:/data/harness.db?mode=ro", uri=True)
cur = db.execute(query)
cols = [d[0] for d in cur.description] if cur.description else []
rows = [list(r) for r in cur.fetchall()]
print(json.dumps({"columns": cols, "rows": rows}))
"""

SCHEMA_QUERY = """
SELECT m.name, m.type, group_concat(p.name, ', ')
FROM sqlite_master m
LEFT JOIN pragma_table_info(m.name) p
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
GROUP BY m.name ORDER BY m.name
"""


def guard(query):
    """Reject anything that is not a read.

    Comments can hide a leading verb, so they come off before the check.
    """
    stripped = []
    for line in query.splitlines():
        head = line.split("--", 1)[0]
        if head.strip():
            stripped.append(head)
    text = " ".join(stripped).strip()
    if not text:
        sys.exit("prodsql: empty query")
    first = text.split(None, 1)[0].upper().lstrip("(")
    if first not in ("SELECT", "WITH", "PRAGMA", "EXPLAIN"):
        sys.exit(
            f"prodsql: refusing '{first}' — this reads production, it does not "
            "write to it. Only SELECT / WITH / PRAGMA / EXPLAIN are allowed."
        )
    for banned in (";DELETE", ";UPDATE", ";INSERT", ";DROP", ";ALTER", ";PRAGMA"):
        if banned in text.upper().replace(" ", ""):
            sys.exit("prodsql: refusing a statement chained after a semicolon")
    return text


def run(project, query):
    proc = subprocess.run(
        ["docker", "compose", "-p", project, "exec", "-T", "harness",
         "python3", "-c", RUNNER],
        input=query, capture_output=True, text=True,
    )
    if proc.returncode != 0:
        err = (proc.stderr or "").strip()
        if "no such service" in err or "not running" in err:
            err += f"\n\nIs the {project} stack up? Try: scripts/prod.sh status"
        sys.exit(f"prodsql: query failed\n{err}")
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError:
        sys.exit(f"prodsql: unexpected output\n{proc.stdout[:2000]}")


def as_table(columns, rows, width):
    if not rows:
        return "(no rows)"
    def cell(v):
        s = "" if v is None else str(v)
        s = s.replace("\n", "\\n")
        return s if len(s) <= width else s[: width - 1] + "…"
    body = [[cell(v) for v in r] for r in rows]
    widths = [len(c) for c in columns]
    for r in body:
        for i, c in enumerate(r):
            widths[i] = max(widths[i], len(c))
    out = ["  ".join(c.ljust(widths[i]) for i, c in enumerate(columns)),
           "  ".join("-" * w for w in widths)]
    out += ["  ".join(c.ljust(widths[i]) for i, c in enumerate(r)) for r in body]
    out.append(f"\n({len(rows)} row{'s' if len(rows) != 1 else ''})")
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("query", nargs="?", help="a SELECT; omit to read from stdin")
    ap.add_argument("--dev", action="store_true", help="dev stack instead of prod")
    ap.add_argument("--schema", action="store_true", help="tables and their columns")
    ap.add_argument("--json", action="store_true", help="raw JSON instead of a table")
    ap.add_argument("--width", type=int, default=60, help="max column width (default 60)")
    args = ap.parse_args()

    project = DEV_PROJECT if args.dev else PROD_PROJECT

    if args.schema:
        query = SCHEMA_QUERY
        if args.width == 60:  # the column list is the whole point here
            args.width = 400
    elif args.query:
        query = guard(args.query)
    elif not sys.stdin.isatty():
        query = guard(sys.stdin.read())
    else:
        ap.error("give a query as an argument, on stdin, or pass --schema")

    result = run(project, query)
    if args.json:
        print(json.dumps(result, indent=2, default=str))
    else:
        print(as_table(result["columns"], result["rows"], args.width))


if __name__ == "__main__":
    main()
