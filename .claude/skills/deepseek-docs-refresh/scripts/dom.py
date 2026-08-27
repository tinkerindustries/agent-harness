"""A very small read-only DOM over html.parser.

Only what apiconv.py needs: parent links, search by tag/class/attribute, text
extraction, and exact source slices. Slicing the original bytes rather than
re-serialising means a subtree handed back to the Markdown converter is the
markup that was served, character for character.
"""

from html.parser import HTMLParser

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link",
        "meta", "param", "source", "track", "wbr"}


class Node:
    __slots__ = ("tag", "attrs", "children", "parent", "start", "end", "_src")

    def __init__(self, tag, attrs, src):
        self.tag = tag
        self.attrs = dict(attrs)
        self.children = []
        self.parent = None
        self.start = 0
        self.end = 0
        self._src = src

    # -- attributes -------------------------------------------------------
    def get(self, name, default=None):
        value = self.attrs.get(name, default)
        if name == "class" and isinstance(value, str):
            return value.split()
        return value

    def classes(self):
        return (self.attrs.get("class") or "").split()

    # -- navigation -------------------------------------------------------
    @property
    def parents(self):
        cur = self.parent
        while cur is not None:
            yield cur
            cur = cur.parent

    @property
    def summary(self):
        return self.find("summary", recursive=False)

    def find_parent(self, tag=None, class_=None, **attrs):
        for cur in self.parents:
            if cur._matches(tag, class_, attrs):
                return cur
        return None

    def _matches(self, tag, class_, attrs):
        if tag is not None:
            names = (tag,) if isinstance(tag, str) else tuple(tag)
            if self.tag not in names:
                return False
        if class_ is not None and class_ not in self.classes():
            return False
        for k, v in (attrs or {}).items():
            if self.attrs.get(k) != v:
                return False
        return True

    def find_all(self, tag=None, class_=None, recursive=True, **attrs):
        out = []
        for child in self.children:
            if not isinstance(child, Node):
                continue
            if child._matches(tag, class_, attrs):
                out.append(child)
            if recursive:
                out.extend(child.find_all(tag, class_, True, **attrs))
        return out

    def find(self, tag=None, class_=None, recursive=True, **attrs):
        for child in self.children:
            if not isinstance(child, Node):
                continue
            if child._matches(tag, class_, attrs):
                return child
            if recursive:
                hit = child.find(tag, class_, True, **attrs)
                if hit is not None:
                    return hit
        return None

    # -- content ----------------------------------------------------------
    def strings(self):
        for child in self.children:
            if isinstance(child, Node):
                yield from child.strings()
            else:
                yield child

    def get_text(self, sep="", strip=False):
        parts = [p.strip() if strip else p for p in self.strings()]
        if strip:
            parts = [p for p in parts if p]
        return sep.join(parts)

    def decode(self):
        """The exact source of this element, opening and closing tags included."""
        return self._src[self.start:self.end]


class _Builder(HTMLParser):
    def __init__(self, src):
        super().__init__(convert_charrefs=True)
        self.src = src
        self.line_starts = [0]
        for i, ch in enumerate(src):
            if ch == "\n":
                self.line_starts.append(i + 1)
        self.root = Node("[document]", {}, src)
        self.root.end = len(src)
        self.stack = [self.root]

    def _offset(self):
        line, col = self.getpos()
        return self.line_starts[line - 1] + col

    def _tag_end(self, start):
        close = self.src.find(">", start)
        return len(self.src) if close < 0 else close + 1

    def handle_starttag(self, tag, attrs):
        start = self._offset()
        node = Node(tag, attrs, self.src)
        node.start = start
        node.parent = self.stack[-1]
        self.stack[-1].children.append(node)
        if tag in VOID:
            node.end = self._tag_end(start)
        else:
            self.stack.append(node)

    def handle_startendtag(self, tag, attrs):
        start = self._offset()
        node = Node(tag, attrs, self.src)
        node.start = start
        node.end = self._tag_end(start)
        node.parent = self.stack[-1]
        self.stack[-1].children.append(node)

    def handle_endtag(self, tag):
        if tag in VOID:
            return
        end = self._tag_end(self._offset())
        for i in range(len(self.stack) - 1, 0, -1):
            if self.stack[i].tag == tag:
                for node in self.stack[i:]:
                    if not node.end:
                        node.end = end
                del self.stack[i:]
                return

    def handle_data(self, data):
        self.stack[-1].children.append(data)


def parse(src):
    b = _Builder(src)
    b.feed(src)
    b.close()
    for node in b.stack[1:]:
        if not node.end:
            node.end = len(src)
    return b.root
