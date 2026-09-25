#!/bin/sh
set -eu

config_path=${TOKEMON_AGENT_CONFIG:?Set TOKEMON_AGENT_CONFIG to a mode-0600 agent.env}
state_dir=${TOKEMON_AGENT_STATE_DIR:-./data/agent-state}
expected_uid=${TOKEMON_UID:-$(id -u)}
expected_gid=${TOKEMON_GID:-$(id -g)}

if [ ! -f "$config_path" ]; then
  echo "agent config does not exist: $config_path" >&2
  exit 1
fi
config_mode=$(stat -c '%a' "$config_path" 2>/dev/null || stat -f '%Lp' "$config_path")
if [ "$config_mode" != "600" ]; then
  echo "agent config must have mode 0600 (found $config_mode)" >&2
  exit 1
fi

mkdir -p "$state_dir"
chmod 0700 "$state_dir"
if ! chown "$expected_uid:$expected_gid" "$state_dir" 2>/dev/null; then
  owner=$(stat -c '%u:%g' "$state_dir" 2>/dev/null || stat -f '%u:%g' "$state_dir")
  if [ "$owner" != "$expected_uid:$expected_gid" ]; then
    echo "cannot prepare writable agent state directory $state_dir for UID:GID $expected_uid:$expected_gid (owner $owner)" >&2
    exit 1
  fi
fi
owner=$(stat -c '%u:%g' "$state_dir" 2>/dev/null || stat -f '%u:%g' "$state_dir")
if [ "$owner" != "$expected_uid:$expected_gid" ]; then
  echo "agent state directory owner $owner does not match UID:GID $expected_uid:$expected_gid" >&2
  exit 1
fi

echo "agent config is mode 0600 and state directory is writable by UID:GID $expected_uid:$expected_gid"
