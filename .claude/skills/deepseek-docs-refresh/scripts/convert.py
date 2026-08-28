"""Convert a Docusaurus-rendered api-docs.deepseek.com page to Markdown."""
import html as _html
import os
import re
import sys
from html.parser import HTMLParser

VOID = {"br", "hr", "img", "meta", "input", "link", "source", "wbr", "col"}
SKIP = {"script", "style", "svg", "nav", "button"}


def extract_markdown_div(page):
    i = page.find('<div class="theme-doc-markdown markdown">')
    if i < 0:
        raise SystemExit("no markdown div")
    depth, j = 0, i
    for m in re.finditer(r"<(/?)(\w+)([^>]*?)(/?)>", page[i:]):
        tag, closing, selfclose = m.group(2).lower(), m.group(1), m.group(4)
        if tag != "div":
            continue
        if closing:
            depth -= 1
            if depth == 0:
                j = i + m.end()
                break
        elif not selfclose:
            depth += 1
    return page[i:j]


def get(attrs, name):
    for k, v in attrs:
        if k == name:
            return v or ""
    return ""


class Converter(HTMLParser):
    def __init__(self, relbase):
        super().__init__(convert_charrefs=True)
        self.relbase = relbase
        self.out = []          # finished blocks
        self.buf = []          # inline text of the current block
        self.skip = 0
        self.list_stack = []   # ("ul"|"ol", counter)
        self.quote = 0
        self.code_lang = None
        self.in_pre = False
        self.pre_lines = None
        self.table = None
        self.head = []
        self.in_thead = False      # list of rows; row = list of cell strings
        self.row = None
        self.cell = None
        self.cell_span = 1
        self.row_has_td = False
        self.pending_href = []
        self.div_break = 0
        self.block_kind = None
        self.tagstack = []
        self.item_frames = []   # innermost open <li>: {"indent":n,"loose":bool,"pending":bool}

    # ---------- helpers ----------
    def text(self, s):
        if self.in_pre:
            self.pre_lines[-1].append(s)
        elif self.cell is not None:
            self.cell.append(s)
        else:
            self.buf.append(s)

    def flush(self, force_blank=True):
        s = "".join(self.buf)
        self.buf = []
        s = re.sub(r"[ \t]*\n[ \t]*", " ", s)
        s = re.sub(r"[ \t]{2,}", " ", s).strip()
        s = re.sub(r" *\ue003 *", "  \n", s).rstrip()
        if s:
            self.emit(s)
        return s

    def emit(self, block):
        if self.quote and not block.startswith(">"):
            block = "\n".join("> " + ln if ln else ">" for ln in block.split("\n"))
        indent, loose = None, False
        if self.item_frames and self.item_frames[-1]["pending"]:
            f = self.item_frames[-1]
            f["pending"] = False
            indent, loose = f["indent"], f["loose"]
            block = f["marker"] + block
        self.out.append({"text": block, "indent": indent, "loose": loose,
                         "kind": self.block_kind, "div": self.div_break,
                         "quote": self.quote})
        self.block_kind = None

    def rewrite_img(self, src):
        if not src or src.startswith(("http://", "https://", "data:")):
            return src
        return os.path.relpath("_img/" + os.path.basename(src), self.relbase or ".")

    def rewrite(self, href):
        if not href or href.startswith(("http://", "https://", "mailto:", "#")):
            return href
        if not href.startswith("/"):
            return href
        path = href.split("#", 1)
        frag = "#" + path[1] if len(path) > 1 else ""
        p = path[0].strip("/")
        target = "index" if p == "" else p
        return os.path.relpath(target + ".md", self.relbase or ".") + frag

    # ---------- tags ----------
    def handle_starttag(self, tag, attrs):
        cls = get(attrs, "class")
        self._start(tag, attrs, cls)
        if tag not in VOID and not self.skip:
            self.tagstack.append(tag)

    def _start(self, tag, attrs, cls):
        if self.skip:
            if tag not in VOID:
                self.skip += 1
            return
        if tag in SKIP or "hash-link" in cls or "theme-doc-breadcrumbs" in cls \
                or "theme-doc-version-badge" in cls:
            self.skip = 1
            return

        if tag == "div":
            self.div_break += 1
        if tag == "div" and "codeBlockContainer" in cls:
            m = re.search(r"language-([\w-]+)", cls)
            self.code_lang = m.group(1) if m else ""
            return
        if tag == "pre":
            if not self.code_lang:
                m = re.search(r"language-([\w-]+)", cls)
                self.code_lang = m.group(1) if m else ""
            self.flush()
            self.in_pre = True
            self.pre_lines = [[]]
            return
        if self.in_pre:
            if tag == "br":
                self.pre_lines.append([])
            return
        if tag == "br" and self.cell is not None:
            self.cell.append(" ")
            return
        if tag == "thead":
            self.in_thead = True
            return

        if tag in ("h1", "h2", "h3", "h4", "h5", "h6"):
            self.flush()
            self.buf.append("\ue000H%s\ue000" % tag[1])
        elif tag == "p":
            if not (self.tagstack and self.tagstack[-1] in ("h1", "h2", "h3", "h4", "h5", "h6")
                    or "a" in self.tagstack):
                self.flush()
            if self.item_frames and self.tagstack and self.tagstack[-1] == "li":
                self.item_frames[-1]["loose"] = True
        elif tag == "hr":
            self.flush()
            self.emit("---")
        elif tag == "br":
            if self.tagstack and self.tagstack[-1] in ("p", "a", "li", "td", "th", "strong", "b", "em", "i", "code", "span"):
                self.text("\ue003")
            else:
                self.flush()
                self.emit("  ")
        elif tag in ("strong", "b"):
            self.text("**")
        elif tag in ("em", "i"):
            self.text("*")
        elif tag == "code":
            self.text("\ue002")
        elif tag == "a":
            self.pending_href.append(self.rewrite(get(attrs, "href")))
            self.text("\ue001")
        elif tag == "img":
            md = "![%s](%s)" % (get(attrs, "alt"), self.rewrite_img(get(attrs, "src")))
            if self.tagstack and self.tagstack[-1] in ("p", "a", "li", "td", "th", "strong", "b", "em", "i", "code", "span"):
                self.text(md)
            else:
                self.flush()
                self.block_kind = "img"
                self.emit(md)
        elif tag == "blockquote":
            self.flush()
            self.quote += 1
        elif tag in ("ul", "ol"):
            self.flush()
            try:
                start = int(get(attrs, "start")) - 1
            except ValueError:
                start = 0
            self.list_stack.append([tag, start])
        elif tag == "li":
            self.flush()
            kind, n = self.list_stack[-1] if self.list_stack else ("ul", 0)
            if self.list_stack:
                self.list_stack[-1][1] += 1
                n += 1
            depth = max(len(self.list_stack) - 1, 0)
            marker = "  " * depth + ("- " if kind == "ul" else "%d. " % n)
            self.item_frames.append(
                {"indent": depth, "loose": False, "pending": True, "marker": marker})
        elif tag == "table":
            self.flush()
            self.table = []
            self.head = []
        elif tag == "tr" and self.table is not None:
            self.row = []
            self.row_has_td = False
        elif tag in ("td", "th") and self.row is not None:
            if tag == "td":
                self.row_has_td = True
            self.cell = []
            self.cell_span = int(get(attrs, "colspan") or 1)

    def handle_endtag(self, tag):
        if not self.skip and self.tagstack and tag in self.tagstack:
            while self.tagstack and self.tagstack.pop() != tag:
                pass
        self._end(tag)

    def _end(self, tag):
        if self.skip:
            self.skip -= 1
            return

        if tag == "pre":
            self.in_pre = False
            lines = ["".join(p) for p in self.pre_lines]
            while lines and not lines[-1].strip():
                lines.pop()
            body = "\n".join(lines)
            runs = [len(r) for r in re.findall(r"`+", body)]
            fence = "`" * max(3, (max(runs) + 1) if runs else 3)
            self.emit("%s%s\n%s\n%s" % (fence, self.code_lang or "", body, fence))
            self.pre_lines = None
            return
        if self.in_pre:
            return
        if tag == "div":
            self.div_break += 1
        if tag == "div" and self.code_lang is not None:
            self.code_lang = None
            return

        if tag in ("h1", "h2", "h3", "h4", "h5", "h6"):
            self.flush()
        elif tag == "p":
            if not (self.tagstack and self.tagstack[-1] in ("h1", "h2", "h3", "h4", "h5", "h6")
                    or "a" in self.tagstack):
                self.flush()
        elif tag in ("strong", "b"):
            self.text("**")
        elif tag in ("em", "i"):
            self.text("*")
        elif tag == "code":
            target = self.cell if self.cell is not None else self.buf
            joined = "".join(target)
            k = joined.rfind("\ue002")
            body = joined[k + 1:]
            runs = [len(r) for r in re.findall(r"`+", body)]
            n = max(runs) + 1 if runs else 1
            pad = " " if runs else ""
            target[:] = [joined[:k] + "`" * n + pad + body + pad + "`" * n]
        elif tag == "a":
            href = self.pending_href.pop() if self.pending_href else ""
            target = self.cell if self.cell is not None else self.buf
            joined = "".join(target)
            k = joined.rfind("\ue001")
            label = joined[k + 1:]
            head = joined[:k]
            label_clean = label.strip()
            if label_clean and href:
                link = ("<%s>" % href) if label_clean.strip("`") == href else "[%s](%s)" % (label_clean, href)
            else:
                link = label_clean
            target[:] = [head + link]
        elif tag == "blockquote":
            self.flush()
            self.quote -= 1
        elif tag in ("ul", "ol"):
            self.flush()
            if self.list_stack:
                self.list_stack.pop()
        elif tag == "li":
            self.flush()
            if self.item_frames:
                self.item_frames.pop()
        elif tag in ("td", "th") and self.cell is not None:
            s = "".join(self.cell).replace("\n", " ").strip()
            self.row.append(s)
            for _ in range(self.cell_span - 1):
                self.row.append("")
            self.cell = None
        elif tag == "thead":
            self.in_thead = False
        elif tag == "tr" and self.row is not None:
            header = self.in_thead or (not self.row_has_td and not self.table and not self.head)
            (self.head if header else self.table).append(self.row)
            self.row = None
        elif tag == "table" and self.table is not None:
            width = max((len(r) for r in self.table + self.head), default=0)
            if self.head:
                lines = ["| " + " | ".join(self.head[0]) + " |"]
            else:
                lines = ["|" + "".join("  |" for _ in range(width))]
            lines.append("| " + " | ".join(["---"] * width) + " |")
            for r in self.table:
                lines.append("|" + "".join((" %s |" % c) if c else " |" for c in r))
            self.emit("\n".join(lines))
            self.table = None
            self.head = []

    def handle_data(self, data):
        if self.skip:
            return
        data = re.sub(r"[\x00-\x08\x0b\x0c\x0e-\x1f\ue000-\ue002\ue004-\ue00f]", "", data)
        if not data or data == "​":
            return
        self.text(data)

    # ---------- render ----------
    def render(self):
        self.flush()
        self.out = [b for b in self.out if b["text"].strip("*\ue000\ue001\ue002")]
        for b in self.out:
            m = re.match(r"(\s*(?:- |\d+\. )?)\ue000H(\d)\ue000", b["text"])
            if m:
                b["text"] = m.group(1) + "#" * int(m.group(2)) + " " + b["text"][m.end():].strip()
        parts = []
        for i, b in enumerate(self.out):
            if i:
                a = self.out[i - 1]
                if a["quote"] and b["quote"]:
                    sep = "\n>\n"
                elif a["kind"] == "img" and b["kind"] == "img":
                    sep = "\n" if a["div"] == b["div"] else "\n\n"
                elif a["indent"] is not None and b["indent"] is not None:
                    sep = "\n\n" if (b["indent"] > a["indent"] and a["loose"]) else "\n"
                else:
                    sep = "\n\n"
                parts.append(sep)
            parts.append(b["text"])
        return "".join(parts).strip() + "\n"


