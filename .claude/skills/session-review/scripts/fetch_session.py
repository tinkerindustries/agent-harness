#!/usr/bin/env python3
"""Pull one harness session out of a running stack and render it for review.

Writes, under --out:

    session.json        the session row verbatim (model, effort, plan, status)
    events.jsonl        the event log verbatim, one JSON object per line
    metrics.md          per-sub-turn table, totals, and flagged anomalies
    transcript/NN.md    the readable transcript, split at sub-turn boundaries

The raw files are kept so a finding can always be traced back to the bytes the
harness recorded. The rendered transcript is what a reviewer reads.
"""

import argparse
import datetime as dt
import json
import os
import re
import sys
import urllib.error
import urllib.request
from collections import Counter, defaultdict

PAGE = 500  # http.events_limit_max clamps a page to this


def get(base, path, timeout=120):
    url = base.rstrip("/") + path
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")[:400]
        sys.exit(f"GET {url} -> {e.code}: {body}")
    except urllib.error.URLError as e:
        sys.exit(f"GET {url} failed: {e.reason}\n"
                 f"Is the harness up on {base}? "
                 f"(prod is 127.0.0.1:8180, dev is 127.0.0.1:8080)")


def fetch_events(base, sid):
    events, frm = [], 0
    while True:
        page = get(base, f"/api/sessions/{sid}/events?from={frm}&limit={PAGE}")
        events.extend(page["events"])
        if not page.get("has_more"):
            return events
        frm = page["next"]


def clip(text, head, tail):
    """Trim the middle out of a long block, saying how much went missing."""
    if head <= 0 or len(text) <= head + tail + 200:
        return text
    omitted = len(text) - head - tail
    return (text[:head]
            + f"\n\n… [{omitted} chars omitted — re-render with --full or "
              f"--turn N --full to see them] …\n\n"
            + (text[-tail:] if tail else ""))


def ts(s):
    """Parse an RFC3339 stamp with however many fractional digits Go emitted."""
    if not s:
        return None
    s = s.replace("Z", "+00:00")
    m = re.match(r"^(.*\.\d{6})\d*(\+.*)$", s)
    if m:
        s = m.group(1) + m.group(2)
    try:
        return dt.datetime.fromisoformat(s)
    except ValueError:
        return None


def fmt_args(raw):
    """Tool arguments arrive as a JSON string; pretty-print when it parses."""
    try:
        return json.dumps(json.loads(raw), indent=2)
    except Exception:
        return raw


# ---------------------------------------------------------------- grouping

def group_by_subturn(events):
    """Split the flat log into sub-turns, keeping event order within each.

    Everything before the first turn_started (session_started) goes in a
    preamble bucket keyed 0; the terminal events (run_finished, error) land in
    whichever sub-turn was last open, which is where they happened.
    """
    turns = defaultdict(list)
    order = []
    cur = 0
    turns[0] = []
    order.append(0)
    for e in events:
        if e["kind"] == "turn_started":
            cur = e["payload"].get("sub_turn", cur + 1)
            if cur not in turns:
                turns[cur] = []
                order.append(cur)
        turns[cur].append(e)
    return [(n, turns[n]) for n in order]


# ---------------------------------------------------------------- metrics

