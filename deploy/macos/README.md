# macOS agent deployment

The installer creates a user-level LaunchAgent. It does not require root or Docker.

Build or obtain the matching Tokemon binary, then run:

```bash
bash deploy/macos/install-agent.sh \
  --binary ./tokemon \
  --server https://tokemon.example.ts.net \
  --token 'replace-with-a-generated-secret'
```

The installer writes:

- `~/.local/bin/tokemon`
- `~/.config/tokemon/agent.env` with mode `0600`
- `~/Library/LaunchAgents/com.tokemon.agent.plist`
- `~/Library/Logs/Tokemon/agent.log`

The endpoint is the server base URL. The agent appends `/api/v1/events/batch` itself. The LaunchAgent runs as the logged-in user and keeps the token out of process arguments.

Remove the service and its config with:

```bash
bash deploy/macos/install-agent.sh --uninstall
```
