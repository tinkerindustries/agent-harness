---
name: deepseek-docs-refresh
description: Refresh the vendored DeepSeek API docs mirror at third_party/deepseek-docs from the live api-docs.deepseek.com, converting every page in the sitemap to Markdown and classifying each one as unchanged, date-only, changed or new. Use this whenever the DeepSeek docs need re-downloading, whenever someone says the mirror is stale or that DeepSeek has shipped a model, endpoint or guide the mirror does not have, whenever a fact read out of third_party/deepseek-docs turns out to disagree with the live site, and before quoting pricing, rate limits or a request schema that has to be current. Also use it to answer what changed upstream since the last refresh, because the classification separates a genuine upstream edit from a conversion artefact.
---

# Refreshing the DeepSeek docs mirror

`third_party/deepseek-docs/` mirrors <https://api-docs.deepseek.com/> as
Markdown. DeepSeek publishes no OpenAPI spec and no docs source repository, so
the mirror is converted from the rendered site.

## Run it

```bash
python3 .claude/skills/deepseek-docs-refresh/scripts/refresh.py --verify --cache /tmp/ds-docs
```

Standard library only, no install step. `--verify` writes nothing. Run it
first, always, and read the classification before letting the script touch the
mirror:

```bash
python3 .claude/skills/deepseek-docs-refresh/scripts/refresh.py --cache /tmp/ds-docs
```

`--cache` keeps the fetched HTML so a second run costs no requests, which
matters because working out whether a diff is real usually means running it
more than once. `--fetched YYYY-MM-DD` overrides the date stamp.

The writing run also downloads any image a page references that the mirror does
not have, into `_img/`.

## The property the whole thing rests on

The converter reproduces **unchanged pages byte for byte**. That is what makes
the output readable: if a page reports `changed`, upstream edited it, and the
diff is the edit rather than the converter having a different opinion about
whitespace.

So when a page reports `changed`, open the diff and find the upstream reason. If
there isn't one — the wording is identical and only spacing, table padding or
link form moved — that is **a converter bug, not a docs update**. Fix the
converter and re-run rather than committing the churn. Committing it destroys
the property for everyone after you, because from then on nobody can tell the
two apart.

`api/get-user-balance.md` and `api/list-models.md` are the fixtures for the
schema serialiser specifically: it was built to reproduce those two exactly.
`guides/tool_calls.md` is the fixture for the general converter.

## What it skips, and why

- **`faq.md`** is hand-written. The upstream page is a redirect to an app on
  `static.deepseek.com` and has no content of its own.
- **`prompt-library.md`** renders from `/data/prompts.json`, not from page
  markup, so there is no article body to convert. It and `_data/prompts.json`
  are maintained separately — check the JSON against the live file by hand.
  `prompt-library.en.md` is our translation, not upstream content.
- **The image token calculator** at the foot of `quick_start/token_usage.md` is
  an interactive widget. The converter reproduces its bare labels; `LOCAL_NOTES`
  in `refresh.py` rewrites them into a note, and re-applies it on every run so a
  refresh does not undo it. Add an entry there for any other widget that turns
  up, rather than editing the mirror by hand.

## After a refresh

Update `third_party/deepseek-docs/README.md`: the `Fetched:` date, the page
count, the paragraph describing what this refresh found, and the index lists at
the foot if pages were added.

Then check whether anything the repo asserts has gone stale. Pricing lives in
`configs/prices.json` with its own `captured_at` and is **not** updated by this
script; `quick_start/pricing.md` changing means that file needs re-capturing.
`CLAUDE.md`'s "API facts" section names models and base URLs. `docs/OBSERVED.md`
records findings measured against the live API and overrides the mirror where
they disagree.

## The scripts

| File | What it is |
| --- | --- |
| `refresh.py` | The driver: sitemap, fetch, convert, classify, write |
| `convert.py` | Docusaurus page to Markdown — headings, lists, tables, code fences, tabs, admonitions, link rewriting |
| `apiconv.py` | The `api/` pages, whose schemas are collapsible widgets, serialised into a nested bullet tree |
| `dom.py` | A small read-only DOM over `html.parser`: parent links, search, and exact source slices |

`dom.py` slices the original source rather than re-serialising, so a subtree
handed back to the Markdown converter is the markup that was served. Keep it
that way — re-serialising reintroduces the escaping bugs it exists to avoid.