def build_metrics(sess, events, turns):
    rows = []
    flags = []
    tool_counter = Counter()
    tool_errors = Counter()
    totals = dict(prompt=0, hit=0, miss=0, completion=0, reasoning=0,
                  cost=0.0, elapsed=0, requests=0, tool_ms=0, wall_ms=0)
    recent_sigs = []
    err_streak = []          # (sub_turn, tool_name) of the current run of failures
    call_at = {}             # tool_call_id -> created_at of the call
    prev_turn_end = None

    for n, evs in turns:
        if n == 0 and not any(e["kind"] == "turn_started" for e in evs):
            continue
        usage = [e["payload"] for e in evs if e["kind"] == "usage"]
        fin = next((e["payload"] for e in evs if e["kind"] == "turn_finished"), {})
        calls = [e["payload"] for e in evs if e["kind"] == "tool_call"]
        results = [e["payload"] for e in evs if e["kind"] == "tool_result"]
        denied = [e["payload"] for e in evs if e["kind"] == "tool_denied"]
        errs = [e["payload"] for e in evs if e["kind"] == "error"]

        # Wall clock, from the event stamps. Every event a sub-turn commits is
        # stamped at commit time, so turn_started carries the time the model
        # *finished*, not the time it began: the span from the previous turn's
        # last tool result to this turn's commit is the model call. Reading it
        # as dead time would invent a harness stall out of ordinary thinking,
        # so overhead is that span minus the model's own elapsed_ms.
        for e in evs:
            if e["kind"] == "tool_call":
                call_at[e["payload"]["id"]] = ts(e.get("created_at"))
        tool_ms = 0
        slowest = None
        for e in evs:
            if e["kind"] in ("tool_result", "tool_denied"):
                t0 = call_at.get(e["payload"].get("tool_call_id"))
                t1 = ts(e.get("created_at"))
                if t0 and t1:
                    ms = int((t1 - t0).total_seconds() * 1000)
                    tool_ms += ms
                    if slowest is None or ms > slowest[1]:
                        slowest = (e["payload"].get("name", "?"), ms)
        totals["tool_ms"] += tool_ms
        t_commit = ts(evs[0].get("created_at")) if evs else None
        t_end = ts(evs[-1].get("created_at")) if evs else None
        call_ms = 0
        if prev_turn_end and t_commit:
            call_ms = int((t_commit - prev_turn_end).total_seconds() * 1000)
        prev_turn_end = t_end or t_commit

        prompt = sum(u.get("prompt_tokens", 0) for u in usage)
        hit = sum(u.get("prompt_cache_hit_tokens", 0) for u in usage)
        miss = sum(u.get("prompt_cache_miss_tokens", 0) for u in usage)
        comp = sum(u.get("completion_tokens", 0) for u in usage)
        reas = sum(u.get("reasoning_tokens", 0) for u in usage)
        cost = sum(u.get("cost_usd", 0.0) for u in usage)
        elapsed = fin.get("elapsed_ms", 0)
        overhead_ms = max(call_ms - elapsed, 0) if call_ms and elapsed else 0

        totals["prompt"] += prompt
        totals["hit"] += hit
        totals["miss"] += miss
        totals["completion"] += comp
        totals["reasoning"] += reas
        totals["cost"] += cost
        totals["elapsed"] += elapsed
        totals["requests"] += len(usage)

        for c in calls:
            tool_counter[c["name"]] += 1
        for r in results:
            if r.get("is_error"):
                tool_errors[r.get("name", "?")] += 1

        note = []
        for u in usage:
            if u.get("attempt", 0):
                note.append(f"retry#{u['attempt']}")
                flags.append((n, "retry", "the loop re-sent this sub-turn "
                              f"(usage attempt {u['attempt']}) — both requests were billed"))
            if u.get("churn_point_index") is not None:
                note.append(f"churn@msg{u['churn_point_index']}")
                flags.append((n, "cache-churn", "the request's prefix diverged from the "
                              f"last one at messages index {u['churn_point_index']} — "
                              "the cache was rebuilt from there"))
        if fin.get("finish_reason") == "length":
            note.append("hit max_tokens")
            flags.append((n, "truncated-output", "the model was cut off at max_tokens "
                          "mid-answer, so whatever it was saying is incomplete"))
        for d in denied:
            note.append(f"denied:{d.get('name')}")
            flags.append((n, "tool-denied", f"{d.get('name')} refused by rule "
                          f"{d.get('rule')!r}"))
        for r in results:
            if r.get("is_error"):
                note.append(f"err:{r.get('name')}")
                err_streak.append((n, r.get("name", "?")))
            if r.get("truncated"):
                note.append(f"trunc:{r.get('name')}")
                flags.append((n, "result-truncated", f"the harness truncated the "
                              f"{r.get('name')} result — the model never saw the rest"))
        for e in errs:
            note.append("RUN ERROR")
            flags.append((n, "run-error", e.get("message", "")[:300]))

        # A tool that fails once is ordinary. A tool that keeps failing while
        # the model keeps calling it is the model and the harness deadlocked
        # against each other, and it is usually the most expensive thing in a
        # session — so close the streak and report it as one finding.
        if not any(r.get("is_error") for r in results) and err_streak:
            flush_streak(err_streak, flags)
            err_streak = []

        if slowest and slowest[1] > 60000:
            flags.append((n, "slow-tool", f"{slowest[0]} took {slowest[1]/1000:.0f}s "
                          "— check whether it hung or was genuinely that big"))
        if overhead_ms > 30000:
            flags.append((n, "harness-stall", f"{overhead_ms/1000:.0f}s of this sub-turn "
                          "went to neither the model nor a tool — the request took that "
                          "much longer end to end than the model's own elapsed_ms"))

        # A tool call repeated verbatim inside a short window is the model
        # looping rather than making progress.
        for c in calls:
            sig = (c["name"], c.get("arguments", ""))
            if sig in recent_sigs:
                flags.append((n, "repeated-call", f"{c['name']} called with byte-identical "
                              "arguments to a call in the last 5 sub-turns"))
                note.append("repeat")
            recent_sigs.append(sig)
        recent_sigs = recent_sigs[-40:]

        rows.append(dict(
            n=n, prompt=prompt, hit=hit, miss=miss, comp=comp, reas=reas,
            cost=cost, elapsed=elapsed, tool_ms=tool_ms,
            finish=fin.get("finish_reason", ""),
            tools=", ".join(c["name"] for c in calls) or "—",
            note=", ".join(note),
        ))

    if err_streak:
        flush_streak(err_streak, flags)

    # How the run ended. A session whose last event is not a clean completion
    # spent its final sub-turns failing to get out, which the per-turn table
    # shows but no single row calls out.
    fin_ev = next((e["payload"] for e in reversed(events)
                   if e["kind"] == "run_finished"), None)
    if fin_ev:
        reason = fin_ev.get("reason", "")
        if reason == "no_tool_calls":
            flags.append(("end", "unclean-finish",
                          "the run ended on `no_tool_calls` — the model stopped "
                          "talking without calling Complete, so nothing validated "
                          "its result. Read the last sub-turns for why"))
        elif reason == "max_sub_turns":
            flags.append(("end", "hit-turn-cap",
                          "the run was cut off at the sub-turn cap with work still "
                          "in flight — judge whether it was looping or merely slow"))
        if sess.get("complete_status") == "gave_up":
            flags.append(("end", "gave-up",
                          "the model called Complete with status `gave_up` — its own "
                          "account of why is in the run_finished block"))
    else:
        flags.append(("end", "no-finish-event",
                      "the log has no run_finished event at all — the session was "
                      "killed, or the row was left running by a dead worker"))

    flags.sort(key=lambda f: (f[0] == "end", f[0] if f[0] != "end" else 0))
    return rows, flags, tool_counter, tool_errors, totals


