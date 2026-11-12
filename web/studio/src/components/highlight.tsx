import type { ReactNode } from "react";

export type Lang = "json" | "sql" | "yaml";

// Small tokenizers that build React nodes (never HTML strings), so database values
// shown in previews cannot inject markup.

function tokens(text: string, re: RegExp, render: (m: RegExpMatchArray, key: number) => ReactNode): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let k = 0;
  for (const m of text.matchAll(re)) {
    const idx = m.index ?? 0;
    if (m[0] === "") continue;
    if (idx > last) out.push(text.slice(last, idx));
    out.push(render(m, k++));
    last = idx + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

const span = (cls: string, text: string, key: number) => (
  <span key={key} className={cls}>
    {text}
  </span>
);

const JSON_RE = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/g;

const SQL_KEYWORDS =
  "SELECT|FROM|JOIN|LEFT|RIGHT|INNER|OUTER|ON|AND|OR|AS|WHERE|GROUP|BY|ORDER|CREATE|INDEX|ALTER|TABLE|DATABASE|SET|ENABLE|GRANT|TO|CURRENT|TOP|DISTINCT|COUNT|NOT|NULL|IS|IN|CHANGE_TRACKING|ALLOW_SNAPSHOT_ISOLATION|CHANGE_RETENTION|AUTO_CLEANUP|DAYS|VIEW|TRACKING|CHANGE";
const SQL_RE = new RegExp(
  `(--[^\\n]*)|('(?:''|[^'])*')|(\\[[^\\]]+\\])|(\\{\\{\\w+\\}\\})|\\b(${SQL_KEYWORDS})\\b`,
  "gi",
);

const YAML_RE = /(#[^\n]*)|^(\s*(?:- )?)([\w.\-]+)(?=:(?:\s|$))|("(?:\\.|[^"\\])*"|'(?:''|[^'])*')|\b(true|false|null)\b/gm;

export function highlight(text: string, lang: Lang): ReactNode[] {
  switch (lang) {
    case "json":
      return tokens(text, JSON_RE, (m, k) => {
        if (m[1] && m[2]) return span("tk-key", m[0], k);
        if (m[1]) return span("tk-str", m[0], k);
        if (m[3]) return span("tk-kw", m[0], k);
        return span("tk-num", m[0], k);
      });
    case "sql":
      return tokens(text, SQL_RE, (m, k) => {
        if (m[1]) return span("tk-comment", m[0], k);
        if (m[2]) return span("tk-str", m[0], k);
        if (m[3]) return span("tk-ident", m[0], k);
        if (m[4]) return span("tk-var", m[0], k);
        return span("tk-kw", m[0], k);
      });
    case "yaml":
      return tokens(text, YAML_RE, (m, k) => {
        if (m[1]) return span("tk-comment", m[0], k);
        if (m[3] !== undefined)
          return (
            <span key={k}>
              {m[2]}
              <span className="tk-key">{m[3]}</span>
            </span>
          );
        if (m[4]) return span("tk-str", m[0], k);
        return span("tk-kw", m[0], k);
      });
  }
}

/** Pretty-prints a JSON string; returns the input unchanged if it does not parse. */
export function pretty(json: string): string {
  try {
    return JSON.stringify(JSON.parse(json), null, 2);
  } catch {
    return json;
  }
}
