# devops

A [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server that lets AI assistants manage applications running on Linux VPS servers. Register your apps once, then deploy, restart, check logs, and run commands through natural conversation.

## Why use this?

Managing a handful of VPS servers means constantly switching between SSH sessions, remembering service names, and typing the same deployment sequences over and over. `devops` eliminates that friction:

- **Talk to your servers through Claude.** "Deploy the API," "show me the logs for the worker," "restart nginx on prod" -- all without opening a terminal.
- **One command bootstraps a fresh server.** SSH hardening, firewall, Docker log rotation, automatic security updates -- applied in seconds, not hours of manual configuration.
- **Everything is audited.** Every command execution is logged to a local SQLite database. You always know what ran, where, and when.
- **No agents exposed to the internet.** The remote agent listens on a Unix socket, reachable only through authenticated SSH. There are no open ports, no API keys to rotate, no attack surface.

## How it works

Single binary, two modes:

```
┌─────────────────────┐          SSH          ┌─────────────────────┐
│   Your machine      │   ──────────────────▶  │   Linux VPS         │
│                     │                        │                     │
│  Claude ◀──▶ devops │   SSH channel fwd      │  devops agent       │
│         (MCP stdio) │   ──────────────────▶  │  (unix socket HTTP) │
│                     │                        │                     │
│  SQLite (metadata)  │                        │  systemd / docker   │
└─────────────────────┘                        └─────────────────────┘
```

**MCP mode (`devops`)** is the default. It runs on your machine as an MCP server, stores app metadata in SQLite, and maintains a pool of persistent SSH connections to your servers.

**Agent mode (`devops agent`)** runs on each VPS as a systemd service. It accepts HTTP requests over a Unix socket and executes operations (service management, deployments, log retrieval, arbitrary commands).

Communication flows through SSH channel forwarding -- the local binary dials the Unix socket through the SSH connection. No extra ports, no API tokens, no TLS certificates to manage.

## Security model

The remote agent runs as root (necessary for `systemctl` and `docker` operations), which makes the security boundary critical. Here's how it stays safe:

### No network exposure

The agent listens exclusively on a Unix socket at `/run/devops-agent/agent.sock` with `0600` permissions. There is no TCP listener. The only way to reach it is through an authenticated SSH session that channel-forwards to the socket. An attacker cannot connect to the agent without first compromising SSH access.

### SSH as the only gate

All authentication and encryption is handled by OpenSSH. The tool uses key-based authentication with your existing SSH keys (`~/.ssh/id_ed25519` or `~/.ssh/id_rsa`, overridable per-app via `key_path`). Password authentication is disabled on bootstrapped servers.

### Server hardening on bootstrap

When you bootstrap a new server, `devops` automatically applies:

- **SSH hardening** -- password auth disabled, keyboard-interactive disabled, root login restricted to key-only, max 3 auth attempts, X11 forwarding disabled
- **Firewall (UFW)** -- default deny incoming, allow only SSH (22), HTTP (80), HTTPS (443)
- **Automatic security updates** -- unattended-upgrades configured for security patches only, no auto-reboot
- **Kernel tuning** -- TCP backlog, connection reuse, memory swappiness optimized for server workloads
- **Docker hardening** -- JSON-file logging with rotation (10MB, 3 files), live-restore enabled, overlay2 storage driver

### Command filtering

The agent enforces hard-coded security invariants that block dangerous commands before execution. Every command submitted via `/exec` or `/deploy` is checked against these filters:

- **Destructive commands** -- `rm -rf /`, `mkfs`, `dd if=`, `shred`, `wipefs` are always blocked. `rm -rf /opt/myapp` is allowed (path-aware detection).
- **Firewall tampering** -- disabling, deleting, or flushing firewall rules (`ufw disable`, `iptables -F`, `nft flush`, etc.) and stopping firewall services are blocked.
- **SSH disruption** -- stopping or disabling `ssh`/`sshd` services is blocked.
- **Power commands** -- `reboot`, `shutdown`, `halt`, `poweroff` are blocked by default but can be enabled per-host via filter config.
- **Port opening** -- commands that open firewall ports are blocked unless the port is in the allowed list.
- **Custom filters** -- additional patterns can be added per-host via `devops_filter_add` and synced to the agent with `devops_filter_sync`.

Blocked commands return HTTP 403. The filter uses case-insensitive `strings.Contains` matching -- this is defense-in-depth, not a sandbox. SSH authentication remains the primary security boundary.

### Minimal agent surface

The agent exposes exactly 10 HTTP endpoints: `/ping`, `/filters`, `/exec`, `/service`, `/logs`, `/health`, `/discover`, `/deploy`, `/sync-apps`, and `/redeploy`. Each endpoint validates its input and scopes its operations.

### Input validation

Both modes validate all inputs: hostnames, service names, ports, paths, and runtimes are checked against strict allow-lists before any operation executes.

## Installation

### Prerequisites

- Go 1.22+
- [just](https://github.com/casey/just) (task runner, optional but recommended)
- **Root access to your Linux VPS(es) via SSH key authentication.** You must have an SSH key already installed on the server before using `devops`. Password authentication is not supported and never will be -- the tool communicates over SSH programmatically, which is fundamentally incompatible with interactive password prompts. If you don't have key-based access set up yet, do that first (`ssh-copy-id root@your-server`).

### Build from source

```bash
git clone https://github.com/hegner123/devops.git
cd devops

# Build the binary
just build

# Install to your PATH
just install
```

Manual build without `just`:

```bash
# Local MCP binary (runs on your machine)
go build -o devops .

# Remote agent (same binary, cross-compiled for Linux)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o devops-linux-amd64 .

# Install
cp devops /usr/local/bin/devops
```

### Register with Claude Code

```bash
claude mcp add --transport stdio devops -- /usr/local/bin/devops
```

## Getting started

### 1. Bootstrap a server

Point `devops` at a fresh VPS. It will install the agent, harden SSH, configure the firewall, and set up automatic security updates:

```
You: "Bootstrap my server at 203.0.113.10"
Claude: calls devops_bootstrap with host=203.0.113.10
→ installed 0.2.9, server configured
```

For subsequent runs, bootstrap detects the existing agent and upgrades it if a newer version is available.

### 2. Register an app

Tell Claude about an application running on your server:

```
You: "Register my API app on 203.0.113.10. It's a Docker Compose service
      called 'api' at /opt/api with deploy commands: git pull, docker compose up -d"
Claude: calls devops_add
→ added app "api" on 203.0.113.10
```

Or import an existing deployment automatically -- the agent inspects the directory and discovers the runtime, services, repo URL, and branch:

```
You: "Import whatever is running at /opt/api on 203.0.113.10, call it 'api'"
Claude: calls devops_import
→ discovered docker runtime, compose file, 2 services
```

### 3. Manage your apps

Once registered, you manage apps by name:

```
"Deploy the API"                         → devops_deploy
"Show me the API logs"                   → devops_logs
"Restart the API"                        → devops_restart
"What's the status of the API?"          → devops_status
"Check if the API health endpoint is up" → devops_health
"Run 'df -h' on the API server"          → devops_exec
"Stop the worker"                        → devops_stop
```

## Tools reference

| Tool | Description |
|------|-------------|
| `devops_list` | List all registered apps, optionally filtered by host |
| `devops_add` | Register a new app (name, host, service_name required) |
| `devops_remove` | Unregister an app (does not affect the running service) |
| `devops_update` | Update an app's configuration (partial updates supported) |
| `devops_import` | Auto-discover and import an existing deployment from a directory |
| `devops_status` | Get the current service status from the remote agent |
| `devops_deploy` | Run deploy commands on the server with streaming output |
| `devops_restart` | Restart a service (systemd or docker compose) |
| `devops_stop` | Stop a service |
| `devops_logs` | Retrieve recent service logs (default: 50 lines) |
| `devops_exec` | Execute a shell command on the server (pipes, redirects supported) |
| `devops_health` | HTTP health check against the app's configured health URL |
| `devops_bootstrap` | Install or upgrade the agent on a host, with server hardening |
| `devops_version` | Return the tool version |
| `devops_filter_add` | Add a custom command filter for a host |
| `devops_filter_list` | List custom command filters, optionally by host |
| `devops_filter_remove` | Remove a custom command filter |
| `devops_filter_sync` | Sync filters and port/reboot/shutdown settings to the remote agent |
| `devops_app_sync` | Push app configs from local DB to the remote agent for self-redeploy |

## App configuration

When registering or updating an app, these fields are available:

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | yes | -- | Unique identifier for the app |
| `host` | yes | -- | Server hostname or IP |
| `service_name` | yes | -- | Systemd unit or Docker Compose service name |
| `port` | no | 22 | SSH port |
| `user` | no | root | SSH user |
| `runtime` | no | docker | `docker` or `systemd` |
| `compose_file` | no | -- | Path to docker-compose.yaml (docker runtime) |
| `repo_url` | no | -- | Git repository URL |
| `branch` | no | main | Git branch |
| `deploy_dir` | no | -- | Directory where deploy commands execute |
| `binary_path` | no | -- | Path to binary (systemd runtime) |
| `health_check_url` | no | -- | HTTP endpoint for health checks |
| `deploy_commands` | no | `[]` | JSON array of shell commands to run during deploy |
| `notes` | no | -- | Free-form metadata |
| `key_path` | no | `~/.ssh/id_ed25519` | SSH private key path (default: ~/.ssh/id_ed25519, fallback: ~/.ssh/id_rsa) |

## Deploying the agent to servers

### First time (bootstrap)

Use `devops_bootstrap`. It handles everything: config files, firewall, agent binary download, systemd service setup.

### Updating the agent

Either run `devops_bootstrap` again (it detects the existing agent and upgrades) or deploy manually:

```bash
just agent-deploy your-server.com
```

This builds the Linux binary, copies it to the server, and restarts the service.

## Local data

App metadata and command audit logs are stored in SQLite at:

```
$HOME/.local/share/devops/devops.db
```

Override with the `DEVOPS_DB_PATH` environment variable.

## Development

```bash
just test           # Run all tests
just build          # Build the binary
just clean          # Remove built binary
```

Tests use in-memory SQLite and a mock agent (Unix socket httptest server). No real SSH connections or remote hosts needed.

## Platform Support

- macOS (local MCP tool)
- Linux (local MCP tool and remote agent)

Windows is not supported.

## License

MIT
