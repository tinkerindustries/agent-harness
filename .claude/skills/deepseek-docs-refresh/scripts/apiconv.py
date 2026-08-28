"""Serialise an api-docs.deepseek.com OpenAPI page into the mirror's curated style."""
import os
import re
import convert as C
import dom


def inline(node, relbase):
    """Render a node's children as inline Markdown."""
    if node is None:
        return ""
    conv = C.Converter(relbase)
    conv.feed(node.decode())
    text = conv.render().strip()
    text = re.sub(r"^#+\s*", "", text)
    return re.sub(r"\s*\n\s*", " ", text).strip()


def paragraphs(node, relbase):
    return [inline(p, relbase) for p in node.find_all("p", recursive=False) if p.get_text(strip=True)]


def join_desc(parts):
    out = ""
    for p in parts:
        if not p:
            continue
        if out:
            out += " " if out[-1] in ".!?:" else ". "
        out += p
    return out


def qualifiers(container):
    q = []
    for span in container.find_all("span", recursive=False):
        cls = " ".join(span.classes())
        if "openapi-schema__required" in cls or "openapi-schema__nullable" in cls \
                or "openapi-schema__deprecated" in cls:
            t = span.get_text(strip=True)
            if t:
                q.append(t)
    return q


QUALIFIER = re.compile(r"^(required|nullable|deprecated)$", re.I)
DECORATION = {"Array [", "]", "oneOf", "anyOf", "allOf", "{", "}"}


def item_header(item):
    """The element carrying this item's own name/type, in either markup shape."""
    container = item.find("span", class_="openapi-schema__container")
    if container is not None and belongs(container, item):
        return container
    det = item.find("details", recursive=False)
    if det is not None and det.summary is not None:
        return det.summary
    return None


def header_fields(header):
    """(name, type, [qualifiers]) from a container span or a details summary."""
    if header is None:
        return "", "", []
    name_el = header.find("strong", class_="openapi-schema__property")
    type_el = header.find("span", class_="openapi-schema__name")
    if name_el is not None or type_el is not None:
        name = name_el.get_text(" ", strip=True) if name_el else ""
        typ = type_el.get_text(" ", strip=True) if type_el else ""
        return name, typ, qualifiers(header)

    name, typ, quals = "", "", []
    for child in header.find_all(["strong", "span"], recursive=False):
        text = child.get_text(" ", strip=True)
        if not text:
            continue
        if QUALIFIER.match(text):
            quals.append(text.lower())
        elif not name:
            name = text
        elif not typ:
            typ = text
    return name, typ, quals


def descriptions(item, relbase):
    """Paragraphs describing `item` itself, in document order."""
    header = item_header(item)
    parts = []
    for p in item.find_all("p"):
        if not belongs(p, item):
            continue
        if header is not None and (p is header or header in p.parents):
            continue
        if p.find_parent("span", class_="badge") is not None:
            continue
        if p.find_parent("span", class_="openapi-schema__container") is not None:
            continue
        text = inline(p, relbase)
        if not text or text in DECORATION:
            continue
        text = re.sub(r"\*\*(Possible values|Default value|Example):\*\*", r"\1:", text)
        if text.startswith(("Possible values:", "Default value:")):
            text = re.sub(r"`([^`]*)`", r"\1", text)
        parts.append(text)
    return parts


def belongs(node, scope):
    """True when `scope` is the first schema item or tab panel above `node`."""
    cur = node.parent
    while cur is not None:
        if cur is scope:
            return True
        if cur.get("role") == "tabpanel" or "openapi-schema__list-item" in cur.classes():
            return False
        cur = cur.parent
    return False


def render_scope(scope, relbase, depth):
    """Variant groups and schema items sitting directly inside `scope`."""
    lines = []
    for label, panel in variant_groups(scope):
        body = render_scope(panel, relbase, depth + 1)
        head = "  " * depth + "- " + label
        if not body:
            bare = [inline(p, relbase) for p in panel.find_all("p") if belongs(p, panel)]
            bare = [b for b in bare if b and b not in DECORATION]
            if bare:
                head += " — " + join_desc(bare)
        lines.append(head)
        lines += body
    for child in scope.find_all("div", class_="openapi-schema__list-item"):
        if belongs(child, scope):
            lines += schema_item(child, relbase, depth)
    return lines


def schema_item(item, relbase, depth):
    name, typ, quals = header_fields(item_header(item))
    bits = []
    if name:
        bits.append("`%s`" % name)
    if typ:
        bits.append("(%s)" % typ)
    bits += ["**%s**" % q for q in quals]

    line = "  " * depth + "- " + " ".join(bits)
    desc = join_desc(descriptions(item, relbase))
    if desc:
        line += (" — " if bits else "") + desc
    return [line] + render_scope(item, relbase, depth + 1)


def owns(node, ancestor):
    """True when `ancestor` is the nearest schema-item above `node`, with no
    variant tab panel in between (panel contents belong to the variant)."""
    cur = node.parent
    while cur is not None and cur is not ancestor:
        if cur.get("role") == "tabpanel":
            return False
        cls = cur.get("class") or []
        if "openapi-schema__list-item" in cls:
            return False
        cur = cur.parent
    return cur is ancestor


def direct_items(node):
    return [d for d in node.find_all("div", class_="openapi-schema__list-item")
            if owns(d, node)]


def schema_tree(root, relbase):
    return render_scope(root, relbase, 0)


# ---------------------------------------------------------------- variants

