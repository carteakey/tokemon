# Data audit — 2026-11-19

Compared the hub's normalized `usage_events` with the source stores on every
reachable enrolled machine after the `session_v2` (OpenCode) and `steps`
(Antigravity) fixes.

Scope: `Kartikeys-MacBook-Air` (hub), `Kartikeys-MacBook-Air-2`, `kpc-cachy`,
`fx505`. `raspberrypi` is offline (last seen 2026-10-18). Every machine runs
agent `0.4.5-dev` at `9f5569c`.

## Result

| Adapter | Machine | Source | Stored | Verdict |
| --- | --- | --- | --- | --- |
| OpenCode | hub | 43 sessions / 677.2M | 43 / 680.6M | match (live session moved on) |
| OpenCode | MacBook-Air-2 | 15 / 1,035,666,688 | 15 / 1,035,666,688 | exact |
| OpenCode | kpc-cachy | 51 / 978,049,311 | 51 / 978,049,311 | exact |
| OpenCode | fx505 | 5 / 538,113,134 | 5 / 538,113,134 | exact |
| ZCode | hub | 18 groups / 918,268,574 | 18 / 918,268,574 | exact |
| ZCode | MacBook-Air-2 | 33 / 1,471,314,233 | 33 / 1,471,314,233 | exact |
| ZCode | kpc-cachy | 44 / 1,737,747,145 | 44 / 1,737,747,145 | exact |
| ZCode | fx505 | 48 / 968,250,697 | 48 / 968,250,697 | exact |
| Claude Code | MacBook-Air-2 | 48 sessions / 15,731 / 5,093,150,617 | 48 / 15,731 / 5,093,150,617 | exact |
| Claude Code | kpc-cachy | 19 / 2,985 / 449,006,470 | 19 / 2,985 / 449,006,470 | exact |
| Claude Code | fx505 | 2 / 105 / 9,158,731 | 2 / 105 / 9,158,731 | exact |
| Antigravity | hub | 45 conversations / 4,917 steps / 41.5M | 43 sessions / 4,880 / 43.5M | all files matched; +5.0% historical |
| Codex | hub | 531 threads / 7.174B | 529 / 6.879B | −4.1% |
| Codex | MacBook-Air-2 | 212 / 1.359B | 212 / 0.938B | −31.0% |
| Codex | kpc-cachy | 116 / 2.259B | 116 / 2.279B | +0.9% |

Counts are events except OpenCode (sessions), ZCode (session/model groups),
Antigravity (conversations/steps), and Codex (threads).

## Findings

1. **OpenCode, ZCode, Claude Code are exact.** Every current source session
   is present once with the same token total. Claude Code on the other
   machines also matches exactly, including sessions whose transcripts have
   since been deleted (stored history is retained).
2. **Claude Desktop coding sessions are already covered.** On
   MacBook-Air-2, `~/.claude/projects` holds 24,121 records with
   `"entrypoint":"claude-desktop"` and 9,712 with `"entrypoint":"cli"`. The
   Claude Code adapter reads that same store, so Desktop coding usage is in
   the numbers above. The adapter does not currently label Desktop events as
   a separate harness.
3. **Antigravity: current data is complete; older events over-count output.**
   All 45 hub conversations matched. The legacy decoder used the
   conversation's constant max-output setting as the output count, so
   pre-migration events read ~5% high (2.06M tokens on the hub, 0.16% of
   lifetime). New `steps`-based events use the real output counter.
4. **Codex is under-counted against Codex's own thread totals.** The adapter
   derives events from rollout JSONL deltas. MacBook-Air-2 has 27
   `archived_sessions/*.jsonl.zst` files (and 11 live `.jsonl`), which the
   adapter skips, so 421M of 1.359B thread tokens are absent. The same
   pattern explains the hub's 4.1% gap. kpc-cachy is within +0.9% because its
   live JSONL still covers its threads.
5. **One duplicated Claude session, now fixed.** MacBook-Air-2 session
   `e66915b4-…` had each of its 74 usage records stored twice (148 rows,
   10.44M tokens). The duplicate copy was removed; the session now matches
   the transcript exactly.
6. **Backups were stale.** The newest snapshot was 2026-08-12. A fresh
   verified snapshot was written to `data/backups` (177,352 events, 28.63B
   tokens). `PRAGMA integrity_check` is `ok`.

## Follow-ups

- Codex: read `.jsonl.zst` rollouts (or top up each thread to
  `threads.tokens_used` when parsed deltas are lower). This is the only
  material collection gap left.
- Optional: label `entrypoint=claude-desktop` events as their own harness so
  the dashboard separates Desktop from the CLI.
- Antigravity: optionally refresh legacy UUID-session events with the real
  output counter; not worth a migration for 0.16% of lifetime tokens.
- raspberrypi: update the agent when it comes back online.