TABLIST = re.compile(r'<ul role="tablist".*?</ul>', re.S)


def label_tabpanels(body):
    """Replace each tablist with per-panel <strong> labels, as the mirror does."""
    out, pos = [], 0
    for m in TABLIST.finditer(body):
        labels = [re.sub(r"<[^>]+>", "", x) for x in
                  re.findall(r'<li role="tab"[^>]*>(.*?)</li>', m.group(0), re.S)]
        out.append(body[pos:m.start()])
        pos = m.end()
        rest = body[pos:]
        for lab in labels:
            k = rest.find('<div role="tabpanel"')
            if k < 0:
                break
            end = rest.index(">", k) + 1
            out.append(rest[:end] + "<p><strong>%s</strong></p>" % lab.strip())
            rest = rest[end:]
        body = rest
        pos = 0
    out.append(body[pos:])
    return "".join(out)


def convert(page, url, title, fetched):
    body = label_tabpanels(extract_markdown_div(page))
    relbase = os.path.dirname(url.split("api-docs.deepseek.com/", 1)[1].strip("/"))
    c = Converter(relbase)
    c.feed(body)
    md = c.render()
    md = re.sub(r"[\ue000-\ue002]", "", md)
    md = re.sub(r"\n{3,}", "\n\n", md)
    fm = "---\ntitle: %s\nsource: %s\nfetched: %s\n---\n\n" % (title, url, fetched)
    return fm + md


if __name__ == "__main__":
    path, url, title, fetched = sys.argv[1:5]
    page = open(path, encoding="utf-8").read()
    sys.stdout.write(convert(page, url, title, fetched))