def flush_streak(streak, flags):
    """Report a run of consecutive failing calls as one flag, if it is a run."""
    if len(streak) < 3:
        return
    names = Counter(n for _, n in streak)
    tool, count = names.most_common(1)[0]
    lo, hi = streak[0][0], streak[-1][0]
    flags.append((lo, "error-loop",
                  f"{len(streak)} tool calls failed in a row across sub-turns "
                  f"{lo}–{hi}, {count} of them `{tool}` — the model kept retrying "
                  "against a rejection it could not satisfy"))


def write_metrics(path, sess, rows, flags, tool_counter, tool_errors, totals, nparts):
    hit_pct = (100.0 * totals["hit"] / totals["prompt"]) if totals["prompt"] else 0.0
    out = []
    a = out.append
    a(f"# Session metrics — {sess['id']}\n")
    a(f"- model `{sess.get('model')}`, effort `{sess.get('effort')}`, "
      f"permission mode `{sess.get('permission_mode')}`")
    a(f"- job type `{sess.get('job_type')}`, status **{sess.get('status')}**"
      + (f", complete status `{sess['complete_status']}`" if sess.get("complete_status") else ""))
    a(f"- started {sess.get('created_at')}, finished {sess.get('finished_at')}")
    a(f"- sub-turns: {len(rows)} · API requests: {totals['requests']} "
      f"(a request count above the sub-turn count means the loop retried)")
    a(f"- tokens: {totals['prompt']} prompt ({hit_pct:.1f}% cache hit), "
      f"{totals['completion']} completion, {totals['reasoning']} reasoning")
    a(f"- cost: ${totals['cost']:.4f}")
    t0, t1 = ts(sess.get("created_at")), ts(sess.get("finished_at"))
    wall = (t1 - t0).total_seconds() if t0 and t1 else 0.0
    model_s = totals["elapsed"] / 1000
    tool_s = totals["tool_ms"] / 1000
    if wall:
        other = max(wall - model_s - tool_s, 0)
        a(f"- time: {wall:.0f}s wall — {model_s:.0f}s model, {tool_s:.0f}s tools, "
          f"{other:.0f}s neither. That third figure is the one to be suspicious of: "
          "workspace preparation, queueing, and any harness stall live in it.")
    else:
        a(f"- time: {model_s:.0f}s model, {tool_s:.0f}s tools "
          "(no wall clock — the session row has no finished_at)")
    a(f"- transcript parts to read: {nparts}\n")

    a("## Tool use\n")
    a("| tool | calls | errored |")
    a("| --- | --- | --- |")
    for name, c in tool_counter.most_common():
        a(f"| {name} | {c} | {tool_errors.get(name, 0)} |")
    a("")

    a("## Flagged anomalies\n")
    if not flags:
        a("None. No retries, no cache churn, no denials, no truncation, no "
          "run errors, no verbatim-repeated calls.\n")
    else:
        a("Each of these is a *candidate* mechanical failure — confirm against the "
          "transcript before calling it one.\n")
        a("| sub-turn | kind | detail |")
        a("| --- | --- | --- |")
        for n, kind, detail in flags:
            a(f"| {n} | {kind} | {detail.replace('|', '\\|')} |")
        a("")

    a("## Per sub-turn\n")
    a("| # | prompt | cache hit | completion | reasoning | cost | model | tools | finish | called | notes |")
    a("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for r in rows:
        pct = (100.0 * r["hit"] / r["prompt"]) if r["prompt"] else 0.0
        a(f"| {r['n']} | {r['prompt']} | {pct:.0f}% | {r['comp']} | {r['reas']} | "
          f"${r['cost']:.4f} | {r['elapsed']/1000:.1f}s | {r['tool_ms']/1000:.1f}s | "
          f"{r['finish']} | {r['tools']} | {r['note']} |")
    a("")

    with open(path, "w") as f:
        f.write("\n".join(out))


# ---------------------------------------------------------------- transcript

def render_turn(n, evs, head, tail, keep_stdout):
    out = []
    a = out.append
    fin = next((e["payload"] for e in evs if e["kind"] == "turn_finished"), {})
    usage = [e["payload"] for e in evs if e["kind"] == "usage"]
    cost = sum(u.get("cost_usd", 0.0) for u in usage)

    if n == 0:
        started = next((e for e in evs if e["kind"] == "session_started"), None)
        if started:
            a("## Opening message (the task, the CLAUDE.md blocks, the skill catalogue)\n")
            a("```text")
            a(clip(started["payload"].get("opening_message", ""), head * 3, tail))
            a("```\n")
        if not any(e["kind"] != "session_started" for e in evs):
            return "\n".join(out)
    else:
        bits = [f"finish `{fin.get('finish_reason','?')}`"]
        if fin.get("elapsed_ms"):
            bits.append(f"{fin['elapsed_ms']/1000:.1f}s")
        bits.append(f"${cost:.4f}")
        for u in usage:
            if u.get("attempt", 0):
                bits.append(f"**retry attempt {u['attempt']}**")
            if u.get("churn_point_index") is not None:
                bits.append(f"**cache churn at messages[{u['churn_point_index']}]**")
        a(f"## Sub-turn {n} — {' · '.join(bits)}\n")

    for e in evs:
        k, p = e["kind"], e["payload"]
        if k in ("turn_started", "turn_finished", "usage", "session_started"):
            continue
        if k == "tool_stdout" and not keep_stdout:
            continue  # duplicated by the tool_result that follows

        if k == "reasoning_delta":
            a("### Reasoning\n")
            a("> " + clip(p["text"], head * 2, tail).replace("\n", "\n> ") + "\n")
        elif k == "content_delta":
            a("### Says\n")
            a(clip(p["text"], head * 2, tail) + "\n")
        elif k == "tool_call":
            a(f"### → {p['name']}  `{p['id']}`\n")
            a("```json")
            a(clip(fmt_args(p.get("arguments", "")), head, tail))
            a("```\n")
        elif k == "tool_denied":
            a(f"### ⛔ DENIED {p.get('name')}  `{p.get('tool_call_id')}` "
              f"— rule `{p.get('rule')}`\n")
            a("```text")
            a(clip(p.get("content", ""), head, tail))
            a("```\n")
        elif k == "tool_result":
            marks = []
            if p.get("is_error"):
                marks.append("**ERROR**")
            if p.get("truncated"):
                marks.append("**harness-truncated**")
            if p.get("child_session_id"):
                marks.append(f"child session `{p['child_session_id']}`")
            content = p.get("content", "")
            a(f"### ← {p.get('name')} result  `{p.get('tool_call_id')}` "
              f"({len(content)} chars) {' '.join(marks)}\n")
            a("```text")
            a(clip(content, head, tail))
            a("```\n")
            if p.get("diff"):
                a(f"<diff: {len(p['diff'])} lines>\n")
        elif k == "tool_stdout":
            a(f"### (stdout `{p.get('tool_call_id')}`)\n")
            a("```text")
            a(clip(p.get("text", ""), head, tail))
            a("```\n")
        elif k == "steer_message":
            a(f"### 🧭 STEER from {p.get('source','?')}\n")
            a("> " + p.get("text", "").replace("\n", "\n> ") + "\n")
        elif k == "steer_applied":
            a(f"### 🧭 steer applied — {json.dumps(p)}\n")
        elif k == "error":
            a("### 💥 RUN ERROR\n")
            a("```text")
            a(p.get("message", ""))
            a("```\n")
        elif k == "run_finished":
            a(f"### 🏁 Run finished — reason `{p.get('reason')}`"
              + (f", status `{p['status']}`" if p.get("status") else "") + "\n")
            if p.get("text"):
                a("Final text:\n")
                a(clip(p["text"], head * 4, tail) + "\n")
            if p.get("summary"):
                a("Summary field:\n")
                a("> " + clip(p["summary"], head * 4, 0).replace("\n", "\n> ") + "\n")
            if p.get("result"):
                a("Result payload:\n")
                a("```json")
                a(clip(json.dumps(p["result"], indent=2), head * 4, tail))
                a("```\n")
        else:
            a(f"### {k}\n")
            a("```json")
            a(clip(json.dumps(p, indent=2), head, tail))
            a("```\n")
    return "\n".join(out)


def write_transcript(outdir, sess, turns, part_bytes, head, tail, keep_stdout):
    tdir = os.path.join(outdir, "transcript")
    os.makedirs(tdir, exist_ok=True)
    for old in os.listdir(tdir):
        if re.match(r"^\d+\.md$", old):
            os.remove(os.path.join(tdir, old))

    parts, cur, size, first = [], [], 0, None
    for n, evs in turns:
        chunk = render_turn(n, evs, head, tail, keep_stdout)
        if not chunk.strip():
            continue
        if first is None:
            first = n
        if size and size + len(chunk) > part_bytes:
            parts.append((first, cur))
            cur, size, first = [], 0, n
        cur.append((n, chunk))
        size += len(chunk)
    if cur:
        parts.append((first, cur))

    paths = []
    for i, (_, chunk_list) in enumerate(parts, 1):
        lo, hi = chunk_list[0][0], chunk_list[-1][0]
        path = os.path.join(tdir, f"{i:02d}.md")
        with open(path, "w") as f:
            f.write(f"# {sess['id']} — transcript part {i} of {len(parts)} "
                    f"(sub-turns {lo}–{hi})\n\n")
            f.write("\n".join(c for _, c in chunk_list))
        paths.append((path, lo, hi))
    return paths


# ---------------------------------------------------------------- main

def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("session_id")
    ap.add_argument("--base-url", default=os.environ.get("HARNESS_URL", "http://127.0.0.1:8180"),
                    help="harness base URL (default prod, http://127.0.0.1:8180)")
    ap.add_argument("--out", help="directory to write into; required unless --turn")
    ap.add_argument("--part-bytes", type=int, default=40000,
                    help="target size of one transcript part (default 40000, which "
                         "keeps a part inside a single read for most agents)")
    ap.add_argument("--head", type=int, default=4000,
                    help="chars kept from the start of a long block (default 4000)")
    ap.add_argument("--tail", type=int, default=800,
                    help="chars kept from the end of a long block (default 800)")
    ap.add_argument("--full", action="store_true",
                    help="no truncation anywhere")
    ap.add_argument("--turn", type=int, action="append",
                    help="render only these sub-turns, to stdout; repeatable")
    ap.add_argument("--keep-stdout", action="store_true",
                    help="keep tool_stdout events (they duplicate tool_result)")
    args = ap.parse_args()
    if not args.turn and not args.out:
        ap.error("--out is required unless --turn is given")

    head = 10**9 if args.full else args.head
    tail = 0 if args.full else args.tail

    sid = args.session_id.strip()
    if not sid.startswith("sess-"):
        sid = "sess-" + sid.lstrip("-")

    sess = get(args.base_url, f"/api/sessions/{sid}")
    events = fetch_events(args.base_url, sid)
    turns = group_by_subturn(events)

    if args.turn:
        want = set(args.turn)
        for n, evs in turns:
            if n in want:
                print(render_turn(n, evs, head, tail, args.keep_stdout))
        return

    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "session.json"), "w") as f:
        json.dump(sess, f, indent=2)
    with open(os.path.join(args.out, "events.jsonl"), "w") as f:
        for e in events:
            f.write(json.dumps(e) + "\n")

    paths = write_transcript(args.out, sess, turns, args.part_bytes, head, tail,
                             args.keep_stdout)
    rows, flags, tools, tool_errors, totals = build_metrics(sess, events, turns)
    write_metrics(os.path.join(args.out, "metrics.md"), sess, rows, flags, tools,
                  tool_errors, totals, len(paths))

    print(f"session   {sid}  status={sess.get('status')}  model={sess.get('model')}")
    print(f"events    {len(events)}   sub-turns {len(rows)}   "
          f"cost ${totals['cost']:.4f}")
    print(f"anomalies {len(flags)} flagged (see metrics.md)")
    print(f"out       {os.path.abspath(args.out)}")
    print(f"metrics   {os.path.join(args.out, 'metrics.md')}")
    print("transcript parts, read in order:")
    for p, lo, hi in paths:
        print(f"  {p}   sub-turns {lo}-{hi}   {os.path.getsize(p)/1024:.0f}KB")


if __name__ == "__main__":
    main()
