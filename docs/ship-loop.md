# The Tokemon ship loop

How this repository is worked when more than one agent (or agent + human) is
shipping at once. It is the operating pattern behind the 2026-11 batch: one
coordinator, a small worker pool, and a loop that never lets a finished task
sit unlanded.

The product rules are in `AGENTS.md` and the spec. This file is about *how
work moves*.

## Roles

**Supervisor (one).** Owns the gate, the push, the task board, and the queue.
Does not implement product code. Keeps the loop turning: review fast, push
fast, reassign stalled work fast.

**Workers (one or more).** Implement one scoped task at a time. Never push.
Never assign themselves work. Report the commit hash and how they verified it.

The rule that matters most: **workers do not push, and the supervisor does not
implement.** Mixing the two is what creates half-finished branches.

## The loop

1. **Assign** a durable task with a brief, a scope, and a gate definition.
2. **Design note first** for schema, concurrency, auth, or multi-file behavior
   changes. Review the design before code exists.
3. **Implement in a dedicated worktree.** One logical change per commit. No
   push. Post the hash plus what was verified.
4. **Gate in isolation** at the exact commit (see below). Review the diff.
   Reproduce the claimed fix; do not trust "tests are green".
5. **Push, then update the board.** A task is not done until it is on `main`.
6. **Assign the next task immediately.** A worker should never finish into an
   empty queue.
7. **Nudge once, then reassign.** A silent worker must not hold the critical
   path.

## The gate

Run in a throwaway worktree at the exact commit being gated:

```sh
gofmt -l .            # must be empty
go build ./...
go vet ./...
go test ./... -count=1
```

Then, for the affected surface:

- `go test -race` on the packages with concurrency changes.
- `go run ./cmd/tokemon catalog validate --catalog catalog/models.yaml` for
  catalog changes.
- `TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -count=1` for
  dashboard changes.
- `bash deploy/release-check.sh` before any tag (see `AGENTS.md`).

Gate the commit, not the working tree: a worker's tree may contain the next
task's uncommitted changes. Never weaken an existing gate to make a commit
pass.

## Review: reproduce the claim

The highest-leverage supervisor activity is not reading code — it is making
the claimed bug happen against the old commit and showing it is gone at the
new one. This is how the partial-record cursor loss, the CSV formula
injection, the connector-down day-total drop, and the `context canceled` 500s
were caught.

## Worktrees

One worktree per worker, always. Never switch the shared checkout's branch —
it strands other workers' uncommitted commits. The supervisor also uses a
throwaway worktree per gate so gating never disturbs anyone's tree.

## Stalls

| Symptom | Action |
| --- | --- |
| Task assigned, not claimed after one nudge | Reassign to an active worker |
| Worker silent on a critical path | Nudge once, then reassign |
| Branch behind `main` | Rebase, or cherry-pick the scoped commit |
| Work stacked on a blocked commit | Stop the stack; fix the blocker first |
| Two workers on the same file | Serialize: one lands, the other rebases |
| Task grew beyond its brief | Split it; land the smallest correct slice |

## Backlog

Turn every observation into a durable task: review findings, gate follow-ups,
security notes, and "we tried X and it was slower" (with numbers). Groom in
batches; keep tasks unassigned until a worker frees up. Negative results are
results — close them with the measurements.

## Release

1. `bash deploy/release-check.sh` on the candidate commit (browser smoke,
   image identity, trivy, race).
2. Add the CHANGELOG section and release notes.
3. Tag `v<VERSION>` and push; the Release workflow publishes signed assets.
4. Verify the published release independently (checksums, signature, SBOM,
   image version).

## Deploy and verify

Per `AGENTS.md`: `change → test → deploy → verify`. For this repo's Mac hub
that means rebuilding the binary, installing it, restarting the launchd
service, and checking `/healthz`, the dashboard auth, and the changed
behavior. Server-only changes do not need an agent redeploy.

## Why teams stall

- Nobody owns landing → one supervisor owns gate + push.
- "Done" is vague → a written gate, enforced every time.
- Tasks are too big → slice until each fits one gate.
- Workers finish into an empty queue → keep 1–2 tasks queued per worker.
- A stalled worker holds critical work → nudge once, then reassign.
- Shared checkout collisions → one worktree per worker.
- Trusting green tests → reproduce the claim at the gate.