def top_panels(holder):
    """Tab panels directly under `holder`, ignoring panels nested inside them."""
    out = []
    for p in holder.find_all("div", role="tabpanel"):
        cur = p.parent
        while cur is not None and cur is not holder and cur.get("role") != "tabpanel":
            cur = cur.parent
        if cur is holder:
            out.append(p)
    return out


def variant_groups(item):
    """oneOf/anyOf/allOf groups belonging directly to `item`."""
    groups = []
    for badge in item.find_all("span", class_="badge"):
        kind = badge.get_text(" ", strip=True)
        if kind not in ("oneOf", "anyOf", "allOf") or not belongs(badge, item):
            continue
        holder = badge.parent
        tablist = holder.find("ul", role="tablist")
        if tablist is None:
            continue
        labels = [x.get_text(" ", strip=True)
                  for x in tablist.find_all("span", class_="openapi-tabs__schema-label")]
        panels = top_panels(holder)
        for label, panel in zip(labels, panels):
            groups.append(("*%s* — %s" % (kind, label), panel))
    return groups


def panel_items(panel):
    return [d for d in panel.find_all("div", class_="openapi-schema__list-item")
            if d.find_parent("div", class_="openapi-schema__list-item") is None]


# ---------------------------------------------------------------- page

def code_block(node, lang="json"):
    pre = node.find("pre")
    if pre is None:
        return ""
    conv = C.Converter("")
    conv.code_lang = lang
    conv.feed(pre.decode())
    return conv.render().strip()


def pick_example(container):
    """The concrete Example tab, falling back to Example (from schema)."""
    labels = [x.get_text(" ", strip=True) for x in container.find_all("span", class_="openapi-tabs__schema-label")]
    panels = container.find_all("div", role="tabpanel")
    by_label = dict(zip(labels, panels))
    for want in ("Example", "Example (from schema)"):
        if want in by_label:
            block = code_block(by_label[want])
            if block:
                return block
    return ""


def schema_panel_lines(container, relbase):
    labels = [x.get_text(" ", strip=True) for x in container.find_all("span", class_="openapi-tabs__schema-label")]
    panels = container.find_all("div", role="tabpanel")
    for label, panel in zip(labels, panels):
        if label == "Schema":
            return render_scope(panel, relbase, 0)
    return []


def render_page(page, url, title, fetched):
    body = C.extract_markdown_div(page)
    relbase = os.path.dirname(url.split("api-docs.deepseek.com/", 1)[1].strip("/"))
    soup = dom.parse(body)
    left = soup.find("div", class_="openapi-left-panel__container")
    out = ["# " + title, ""]

    endpoint = left.find("pre", class_="openapi__method-endpoint")
    if endpoint:
        method = endpoint.find("span", class_="badge").get_text(strip=True)
        path = endpoint.find("h2").get_text(strip=True)
        out += ["```", "%s %s" % (method, path), "```", ""]

    for p in left.find_all("p", recursive=False):
        t = inline(p, relbase)
        if t:
            out += [t, ""]

    req = None
    for h in left.find_all("h2"):
        if h.get_text(strip=True).replace("​", "") == "Request":
            req = h
            break
    if req is not None:
        out += ["## Request", ""]
        tabs = None
        siblings = [c for c in req.parent.children if isinstance(c, dom.Node)]
        for i, node in enumerate(siblings):
            if node is req:
                tabs = next((s for s in siblings[i + 1:] if s.tag == "div"), None)
                break
        if tabs is None:
            tabs = req.parent
        mime = tabs.find("div", class_="openapi-tabs__mime-container")
        if mime:
            out += ["**%s**" % mime.get_text(" ", strip=True), ""]
        body_details = [d for d in tabs.find_all("details", class_="openapi-markdown__details")
                        if d.find_parent("div", class_="openapi-schema__list-item") is None]
        for det in body_details:
            summary = det.summary.get_text(" ", strip=True)
            head = summary.replace("required", "").strip() or "Body"
            out += ["### " + head, ""]
            if "required" in summary:
                out += ["**required**", ""]
            lines = render_scope(det, relbase, 0)
            if lines:
                out += lines + [""]

    resp = left.find("div", class_="openapi-tabs__response-container")
    if resp is not None:
        out += ["## Responses", ""]
        codes = [x.get_text(" ", strip=True) for x in resp.find_all("li", class_="tabs__item")]
        panels = resp.find_parent("div", class_="openapi-tabs__container").find_all(
            "div", role="tabpanel", recursive=True)
        panels = [p for p in panels
                  if p.find_parent("div", class_="openapi-tabs__schema-container") is None
                  and p.find_parent("div", role="tabpanel") is None]
        for code, panel in zip(codes, panels):
            out += ["### " + code, ""]
            for p in panel.find_all("p"):
                if p.find_parent("details") or p.find_parent("div", class_="openapi-schema__list-item"):
                    continue
                t = inline(p, relbase)
                if t:
                    out += [t, ""]
                break
            container = panel.find("div", class_="openapi-tabs__schema-container")
            if container is None:
                continue
            lines = schema_panel_lines(container, relbase)
            if lines:
                out += ["**Schema**", ""] + lines + [""]
            ex = pick_example(container)
            if ex:
                out += ["**Example**", "", ex, ""]

    md = "\n".join(out).rstrip() + "\n"
    md = re.sub(r"\n{3,}", "\n\n", md)
    fm = "---\ntitle: %s\nsource: %s\nfetched: %s\n---\n\n" % (title, url, fetched)
    return fm + md
