# devops MCP Tool

Go MCP tool for managing deployed applications on Linux VPS servers. Single codebase, two binaries via build tags.

## Architecture

- **Local binary** (`devops`): MCP server with SQLite metadata store and persistent SSH connection pool. Runs on macOS.
- **Remote agent** (`devops-agent`): HTTP server on a unix socket, systemd-managed. Runs on each VPS as root. Stdlib-only, no dependencies.
- Communication: SSH channel forwarding (`client.Dial("unix", ...)`) -- no extra ports, SSH handles auth/encryption.

## Build

```bash
just build          # builds both: devops (macOS) + devops-agent (linux/amd64)
just test           # run local tests
just test-agent     # run agent tests
just install        # build + copy devops to /usr/local/bin/
just agent-deploy HOST  # scp agent to HOST, restart service
```

Manual build:
```bash
go build -o devops .                                           # local MCP tool
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags agent -o devops-agent .  # remote agent
```

## Build Tags

- No tag (default): local MCP binary -- includes SQLite, SSH, embedded configs
- `-tags agent`: remote agent binary -- stdlib-only, unix socket HTTP server

Files with `//go:build !agent` are local-only. Files with `//go:build agent` are agent-only. Files without build tags are shared.

## Key Files

| File | Purpose | Build Tag |
|------|---------|-----------|
| main.go | MCP server entry point | !agent |
| server.go | JSON-RPC loop, request routing | !agent |
| handlers.go | All 13 tool handler implementations | !agent |
| db.go | SQLite schema, CRUD operations | !agent |
| ssh.go | SSH connection pool, keepalive, embedded key + configs | !agent |
| remote.go | HTTP-over-SSH-channel client | !agent |
| agent_main.go | Agent entry point, unix socket listener | agent |
| agent_handler.go | Agent HTTP endpoints (/exec, /service, /deploy, etc.) | agent |
| validate.go | Input validation (shared by both binaries) | none |
| version.go | Version constant (shared) | none |
| embed/ | Server config files (systemd, UFW, sshd, sysctl, Docker) | !agent |

## Database

SQLite at `$HOME/.local/share/devops/devops.db` (override: `DEVOPS_DB_PATH` env var).
Tables: `apps` (deployment metadata), `exec_log` (command audit trail).
Driver: `modernc.org/sqlite` (pure Go, driver name "sqlite").

## Dependencies

- Local: `modernc.org/sqlite`, `golang.org/x/crypto/ssh`
- Agent: stdlib only

## MCP Registration

```bash
claude mcp add --transport stdio devops -- /usr/local/bin/devops
```

## Tools (13)

devops_list, devops_add, devops_import, devops_remove, devops_update, devops_status, devops_deploy, devops_restart, devops_stop, devops_logs, devops_exec, devops_health, devops_bootstrap

## Testing

```bash
go test -v -count=1 ./...              # local binary tests (DB, handlers, validation)
go test -v -count=1 -tags agent ./...  # agent binary tests (endpoints, discovery, deploy)
```

Tests use in-memory SQLite and mock agent (unix socket httptest server). No real SSH or remote hosts needed.
