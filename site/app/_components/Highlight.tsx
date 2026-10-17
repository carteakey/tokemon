import type { ReactNode } from "react";

// Tiny server-side highlighters for the static code samples on the page.
const JSON_TOKEN = /("(?:\\.|[^"\\])*")(\s*:)?|(-?\d+(?:\.\d+)?)|\b(null|true|false)\b/g;

export function JsonBlock({ value }: { value: unknown }) {
  const source = JSON.stringify(value, null, 2);
  const out: ReactNode[] = [];
  let last = 0;
  let key = 0;
  for (const match of source.matchAll(JSON_TOKEN)) {
    const index = match.index ?? 0;
    if (index > last) out.push(source.slice(last, index));
    if (match[1] && match[2]) {
      out.push(<span className="tkKey" key={key++}>{match[1]}</span>, match[2]);
    } else if (match[1]) {
      out.push(<span className="tkStr" key={key++}>{match[1]}</span>);
    } else if (match[3]) {
      out.push(<span className="tkNum" key={key++}>{match[3]}</span>);
    } else {
      out.push(<span className="tkNull" key={key++}>{match[4]}</span>);
    }
    last = index + match[0].length;
  }
  out.push(source.slice(last));
  return <code>{out}</code>;
}

const FLAG = /(\s--[a-z][a-z-]*)/g;

// Renders shell commands with a "$" prompt on each new command (not on
// continuation lines or heredoc bodies) and highlighted --flags.
export function ShellBlock({ source }: { source: string }) {
  const lines: ReactNode[] = [];
  let continued = false;
  let heredoc: string | null = null;
  source.split("\n").forEach((line, i) => {
    let body: ReactNode;
    if (heredoc) {
      body = <span className="tkHere">{line}</span>;
      if (line.trim() === heredoc) heredoc = null;
    } else {
      const parts = line.split(FLAG).map((part, j) => (j % 2 ? <span className="tkFlag" key={j}>{part}</span> : part));
      const prompt = line.trim() && !continued ? <b aria-hidden="true">$ </b> : null;
      body = <>{prompt}{parts}</>;
      heredoc = line.match(/<<'(\w+)'/)?.[1] ?? null;
    }
    continued = line.endsWith("\\");
    lines.push(<span key={i}>{body}{"\n"}</span>);
  });
  return <code>{lines}</code>;
}

const YAML_LINE = /^(\s*)([\w.-]+)(:)(\s*)(.*)$/;

export function YamlBlock({ source }: { source: string }) {
  return (
    <code>
      {source.split("\n").map((line, i) => {
        const match = line.match(YAML_LINE);
        if (!match) return <span key={i}>{line}{"\n"}</span>;
        const [, indent, name, colon, gap, rest] = match;
        const valueClass = /^-?\d/.test(rest) ? "tkNum" : rest.startsWith("[") ? "tkList" : "tkStr";
        return (
          <span key={i}>
            {indent}<span className="tkKey">{name}</span>{colon}{gap}
            {rest ? <span className={valueClass}>{rest}</span> : null}
            {"\n"}
          </span>
        );
      })}
    </code>
  );
}
