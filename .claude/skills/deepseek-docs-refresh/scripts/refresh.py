#!/usr/bin/env python3
"""Refresh third_party/deepseek-docs from the live api-docs.deepseek.com.

Fetches every URL in the site's sitemap, converts each page to Markdown, and
compares the result against the mirror already on disk. Pages are classified
so a reviewer can tell an upstream edit from a conversion artefact:

  unchanged   the conversion matches the mirror exactly
  date-only   only the `fetched:` line moved
  changed     upstream edited the page
  new         the page is not in the mirror yet

The classification is only worth anything because the converter reproduces
unchanged pages byte for byte. `--verify` asserts exactly that and writes
nothing: run it first, and treat any page that reports `changed` without a
visible upstream reason as a converter bug rather than a docs update.

  refresh.py --verify                    fetch and classify, write nothing
  refresh.py                             fetch, classify, write the mirror
  refresh.py --cache DIR                 reuse (or fill) an HTML cache
"""

import argparse
import html
import os
import re
import sys
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import apiconv
import convert as C

BASE = "https://api-docs.deepseek.com"
UA = "agent-harness docs mirror refresh (+https://github.com/tinkerindustries/agent-harness)"

# Pages the mirror keeps by hand. They are never overwritten: faq.md redirects
# to an app on another host and carries a note of ours instead.
HAND_WRITTEN = {"faq"}

# The prompt library renders from /data/prompts.json rather than page markup,
# so the page has no article body to convert. _data/prompts.json and
# prompt-library.md are maintained separately; see the mirror's README.
NO_BODY = {"prompt-library"}

# Fragments the converter faithfully reproduces but which carry no content,
# because the real thing is an interactive widget. Each is rewritten in place
# after conversion so a refresh does not undo the note.
LOCAL_NOTES = [
    (
        "quick_start/token_usage",
        "#### Image Token Calculator\n\nWidth (px)Height (px)\n",
        "#### Image Token Calculator\n\n"
        "The page offers an interactive calculator here, taking a width and a height in\n"
        "pixels. It renders client-side and has no static content, so it is not mirrored.\n",
    ),
]


def fetch(url, cache_path=None):
    if cache_path and os.path.exists(cache_path) and os.path.getsize(cache_path):
        return open(cache_path, encoding="utf-8").read()
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=60) as resp:
        body = resp.read().decode("utf-8", "replace")
    if cache_path:
        os.makedirs(os.path.dirname(cache_path), exist_ok=True)
        open(cache_path, "w", encoding="utf-8").write(body)
    return body


def sitemap_urls(cache_dir):
    xml = fetch(BASE + "/sitemap.xml", cache_dir and os.path.join(cache_dir, "sitemap.xml"))
    return [m.group(1) for m in re.finditer(r"<loc>\s*([^<\s]+)\s*</loc>", xml)]


def slug(url):
    return url[len(BASE):].strip("/") or "index"


def render(page, url, fetched):
    body = C.extract_markdown_div(page)
    m = re.search(r"<h1[^>]*>(.*?)</h1>", body, re.S)
    title = html.unescape(re.sub(r"<[^>]+>", "", m.group(1))) if m else ""
    title = title.replace("​", "").strip()
    if "openapi-left-panel__container" in body:
        return apiconv.render_page(page, url, title, fetched)
    return C.convert(page, url, title, fetched)


def apply_local_notes(name, md):
    for target, stub, replacement in LOCAL_NOTES:
        if name == target and stub in md:
            md = md.replace(stub, replacement)
    return md


def strip_fetched(md):
    return "\n".join(l for l in md.splitlines() if not l.startswith("fetched:"))


def image_refs(md):
    return set(re.findall(r"\]\((?:\.\./)*_img/([^)]+)\)", md))


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mirror", default="third_party/deepseek-docs",
                    help="the mirror directory (default: %(default)s)")
    ap.add_argument("--cache", help="directory for fetched HTML, reused across runs")
    ap.add_argument("--fetched", help="the fetched: date to stamp (default: today, UTC)")
    ap.add_argument("--verify", action="store_true",
                    help="classify every page and write nothing")
    args = ap.parse_args()

    if args.fetched:
        fetched = args.fetched
    else:
        import datetime
        fetched = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")

    mirror = os.path.abspath(args.mirror)
    if not os.path.isdir(mirror):
        sys.exit("no such mirror directory: " + mirror)

    urls = sitemap_urls(args.cache)
    print("sitemap: %d pages\n" % len(urls))

    buckets = {"unchanged": [], "date-only": [], "changed": [], "new": [], "skipped": []}
    wanted_images = set()
    writes = []

    for url in sorted(urls):
        name = slug(url)
        if name in HAND_WRITTEN or name in NO_BODY:
            buckets["skipped"].append(name)
            continue
        cache_path = args.cache and os.path.join(args.cache, name.replace("/", "_") + ".html")
        try:
            md = apply_local_notes(name, render(fetch(url, cache_path), url, fetched))
        except SystemExit as exc:
            print("  ! %s: %s" % (name, exc))
            buckets["skipped"].append(name)
            continue

        wanted_images |= image_refs(md)
        target = os.path.join(mirror, name + ".md")
        if not os.path.exists(target):
            buckets["new"].append(name)
        else:
            old = open(target, encoding="utf-8").read()
            if old == md:
                buckets["unchanged"].append(name)
            elif strip_fetched(old) == strip_fetched(md):
                buckets["date-only"].append(name)
            else:
                buckets["changed"].append(name)
        writes.append((target, md))

    for label in ("changed", "new", "skipped"):
        for name in buckets[label]:
            print("%-10s %s" % (label, name))
    print("\nunchanged %d | date-only %d | changed %d | new %d | skipped %d"
          % tuple(len(buckets[k]) for k in
                  ("unchanged", "date-only", "changed", "new", "skipped")))

    have = set(os.listdir(os.path.join(mirror, "_img"))) if os.path.isdir(os.path.join(mirror, "_img")) else set()
    missing = sorted(wanted_images - have)
    if missing:
        print("\nimages not in the mirror: %s" % ", ".join(missing))

    if not buckets["changed"] and not buckets["new"] and not missing:
        print("\nno upstream change beyond the fetched: date")

    if args.verify:
        print("--verify: nothing written")
        return 0

    for target, md in writes:
        os.makedirs(os.path.dirname(target), exist_ok=True)
        open(target, "w", encoding="utf-8").write(md)
    print("\nwrote %d pages to %s" % (len(writes), mirror))

    for name in missing:
        url = "%s/img/%s" % (BASE, name)
        dest = os.path.join(mirror, "_img", name)
        try:
            req = urllib.request.Request(url, headers={"User-Agent": UA})
            with urllib.request.urlopen(req, timeout=120) as resp:
                data = resp.read()
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            open(dest, "wb").write(data)
            print("fetched image %s (%d bytes)" % (name, len(data)))
        except Exception as exc:  # noqa: BLE001 - report and carry on
            print("could not fetch image %s: %s" % (name, exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
