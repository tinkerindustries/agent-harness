import hljs from "highlight.js/lib/core";
import bash from "highlight.js/lib/languages/bash";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import go from "highlight.js/lib/languages/go";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import markdown from "highlight.js/lib/languages/markdown";
import python from "highlight.js/lib/languages/python";
import rust from "highlight.js/lib/languages/rust";
import sql from "highlight.js/lib/languages/sql";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";

// Registering a fixed, small language set against highlight.js/lib/core
// (rather than importing the full highlight.js, which bundles ~190
// languages) is what keeps the syntax highlighter's cost proportional to
// what a coding harness's own tool output actually contains
// (docs/CLAUDE.md, "keep frontend dependencies minimal"). Add a language
// here when a tool result shows up in a language this list is missing.
hljs.registerLanguage("bash", bash);
hljs.registerLanguage("shell", bash);
hljs.registerLanguage("javascript", javascript);
hljs.registerLanguage("typescript", typescript);
hljs.registerLanguage("python", python);
hljs.registerLanguage("go", go);
hljs.registerLanguage("json", json);
hljs.registerLanguage("yaml", yaml);
hljs.registerLanguage("xml", xml);
hljs.registerLanguage("html", xml);
hljs.registerLanguage("css", css);
hljs.registerLanguage("diff", diff);
hljs.registerLanguage("markdown", markdown);
hljs.registerLanguage("sql", sql);
hljs.registerLanguage("rust", rust);

export default hljs;

const EXT_LANGUAGE: Record<string, string> = {
  ts: "typescript",
  tsx: "typescript",
  js: "javascript",
  jsx: "javascript",
  mjs: "javascript",
  go: "go",
  py: "python",
  json: "json",
  yml: "yaml",
  yaml: "yaml",
  sh: "bash",
  bash: "bash",
  zsh: "bash",
  md: "markdown",
  css: "css",
  html: "xml",
  htm: "xml",
  xml: "xml",
  sql: "sql",
  rs: "rust",
};

// languageForPath infers a highlight.js language name from a file
// extension, for tool results (Read, Write, Edit) that carry a file_path in
// their originating tool_call's arguments but no language of their own.
export function languageForPath(path: string | undefined): string | undefined {
  if (!path) return undefined;
  const ext = path.split(".").pop()?.toLowerCase();
  return ext ? EXT_LANGUAGE[ext] : undefined;
}
