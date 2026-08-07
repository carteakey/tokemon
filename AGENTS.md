# Tokemon Repository Instructions

## Product guardrails

- Treat `docs/tokemon-spec-v0.2.md` as authoritative; `docs/token-bandit-spec.md` is historical context.
- Store usage metadata only. Do not collect prompts, responses, source code, repository contents, or conversation titles by default.
- Prefer the existing Go, SQLite, server-rendered HTML, and deterministic evolution architecture.
- Keep provider quirks inside adapters and preserve unknown values as unknown.

## Delivery rule

Use the `ship` loop after every implementation change:

```text
change → test → deploy → verify
```

Do not report a change complete after tests alone. Deploy the affected Tokemon service to the active target, then verify its health and the changed behavior. For server or dashboard changes, deploy the primary Mac hub and verify its Tailscale URL; for agent changes, redeploy each affected enrolled machine. Keep the deployment scoped to the changed component and preserve persistent data.

## Luna worker orchestration

Use Sol for planning, orchestration, and reviews.

Use Luna for implementation and routine investigation. When spawning Luna, use `model="gpt-5.6-luna"` and `fork_turns="none"`.

When a task can be split into several independent and substantial subtasks, spawn one `luna_worker` per subtask and run them in parallel. Keep anything that takes only a few minutes in the main thread.

Each worker runs in its own thread. Do not assume it can see the main thread conversation. Make every task description self-contained: files in scope, boundaries, and expected output.

Independent read-only tasks can run in parallel. Any worker that writes files needs its own isolated worktree. Otherwise, run those workers sequentially.

After all workers finish, the main thread checks each result against the acceptance criteria in the dispatch prompt before combining them. Re-dispatch anything that fails.

If you dispatched multiple workers but only one ever runs, first check whether `agents.max_concurrent_threads_per_session` is set to `1` in `config.toml`.

If Codex multi-agent rejects `luna_worker` because Luna is unavailable as a subagent, use a separate Codex task as the fallback. Create it with `gpt-5.6-luna`, Max reasoning, and an isolated worktree for any file-writing task. Give it the same self-contained scope and acceptance criteria, and ask it to create its own explicit goal for persistent work. Monitor and message that task from the main thread, then review its result before integrating it. Do not use this fallback when `luna_worker` launches normally.
