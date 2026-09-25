# Tokemon incident response

This runbook is for the self-hosted Tokemon hub and its metadata-only agents.
It is deliberately public: do not put tokens, provider records, prompts,
responses, source code, repository paths, database files, or private contacts
in this document, an issue, or a chat transcript.

## First 15 minutes

1. Assign one incident lead and record the UTC start time, affected hub/agents,
   and the decision owner. Keep one evidence log with timestamps and commands;
   redact credentials before sharing it.
2. Contain before investigating. Restrict the hub to its private Tailscale or
   WireGuard network, remove an untrusted proxy route, stop affected agents,
   and disable public forwarding. Do not delete the database or logs.
3. Preserve evidence. While the service is stopped (or from a read-only copy),
   copy the SQLite database and any `-wal`/`-shm` sidecars, hash the copies, and
   capture the last 100 Tokemon service log lines. Store evidence access-
   controlled and never paste raw event payloads into a ticket.
4. Classify the incident: availability/data loss, credential exposure, or
   sensitive-metadata/privacy report. Use the matching procedure below and
   keep a decision, owner, timestamp, and exit criterion for every action.

## Lost or corrupt data

Do not run repair commands against the live database. Stop the hub gracefully,
preserve the original directory, and work on a copy:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml stop tokemon
mkdir -p evidence
cp data/tokemon.db evidence/tokemon.db.$(date -u +%Y%m%dT%H%M%SZ)
cp data/tokemon.db-wal evidence/tokemon.db-wal.$(date -u +%Y%m%dT%H%M%SZ) 2>/dev/null || true
cp data/tokemon.db-shm evidence/tokemon.db-shm.$(date -u +%Y%m%dT%H%M%SZ) 2>/dev/null || true
shasum -a 256 evidence/tokemon.db.*
sqlite3 evidence/tokemon.db 'PRAGMA integrity_check;'
```

Restore only a verified backup, with the hub stopped and a fresh copy of the
current data retained. Keep the database, WAL, and SHM files together; never
overwrite a running SQLite store:

```bash
cp data/tokemon.db data/tokemon.db.before-restore
cp evidence/tokemon-v0-before-restore.db data/tokemon.db
rm -f data/tokemon.db-wal data/tokemon.db-shm
sqlite3 data/tokemon.db 'PRAGMA integrity_check;'
docker compose --env-file .env -f deploy/docker-compose.yml up -d
curl --fail http://127.0.0.1:${TOKEMON_PORT:-18787}/healthz
```

Record the backup checksum, integrity result, event/token counts before and
after, and the health URL. If counts do not match the declared recovery point,
stop again and escalate; do not guess at repairs.

## Leaked ingest or dashboard credentials

Treat a token as compromised if it appeared in a log, shell history, issue,
screen recording, proxy trace, or an untrusted host. Do not try to recover the
old value or paste it into a report.

1. Contain: restrict the hub to the private network, stop agents that use the
   credential, and remove the exposed proxy route or access rule.
2. Rotate `TOKEMON_INGEST_TOKEN`; if dashboard access was also exposed, rotate
   `TOKEMON_DASHBOARD_TOKEN` independently. Store both in the mode-`0600`
   `.env`/server configuration and restart the existing Compose service in
   place. Never include values in process listings, commits, or PRs.
3. Update enrolled agents through the normal installer/configuration path.
   Revoke the old credential everywhere, including reverse proxies and local
   test scripts.
4. Verify without printing secrets: the old credential receives `401`, the new
   credential can perform an authenticated dashboard/metrics smoke check, and
   an agent heartbeat succeeds. Record only status codes and timestamps.
5. Escalate if the token may have reached a third party; preserve proxy access
   logs and notify affected owners through the repository's private security
   channel.

## Sensitive metadata or privacy report

Tokemon is metadata-only, but a provider adapter or manual import can still
produce an event that should not be retained. Treat the report as valid until
proven otherwise:

1. Stop the affected agent/source and contain the hub. Do not ask the reporter
   to send the offending prompt, response, source, repository path, or database.
2. Preserve only the minimum evidence needed to identify the event (UTC time,
   machine/provider, and a private event identifier). Restrict access and hash
   any copied database before analysis.
3. Export a protected copy before deletion when policy permits:

   ```bash
   go run ./cmd/tokemon export --database /path/to/tokemon.db /secure/evidence/events.jsonl
   chmod 600 /secure/evidence/events.jsonl
   ```

   The export is still sensitive metadata. Never attach it to a public issue.
4. Use the supported purge operation for a date-bounded deletion, after taking
   a protected backup and confirming the exact UTC cutoff:

   ```bash
   go run ./cmd/tokemon purge --database /path/to/tokemon.db --before YYYY-MM-DD
   ```

   For a single-event or field-level removal, keep the original backup,
   escalate to the repository maintainer, and use a reviewed migration or
   restore-and-reimport procedure; do not improvise SQL on the live store.
5. Rotate any credential present in the metadata, fix or disable the adapter,
   and rerun `tokemon inspect` on a sanitized fixture before resuming. Notify
   the reporter and any affected owner with what was removed, when, and how
   recurrence is prevented.

## Notification, escalation, and evidence

- **P0:** public credential exposure, cross-machine data disclosure, or
  unrecoverable active data loss. Contain immediately, preserve evidence, and
  notify the maintainer/affected owners as soon as the first facts are stable.
- **P1:** private-network credential exposure, suspected sensitive metadata, or
  a failed restore with a verified backup available. Contain and begin
  rotation/recovery within the same working session.
- **P2:** isolated malformed metadata, stale agent, or transient availability
  issue with no disclosure. Fix, verify, and record the follow-up.

The incident record should contain: UTC timeline; owner and decision log;
affected components; sanitized command/status output; evidence filenames and
SHA-256 hashes; token rotation/revocation confirmation without token values;
backup/restore or purge result; notification decisions; and prevention work.
Close only after health, authentication, data counts, and the changed adapter
behavior are verified.

## Disposable tabletop checklist

Run this checklist from a clean checkout using a disposable temporary database.
It must not point at `data/tokemon.db` or any production/primary-agent state.

- [ ] Assign an incident lead, a UTC start time, an evidence directory, and a
      communication owner.
- [ ] Create a temporary database and ingest the checked-in fixture:

  ```bash
  tabletop_dir="$(mktemp -d)"
  tabletop_db="$tabletop_dir/tokemon.db"
  go run ./cmd/tokemon import --database "$tabletop_db" examples/usage.jsonl
  sqlite3 "$tabletop_db" 'PRAGMA integrity_check;'
  ```

- [ ] Export the fixture, hash the export, and confirm it is mode `0600`:

  ```bash
  go run ./cmd/tokemon export --database "$tabletop_db" "$tabletop_dir/events.jsonl"
  chmod 600 "$tabletop_dir/events.jsonl"
  shasum -a 256 "$tabletop_dir/events.jsonl"
  ```

- [ ] Copy the temporary database as a recovery point, deliberately corrupt
      only the disposable copy, and verify that `PRAGMA integrity_check` fails;
      retain the clean recovery point and restore it:

  ```bash
  cp "$tabletop_db" "$tabletop_dir/recovery.db"
  cp "$tabletop_db" "$tabletop_dir/corrupt.db"
  printf 'not a sqlite database\n' > "$tabletop_dir/corrupt.db"
  ! sqlite3 "$tabletop_dir/corrupt.db" 'PRAGMA integrity_check;'
  cp "$tabletop_dir/recovery.db" "$tabletop_db"
  sqlite3 "$tabletop_db" 'PRAGMA integrity_check;'
  ```
- [ ] Run `go run ./cmd/tokemon purge --database "$tabletop_db" --before
      2999-01-01`, record the deleted count, restore the recovery point, and
      verify the original event/token counts return.
- [ ] Exercise credential containment with fake tokens only: record old/new
      status-code expectations (`401` old, authenticated success new), then
      confirm no token value appears in the evidence log.
- [ ] Record notification/escalation decisions, evidence hashes, restore and
      health results, owner sign-off, and one prevention action. Delete the
      temporary directory only after the tabletop record is complete.

The repository's database and state tests provide the repeatable backup,
restore, purge, and cursor-retention checks used alongside this tabletop:
`go test ./internal/database ./internal/agent ./internal/api`.
