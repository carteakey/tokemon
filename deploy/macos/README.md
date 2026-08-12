# macOS deployment

The installers create user-level LaunchAgents. They do not require root or Docker.

## Server

The primary Mac can run the Tokemon hub persistently with the server installer. It keeps the ingest token in `~/.config/tokemon/server.env` with mode `0600`; the token is not placed in the LaunchAgent arguments.

Build or obtain the matching Tokemon binary, then run:

```bash
bash deploy/macos/install-server.sh \
  --binary ./tokemon \
  --catalog ./catalog/models.yaml \
  --database ./data/tokemon.db \
  --token 'replace-with-a-generated-secret'
```

The default listen address is `0.0.0.0:18080`. The server config supports `TOKEMON_SERVER_ADDR`, `TOKEMON_DATABASE`, `TOKEMON_MODEL_CATALOG`, `TOKEMON_INGEST_TOKEN`, and `TOKEMON_ANALYTICS_TIMEZONE` (an IANA name such as `America/Toronto`; default `UTC`). Optional Insights synthesis reads `TOKEMON_INSIGHTS_OPENAI_API_KEY`, `TOKEMON_INSIGHTS_OPENAI_MODEL`, and `TOKEMON_INSIGHTS_OPENAI_BASE_URL` from the same mode-`0600` config file. Its LaunchAgent is `com.tokemon.server`.

The installer writes:

- `~/.local/bin/tokemon`
- `~/.config/tokemon/server.env` with mode `0600`
- `~/Library/Application Support/Tokemon/models.yaml`
- `~/Library/LaunchAgents/com.tokemon.server.plist`
- `~/Library/Logs/Tokemon/server.log`

Remove the server supervisor and config without deleting the database with:

```bash
bash deploy/macos/install-server.sh --uninstall
```

## Agent

Build or obtain the matching Tokemon binary, then run:

```bash
bash deploy/macos/install-agent.sh \
  --binary ./tokemon \
  --server https://tokemon.example.ts.net \
  --token 'replace-with-a-generated-secret' \
  --adapters claude-code,codex
```

The installer writes:

- `~/.local/bin/tokemon`
- `~/.config/tokemon/agent.env` with mode `0600`
- `~/.local/share/tokemon/state.db` with mode `0600`
- `~/Library/LaunchAgents/com.tokemon.agent.plist`
- `~/Library/Logs/Tokemon/agent.log`

The endpoint is the server base URL. The agent appends `/api/v1/events/batch` and `/api/v1/agents/heartbeat` itself. The LaunchAgent runs as the logged-in user and keeps the token out of process arguments.

For the same install flow on macOS and Linux, use `deploy/install-agent.sh` with a release version. It downloads the architecture-matched archive, verifies `checksums.txt`, writes the adapter profile, and installs the native user supervisor. `deploy/macos/install-agent.sh` remains as a compatibility entry point and delegates to that shared installer.

Remove the service and its config with:

```bash
bash deploy/macos/install-agent.sh --uninstall
```
