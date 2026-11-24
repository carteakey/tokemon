# Codex token accounting

Tokemon has two independent token totals for a Codex thread:

| Source | Meaning | Where |
| --- | --- | --- |
| `threads.tokens_used` | Codex's per-thread summary counter | `~/.codex/state_*.sqlite` |
| `total_token_usage.total_tokens` | The cumulative total Codex writes on each rollout `token_count` record | rollout JSONL / `.jsonl.zst` |

The second value is the authoritative per-call report. Tokemon turns each
rollout's cumulative total into a delta, stores one `usage_events` row per
delta, and uses `total_tokens` on that row. Summing those rows is the
"total tokens utilized" count.

## Why the two disagree

Rollouts are archived and sometimes deleted before their full usage is parsed.
`tokens_used` survives in the state database and can therefore be larger than
the sum of the parseable rollout deltas. Conversely, some threads report more
detailed usage than `tokens_used` (stale counters, forked or auxiliary turns).

Neither source is a global upper bound:

- Detailed-only loses threads whose rollout is gone.
- `tokens_used`-only loses the per-call detail and inherits stale counters.

## Policy: per-thread maximum

Lifetime usage is the sum, across threads, of:

```text
max(sum(detailed rollout deltas), threads.tokens_used)
```

When detailed totals are lower, Tokemon stores the gap as an aggregate
"residual" event:

```text
residual = max(0, tokens_used - detailed_total)
```

- A new `codex_thread_totals` table (schema v6) stores `tokens_used` per
  machine and thread.
- The thread snapshot event has no component fields; its `total_tokens` is the
  residual.
- A detailed event refreshes the residual for its thread.
- A thread fully covered by detailed events drops its aggregate row.
- Agents re-emit thread snapshots whenever the Codex state database changes,
  so new threads and changed `tokens_used` values are picked up without a full
  rollout reparse.

This never discards observed detailed usage, and it never double-counts the
gap.

## Consequences

- Lifetime can exceed `sum(threads.tokens_used)`. The detailed rollouts
  recorded more usage than the thread counter for some threads.
- Lifetime can exceed the detailed-only sum. The residual fills threads whose
  rollout files are missing.
- A residual is not a duplicate: it is only the positive difference between
  the thread counter and the detailed total.

At the 2026-11-24 deployment:

| Machine | Detailed | `tokens_used` | Stored (max) | Residual rows |
| --- | --- | --- | --- | --- |
| hub | 6.801B | 7.174B | 8.018B | 42 |
| m2 | 0.938B | 1.359B | 1.421B | 29 |
| kpc | 2.473B | 2.259B | 2.506B | 26 |
| fx505 | 0.054B | 0.054B | 0.054B | 0 |

## Rejected alternative: detailed-only

Summing only rollout deltas gives the hub 6.801B, m2 0.938B, kpc 2.473B. It is
appropriate only if missing-rollout usage should be discarded. Switching to
it means removing the residual ingest path, the `codex_thread_totals` table,
and the extra thread-snapshot discovery.

## Verification

```sh
# source totals
sqlite3 ~/.codex/state_5.sqlite \
  "select count(*), sum(tokens_used) from threads where tokens_used>0;"

# stored Codex total and residuals
sqlite3 data/tokemon.db "
select machine_id, sum(total_tokens) from usage_events
where adapter='codex' group by machine_id;
select count(*), sum(total_tokens) from usage_events
where adapter='codex' and input_tokens is null and output_tokens is null
  and cache_read_tokens is null and cache_write_tokens is null;"
```
