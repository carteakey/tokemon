# Containerized agent (supported example)

`docker-compose.agent.example.yml` runs the native Tokemon agent in a
non-root, read-only-rootfs container. It mounts every built-in provider store
and the optional generic JSONL directory read-only; only the dedicated state
volume is writable.

Prepare a mode-`0600` config outside the repository (replace the token without
checking it in):

```env
TOKEMON_SERVER_URL=https://tokemon.example.ts.net
TOKEMON_INGEST_TOKEN=replace-with-a-generated-secret
TOKEMON_MACHINE_ID=container-agent
TOKEMON_ADAPTERS=claude-code,codex,copilot-cli,opencode,antigravity,openclaw,hermes-agent
TOKEMON_HOME=/home/tokemon
TOKEMON_STATE=/state/state.db
TOKEMON_JSONL_PATHS=/home/tokemon/generic/*.jsonl
```

Run configuration and mount checks before starting the agent. The first
command intentionally expands no secrets; the second exits non-zero if any
provider mount can be written:

```bash
chmod 0600 ./agent.env
export TOKEMON_AGENT_CONFIG="$PWD/agent.env"
export TOKEMON_UID="$(id -u)"
export TOKEMON_GID="$(id -g)"
export TOKEMON_AGENT_STATE_DIR="$PWD/data/agent-state"
deploy/agent-container-preflight.sh
docker compose --file deploy/docker-compose.agent.example.yml config --quiet
docker compose --file deploy/docker-compose.agent.example.yml --profile smoke run --rm tokemon-agent-permissions-smoke
docker compose --file deploy/docker-compose.agent.example.yml up -d --build tokemon-agent
```

The host provider directories must exist before Compose starts (an empty
directory is fine for an uninstalled provider). Set the `TOKEMON_*_DIR`
variables in the Compose example when the provider stores live outside
`$HOME`. The preflight creates the state directory with mode `0700` and
verifies that its owner matches the non-root `TOKEMON_UID:GID`; this avoids
Docker's default root-owned bind-directory behavior. The permissions smoke also
verifies the mode-`0600` config, creates `/state/state.db` as the configured UID,
and proves provider mounts reject writes. The agent writes cursors only to
`TOKEMON_AGENT_STATE_DIR` (default `./data/agent-state`), which is the sole
writable mount. The ingest token stays in the mounted config file and is not
placed in Compose environment or process arguments. If your deployment cannot
provide read-only mounts, do not run an agent container; use the native
macOS/Linux installer instead.
