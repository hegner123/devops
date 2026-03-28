# devops MCP Tool

Go MCP tool for managing deployed applications on Linux VPS servers. Single binary, two modes via runtime subcommand.

## Architecture

- **MCP mode** (default): JSON-RPC server on stdin/stdout with SQLite metadata store and persistent SSH connection pool. Runs on macOS.
- **Agent mode** (`devops agent`): HTTP server on a unix socket, systemd-managed. Runs on each VPS as root. Has its own SQLite DB for app configs synced from local.
- Communication: SSH channel forwarding (`client.Dial("unix", ...)`) -- no extra ports, SSH handles auth/encryption.
- Self-redeploy: Containers can trigger their own redeployment by POSTing to the agent socket's `/redeploy` endpoint. The agent reads deploy commands from its local DB.

## Build

```bash
just build              # builds devops (macOS) + devops-linux-amd64 (linux/amd64)
just test               # run all tests
just install            # build + copy devops to /usr/local/bin/
just agent-deploy HOST  # scp binary to HOST, restart service
```

Manual build:
```bash
go build -o devops .                                                    # local
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o devops-linux-amd64 . # remote
```

## Key Files

| File | Purpose |
|------|---------|
| main.go | Subcommand dispatch: `devops agent` or MCP mode |
| server.go | `mcpMain()`, JSON-RPC loop, request routing |
| handlers.go | All 18 MCP tool handler implementations |
| db.go | SQLite schema, CRUD operations, upsert for sync |
| ssh.go | SSH connection pool, keepalive, embedded key + configs |
| remote.go | HTTP-over-SSH-channel client |
| agent_main.go | `agentMain()`, opens agent DB, unix socket listener |
| agent_filter.go | Command filter logic, hard-coded + configurable patterns |
| agent_handler.go | Agent HTTP endpoints (/exec, /service, /deploy, /sync-apps, /redeploy, etc.) |
| validate.go | Input validation (shared) |
| version.go | Version constant and shared constants |
| embed/ | Server config files (systemd, UFW, sshd, sysctl, Docker) |

## Database

**Local:** SQLite at `$HOME/.local/share/devops/devops.db` (override: `DEVOPS_DB_PATH` env var).
**Agent:** SQLite at `/var/lib/devops-agent/apps.db` (created by systemd StateDirectory).
Tables: `apps` (deployment metadata), `exec_log` (command audit trail), `command_filters` (per-host command filter rules).
Driver: `modernc.org/sqlite` (pure Go, driver name "sqlite").

## Dependencies

`modernc.org/sqlite`, `golang.org/x/crypto/ssh`

## MCP Registration

```bash
claude mcp add --transport stdio devops -- /usr/local/bin/devops
```

## Tools (18)

devops_list, devops_add, devops_import, devops_remove, devops_update, devops_status, devops_deploy, devops_restart, devops_stop, devops_logs, devops_exec, devops_health, devops_bootstrap, devops_filter_add, devops_filter_list, devops_filter_remove, devops_filter_sync, devops_app_sync

## Agent Endpoints

GET /ping, GET /filters, POST /exec, POST /service, POST /logs, POST /health, POST /discover, POST /deploy, POST /sync-apps, POST /redeploy

## Self-Redeploy Flow

1. `devops_app_sync host=X` pushes app configs from local DB to agent DB
2. Container bind-mounts `/run/devops-agent/agent.sock` and sets `DEVOPS_APP=appname`
3. Container checks GitHub for new release, POSTs `{"name": "$DEVOPS_APP"}` to `/redeploy`
4. Agent reads deploy commands from its DB, runs them (e.g., `docker compose pull && up -d`)
5. Container gets replaced; agent records result in its DB

Retry protection: if last deploy failed within 5 minutes, `/redeploy` returns 429 unless `force=true`.

## Testing

```bash
go test -v -count=1 ./...  # all tests (DB, handlers, agent, validation)
```

Tests use in-memory SQLite and mock agent (unix socket httptest server). No real SSH or remote hosts needed.
