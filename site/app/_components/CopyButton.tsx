"use client";

import { useState, useSyncExternalStore } from "react";

const noop = () => () => {};
const canCopy = () => typeof navigator !== "undefined" && Boolean(navigator.clipboard?.writeText);

// Progressive enhancement: the button stays hidden in server HTML and only
// appears once the page hydrates in a browser with the async Clipboard API.
export function CopyButton({ text, label }: { text: string; label: string }) {
  const available = useSyncExternalStore(noop, canCopy, () => false);
  const [status, setStatus] = useState<"idle" | "copied" | "failed">("idle");

  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setStatus("copied");
    } catch {
      setStatus("failed");
    }
    window.setTimeout(() => setStatus("idle"), 2000);
  }

  return (
    <>
      <button type="button" className="copyButton" data-status={status} hidden={!available} onClick={copy}>
        <span aria-hidden="true" className="copyGlyph">{status === "copied" ? "✓" : "⧉"}</span>
        {status === "copied" ? "Copied" : status === "failed" ? "Select to copy" : "Copy"}
        <span className="srOnly"> {label}</span>
      </button>
      <span className="srOnly" role="status">{status === "copied" ? `${label} copied to clipboard` : ""}</span>
    </>
  );
}
