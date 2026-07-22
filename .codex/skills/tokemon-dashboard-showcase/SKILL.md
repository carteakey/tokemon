---
name: tokemon-dashboard-showcase
description: Capture and package polished screenshots of the Tokemon dashboard for product updates, X posts, design reviews, and demos. Use when asked to take a Tokemon dashboard screenshot, capture the full page, include a Mac window frame, or prepare dashboard visuals for sharing.
---

# Tokemon Dashboard Showcase

## Workflow

Create two useful captures when possible: a clean page-only viewport image and a full-page image that includes the entire dashboard. Save both outside the repository, include the images inline in the final response using absolute paths, and explain which one is best for the user's intended channel.

### 1. Confirm the target

- Prefer the currently running Tokemon dashboard URL when one is supplied or visible. The normal Compose URL is `http://100.89.197.43:18787/` on the private Tailscale network; use localhost only when it is the active target.
- Treat dashboard data as potentially private. Do not upload it, post it, or transmit it to another service unless the user explicitly asks for that exact action.
- Read and follow the in-app browser screenshot guidance before browser work. Use the in-app browser for page captures; use Computer Use only when an actual macOS window or desktop frame is specifically required.

### 2. Capture the page

- Reuse the existing open dashboard tab when available. If no controllable tab exists, open the target URL in a fresh browser tab.
- Inspect the page visually before capturing it. Confirm that the title, creature panel, lifetime token counter, activity grid, and summary tables have rendered.
- Capture the viewport with the browser screenshot API using `fullPage: false`.
- Capture the complete scrolling page with the browser screenshot API using `fullPage: true`.
- Do not add artificial browser chrome to a clean social image. Keep the page-only capture as the primary share artifact.

#### Tighter viewport captures

When the user asks for a tighter composition or a responsive-size capture, use the browser viewport capability instead of guessing CSS crops:

```js
const viewport = await browser.capabilities.get("viewport");
await viewport.set({ width: 1120, height: 760 });
const tightViewport = await tab.screenshot({ fullPage: false });
await viewport.reset();
```

- Use `1120 × 760` for a compact desktop hero, `960 × 760` for a denser desktop view, and `390 × 844` for a phone-sized capture.
- The width tests a real responsive breakpoint; the height controls the visible fold. Re-inspect the page after changing dimensions and wait for the creature, counter, and activity field to be visible before capturing.
- For a precise crop after visual inspection, use `tab.screenshot({ clip: { x, y, width, height } })`; keep enough surrounding context to avoid making the dashboard misleading.
- Always call `viewport.reset()` before finishing unless the user explicitly asks to leave the browser at that size.

### 3. Save and present artifacts

- Save captures under the active Codex visualization artifact directory when available, for example:
  - `tokemon-dashboard-viewport.png`
  - `tokemon-dashboard-full-page.png`
- Keep screenshot artifacts out of the Git worktree unless the user explicitly requests a committed asset.
- Include each requested screenshot inline in the final Markdown with an absolute local path, for example:

  `![Tokemon dashboard](/absolute/path/tokemon-dashboard-full-page.png)`

- Recommend the full-page image for a product or X post when the whole dashboard is the point. Recommend the viewport image when the user wants a tighter composition or plans to add a native Mac window frame.

### 4. Handle native Mac window captures

- Explain that macOS window captures show only the visible viewport, not the complete scrolling page.
- For a native Mac frame, tell the user to press `⌘⇧5` and choose **Capture Selected Window**, or press `⌘⇧4`, press **Space**, and click the browser window.
- If the user explicitly asks Codex to create the native window-framed image, use Computer Use to bring the correct window forward and capture it. Check the result for unrelated apps, private notifications, credentials, or other sensitive content before returning it.
- Clean up temporary browser tabs after capture while leaving a user-facing dashboard tab open only when the user needs it.

## Visual quality checklist

- Prefer the dashboard's native dark instrument-panel styling, visible token odometer, creature art, activity field, and summary cuts.
- Capture after the page has fully rendered; avoid loading states, empty data, browser error pages, and partially visible responsive breakpoints.
- Keep the screenshot honest: do not alter token totals, hide privacy notices, or crop away context that changes what the dashboard communicates.
- For social sharing, pair the image with concise copy describing Tokemon as a local-first token counter and mention metadata-only collection when relevant.
