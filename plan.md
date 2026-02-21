devops MCP Tool - Implementation Plan (v8)

Context

Build a Go MCP tool for managing deployed applications on Linux VPS servers. Single
codebase, two binaries via build tags: a local MCP tool (devops) that stores metadata in
SQLite and maintains persistent SSH connections, and a remote agent (devops agent) started
with `devops agent` that listens on a unix socket and executes commands locally. The local
tool reaches the agent via SSH channel forwarding (client.Dial("unix", ...)) -- no extra
ports opened, SSH handles auth and encryption.

Primary deployment model: Linode VPS with Docker containers (app + Caddy reverse proxy).
The agent supports both Docker and systemd runtimes, selected per-app via a runtime field.
The agent itself is always a systemd service -- it must exist before containers and survive
container restarts.

Follows the terse-mcp ecosystem patterns: manual JSON-RPC, MCP-only mode, token-efficient
output. Single codebase with build tags produces two lean binaries -- the local binary
includes SQLite + SSH, the agent binary is stdlib-only.

Architecture

    [Claude]
         |
    [devops MCP tool]  (local, macOS)
         |  SQLite: app metadata
         |  SSH connection pool: map[string]*ssh.Client (one per host, persistent)
         |  Keepalive goroutine per connection (30s interval)
         |
         | ssh.Client.Dial("unix", "/run/devops-agent/agent.sock")
         |  (SSH channel forwarding, no extra ports)
         |
    [devops agent]  (remote, each VPS)
         |  HTTP server on unix socket /run/devops-agent/agent.sock
         |  Socket: chmod 0600 root:root
         |  Systemd managed: Type=simple, Restart=on-failure
         |  Validates commands server-side
         |  Returns structured JSON responses
         |  Streams deploy output via chunked HTTP

Why not raw SSH exec: the agent returns structured JSON (not stdout parsing), validates
commands server-side (not just client-side), can stream deploy output incrementally, and
maintains state between calls if needed later.

Project Structure

Single codebase, two binaries via build tags. Files tagged //go:build !agent contain
MCP server, SQLite, and SSH code (local binary). Files tagged //go:build agent contain
the unix socket HTTP server (remote binary). Shared files (version, validation) have
no build tags.

devops/
    main.go              # Entry point, MCP server startup              //go:build !agent
    server.go            # MCP JSON-RPC loop, request routing, signals  //go:build !agent
    protocol.go          # JSON-RPC types (from notab/mem pattern)      //go:build !agent
    tools.go             # Tool definitions (tools/list response)       //go:build !agent
    db.go                # SQLite schema, connection, CRUD              //go:build !agent
    ssh.go               # SSH connection pool, keepalive, forwarding   //go:build !agent
    remote.go            # HTTP client over SSH-forwarded unix socket   //go:build !agent
    handlers.go          # All 13 tool handler implementations          //go:build !agent
    db_test.go           # DB layer tests (in-memory SQLite)            //go:build !agent
    handlers_test.go     # Table-driven tests with mock agent           //go:build !agent
    agent_main.go        # Agent entry point, socket listener, signals  //go:build agent
    agent_handler.go     # HTTP route handlers (exec, service, deploy)  //go:build agent
    agent_handler_test.go                                               //go:build agent
    version.go           # const Version = "1.0.0"                      (shared, no tag)
    validate.go          # Input validation (used by both binaries)     (shared, no tag)
    validate_test.go     # Validation tests                             (shared, no tag)
    keys/
        deploy_key       # SSH private key (gitignored, embedded)       //go:build !agent
    embed/                                                              //go:build !agent
        devops-agent.service   # Systemd unit for the agent
        ufw.sh                 # Firewall setup (SSH + HTTP + HTTPS only)
        sshd-hardening.conf    # Drop-in for /etc/ssh/sshd_config.d/
        sysctl-tuning.conf     # Network performance tuning
        docker-daemon.json     # Docker log rotation + live-restore
        unattended-upgrades    # Security auto-updates config
        setup.sh               # Orchestrator: applies all embedded configs
    go.mod
    go.sum
    justfile
    .gitignore
    CLAUDE.md

Build:
    go build -o devops .                                    # local MCP tool (default, no tags)
    go build -tags agent -o devops-agent .                  # remote agent (stdlib-only)

Dependencies

Local binary (no build tag):
- modernc.org/sqlite - pure Go SQLite (no CGo, consistent with hq). Driver name: "sqlite"
- golang.org/x/crypto/ssh - SSH client + channel forwarding

Agent binary (-tags agent):
- No dependencies beyond stdlib (net/http, os/exec, encoding/json).
  Build tags exclude all SQLite and SSH imports from the agent binary.

Version

const Version = "1.0.0" in version.go (shared, no build tag). Both binaries report the
same version. The /ping endpoint returns it; devops_bootstrap compares it. Version is
bumped in one place, both binaries pick it up.

SQLite Schema

Single table, location $HOME/.local/share/devops/devops.db (override: DEVOPS_DB_PATH env var).

CREATE TABLE IF NOT EXISTS apps (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT UNIQUE NOT NULL,
    host TEXT NOT NULL,
    port INTEGER NOT NULL DEFAULT 22,
    user TEXT NOT NULL DEFAULT 'root',
    runtime TEXT NOT NULL DEFAULT 'docker',       -- 'docker' or 'systemd'
    service_name TEXT NOT NULL,                   -- systemd unit name or docker compose service name
    compose_file TEXT NOT NULL DEFAULT '',        -- path to compose file (docker runtime)
    repo_url TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT 'main',
    deploy_dir TEXT NOT NULL DEFAULT '',
    binary_path TEXT NOT NULL DEFAULT '',         -- used by systemd runtime
    health_check_url TEXT NOT NULL DEFAULT '',
    deploy_commands TEXT NOT NULL DEFAULT '[]',
    notes TEXT NOT NULL DEFAULT '',
    key_path TEXT NOT NULL DEFAULT '',
    last_deploy_at TEXT,
    last_deploy_ok INTEGER,
    last_deploy_output TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_apps_host ON apps(host);

CREATE TABLE IF NOT EXISTS exec_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    app_name TEXT NOT NULL,
    host TEXT NOT NULL,
    command TEXT NOT NULL,
    args TEXT NOT NULL DEFAULT '[]',
    exit_code INTEGER,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TRIGGER IF NOT EXISTS apps_updated_at
AFTER UPDATE ON apps
BEGIN
    UPDATE apps SET updated_at = datetime('now') WHERE id = NEW.id;
END;

Changes from v7:
- Deploy commands execute via sh -c (explicit shell interpretation for pipes/redirects)
- /exec cmd field must be a single binary name (not shell string), args are separate
- Pool close() takes (user, host, port) to match map key "user@host:port"
- devops_add returns explicit error message on duplicate name

Changes from v6:
- Replaced hand-rolled YAML parser with docker compose config --services
- Added 30s HTTP client timeout for non-streaming agent calls
- Specified mid-stream deploy failure handling (agent aborts, local marks unknown)
- Clarified setup.sh as activator only (bootstrap writes files, setup.sh reloads)
- docker-daemon.json: overwrite on fresh install, not merged
- Added dir param to /deploy endpoint (commands run in deploy_dir)

Changes from v5:
- Embedded server configuration files (embed/ directory)
- Bootstrap applies UFW, SSH hardening, sysctl, Docker daemon, unattended-upgrades
- Configs applied on fresh install only; updates skip unless forced
- setup.sh orchestrator script (embedded, not installed)

Changes from v4:
- Single codebase with build tags (no agent/ subdirectory)
- Shared version.go constant for bootstrap version comparison
- Two build targets: default (local MCP) and -tags agent (remote agent)

Changes from v3:
- Added devops_import tool (13 tools total) with agent /discover endpoint
- Changed devops_bootstrap to take host directly instead of app name
- Added directory discovery specification
- Added bootstrap idempotency specification

Changes from v2:
- Added runtime field ('docker' default, 'systemd' alternative)
- Added compose_file (path to docker-compose.yml, docker runtime)
- Removed config_path and log_path (both runtimes handle these internally)
- service_name now dual-purpose: systemd unit name or docker compose service name

Changes from v1:
- Removed env_vars (unused, add when needed)
- Added key_path (per-host SSH key override, empty = use embedded key)
- Added last_deploy_ok (INTEGER bool: 1=success, 0=failure, NULL=never deployed)
- Added last_deploy_output (stderr/stdout from last deploy, tail-truncated to last 4KB)
- Added updated_at trigger

WAL mode, busy_timeout=5000, MaxOpenConns(1).

Input Validation

All fields that touch shell commands or SSH connection parameters are validated on the
local side before INSERT/UPDATE, and re-validated on the agent side before execution.

validate.go provides:

    func validateServiceName(s string) error    // [a-zA-Z0-9._@:-]+, max 256
    func validateHostname(s string) error       // [a-zA-Z0-9.-]+, max 253
    func validateUsername(s string) error        // [a-zA-Z0-9._-]+, max 32
    func validatePort(p int) error              // 1-65535

    func validateRuntime(s string) error          // "docker" or "systemd"
    func validatePath(s string) error              // absolute path, no null bytes, max 4096
    func validateDeployCommands(s string) error    // valid JSON array of strings, non-empty

Service name validation covers systemd unit names including template instances (foo@bar)
and docker compose service names (same character class works for both).
These are exact character class checks, not regex -- implemented with a loop over runes.
deploy_commands is validated as valid JSON on INSERT/UPDATE.
compose_file validated as absolute path when runtime is docker.

SSH Connection Pool

ssh.go provides a connection pool keyed by "user@host:port":

    type connEntry struct {
        client *ssh.Client
        cancel context.CancelFunc  // cancels the keepalive goroutine
    }

    type connPool struct {
        mu    sync.Mutex
        conns map[string]*connEntry
        key   ssh.Signer           // default embedded key (//go:embed keys/deploy_key)
    }

    func (p *connPool) get(ctx context.Context, app *App) (*ssh.Client, error)
    func (p *connPool) close(user, host string, port int)   // constructs key "user@host:port" internally
    func (p *connPool) closeAll()

Behavior:
- get() returns existing client if alive, otherwise dials a new one
- Liveness check: client.SendRequest("keepalive@openssh.com", true, nil) before returning
- If liveness check fails, close dead client, dial fresh
- 10s dial timeout via net.DialTimeout, then ssh.NewClientConn on the raw conn
- ssh.InsecureIgnoreHostKey() (single-user fleet, SSH key trust model)
- Per-host key override: if app.KeyPath != "", parse that key instead of embedded default
- Keepalive: background goroutine per client sends keepalive every 30s, closes on failure.
  Each keepalive goroutine receives a per-connection context via connEntry.cancel.
  When keepalive detects failure, it closes the client and removes the pool entry.
- closeAll() cancels all keepalive goroutine contexts, then closes all clients. Called on
  MCP server shutdown.

Key embedded via //go:embed keys/deploy_key, parsed once at startup via ssh.ParsePrivateKey.
Per-host keys (from app.KeyPath) parsed on first connection and cached in the pool.

Reaching the Agent (remote.go)

remote.go wraps HTTP-over-SSH-channel-forwarding:

    type agentClient struct {
        pool *connPool
    }

    func (a *agentClient) call(ctx context.Context, app *App, endpoint string, req any) (*AgentResponse, error)
    func (a *agentClient) callStream(ctx context.Context, app *App, endpoint string, req any) (io.ReadCloser, error)

call() does:
1. pool.get(ctx, app) to get the *ssh.Client
2. client.Dial("unix", "/run/devops-agent/agent.sock") to get a net.Conn
3. HTTP POST to endpoint with JSON body over that conn, 30s client timeout
4. Read JSON response, close conn
5. On connection error, retry once by calling pool.get() again (which handles
   invalidation and redialing internally, avoiding concurrent retry races)

HTTP client for non-streaming calls: http.Client{Timeout: 30 * time.Second}.
Prevents hung agent processes from blocking the MCP tool indefinitely.

callStream() uses a separate http.Client with no Timeout (the deploy endpoint can
run for up to 30m). Instead, the stream reader checks ctx.Done() between chunks.

The forwarded connection is per-request (cheap -- it is an SSH channel open, not a TCP
handshake). The expensive part (SSH handshake) only happens once per host.

Remote Agent (devops agent)

Started via `devops agent` (build with -tags agent). Minimal HTTP server, stdlib-only.
Runs as root on each VPS.

Socket: /run/devops-agent/agent.sock, chmod 0600, owned by root.
Systemd unit: Type=simple, Restart=on-failure, RuntimeDirectory=devops-agent.

Endpoints:

    POST /exec        {"cmd":"df","args":["-h"]}           -> {"ok":true,"stdout":"...","stderr":"...","exit":0}
                      cmd must be a single binary name resolvable by exec.LookPath (not a shell string).
                      args are passed as separate arguments. Use devops_deploy for shell commands.
    POST /service     {"action":"status","name":"nginx","runtime":"docker","compose_file":"/opt/app/docker-compose.yml"}
                                                           -> {"ok":true,"data":{"state":"running","status":"Up 2 hours"}}
    POST /service     {"action":"restart","name":"nginx","runtime":"systemd"}
                                                           -> {"ok":true}
    POST /service     {"action":"stop",...}                 -> {"ok":true}
    POST /logs        {"name":"nginx","lines":50,"runtime":"docker","compose_file":"..."}
                                                           -> {"ok":true,"output":"..."}
    POST /deploy      {"dir":"/root/app","commands":["git pull","docker compose up -d --build"]}  -> chunked streaming
    POST /health      {"url":"http://localhost:8080/health"} -> {"ok":true,"status":200,"body":"OK"}
    POST /discover    {"dir":"/root/modulacms.com"}        -> {"ok":true,"data":{...discovered metadata}}
    GET  /ping                                             -> {"ok":true,"version":"1.0.0","hostname":"vps1"}

All responses follow: {"ok":bool, "error":"...", ...fields}

The /service and /logs endpoints accept a "runtime" field ("docker" or "systemd") that
selects which commands to run. The local MCP tool passes runtime and compose_file from
the app's DB record -- the agent does not store app metadata.

Service actions by runtime (exec.CommandContext, binary resolved via exec.LookPath):

    systemd:
    - status:  systemctl show -p ActiveState,MainPID,MemoryCurrent <name>
    - restart: systemctl restart <name>
    - stop:    systemctl stop <name>
    - logs:    journalctl -u <name> -n <lines> --no-pager

    docker:
    - status:  docker compose -f <compose_file> ps <name> --format json
    - restart: docker compose -f <compose_file> restart <name>
    - stop:    docker compose -f <compose_file> stop <name>
    - logs:    docker compose -f <compose_file> logs --tail <lines> <name>

The agent validates service names and compose_file paths server-side.
Service names: rejects anything not matching [a-zA-Z0-9._@:-]+.
Compose file: must be absolute path, must exist, no null bytes.

Directory Discovery

POST /discover accepts {"dir":"/path/to/project"} and inspects the directory to return
metadata about an existing deployment. The dir must be absolute, must exist, and must be
a directory.

Discovery steps (in order):
1. Check for compose file: look for compose.yml, docker-compose.yml, compose.yaml,
   docker-compose.yaml in the dir. If found, runtime = "docker".
2. If compose file found, get service names via:
   docker compose -f <file> config --services
   Output is one service name per line. Handles all YAML edge cases (anchors,
   merge keys, extensions). Docker Compose is already required for this runtime.
3. Check for git repo: look for .git directory. If found, extract remote URL
   (git config --get remote.origin.url) and current branch (git branch --show-current).
4. Check running containers: if compose file found, run
   docker compose -f <file> ps --format json to get current container states.
5. Check for systemd units: if no compose file found, look for .service files in the dir
   or check if a service matching the directory name is active (systemctl is-active <name>).
   If found, runtime = "systemd".

Response:

    {
        "ok": true,
        "data": {
            "runtime": "docker",
            "compose_file": "/root/modulacms.com/compose.yml",
            "services": ["site"],
            "repo_url": "git@github.com:user/modulacms.com.git",
            "branch": "main",
            "containers": [
                {"name": "site", "state": "running", "status": "Up 2 hours", "ports": "0.0.0.0:5050->5050/tcp"}
            ]
        }
    }

If no compose file and no systemd unit found, returns ok:true with runtime:"unknown" and
empty fields -- the caller decides what to do.

Deploy Streaming

POST /deploy accepts {"dir":"/path/to/project","commands":["cmd1","cmd2",...]} and returns
chunked HTTP (Transfer-Encoding: chunked). All commands execute with dir as the working
directory (os/exec Cmd.Dir). The dir must be absolute, must exist, and must be a
directory (same validation as /discover). The local devops_deploy handler passes
deploy_dir from the app's DB record.
Each chunk is a JSON line:

    {"step":1,"cmd":"git pull","status":"running"}
    {"step":1,"cmd":"git pull","status":"done","exit":0,"stdout":"Already up to date.","elapsed":"0.3s"}
    {"step":2,"cmd":"docker compose up -d --build","status":"running"}
    {"step":2,"cmd":"docker compose up -d --build","status":"done","exit":0,"stdout":"...","elapsed":"12.1s"}

On failure (non-zero exit), the stream ends with:

    {"step":2,"cmd":"docker compose up -d --build","status":"failed","exit":1,"stderr":"...","elapsed":"5.2s"}

The local devops_deploy handler reads the stream, collects all output, and returns a
structured summary to the MCP caller. It also stores the result in last_deploy_ok and
last_deploy_output (tail-truncated to last 4KB -- error output is at the end).

Each deploy command is executed via exec.CommandContext(ctx, "sh", "-c", command) to
support shell syntax (pipes, redirects, compound commands). This is intentional --
deploy_commands are authored by the tool's single user, not untrusted input. Each
command has a 5m timeout. The deploy endpoint has a 30m overall timeout.

Mid-stream connection failure handling:

Agent side: before starting each command, check r.Context().Done() (net/http cancels
this when the client disconnects). If cancelled, skip remaining commands and return.
Commands already running continue to completion (exec.CommandContext handles their
timeout), but no new commands start.

Local side: if the stream reader returns an error (io.UnexpectedEOF, connection reset),
the devops_deploy handler:
- Sets last_deploy_ok = NULL (unknown state, not 0/failure)
- Stores whatever output was received so far in last_deploy_output
- Returns error to MCP caller: "connection lost during deploy at step N, server
  state unknown -- use devops_status to check"

MCP Tools (13 total)

Tool              Params                                      Behavior
devops_list       host (opt)                                  DB: list all apps, compact summary
devops_add        name, host, service_name (req) + optional   DB: INSERT (validates all fields). runtime defaults to 'docker'.
                                                              Returns error if name exists: "app <name> already exists, use devops_update to modify"
devops_import     name, host, deploy_dir (req) + optional     Agent: POST /discover, then DB: INSERT with discovered + override fields
devops_remove     name (req)                                  DB: DELETE
devops_update     name (req) + any optional fields            DB: read-modify-write (SELECT, merge, validate, UPDATE all fields)
devops_status     name (req)                                  Agent: POST /service {action:status} (passes runtime + compose_file from DB)
devops_deploy     name (req), commands (opt override)         Agent: POST /deploy (streaming), updates DB. Error if no commands (empty deploy_commands and no override)
devops_restart    name (req)                                  Agent: POST /service {action:restart} (passes runtime + compose_file from DB)
devops_stop       name (req)                                  Agent: POST /service {action:stop} (passes runtime + compose_file from DB)
devops_logs       name (req), lines (opt, default 50)         Agent: POST /logs (passes runtime + compose_file from DB)
devops_exec       name (req), command (req), args (opt)       Agent: POST /exec. Logs cmd+args+exit to exec_log table for forensics
devops_health     name (req)                                  Agent: POST /health (uses health_check_url from DB; errors if empty)
devops_bootstrap  host (req), user (opt), port (opt), key_path (opt)
                                                              SSH: idempotent agent install/upgrade (see Bootstrap Idempotency below)

devops_import requires the agent to be running (use devops_bootstrap first). It calls
POST /discover on the agent, then creates a DB record with the discovered metadata.
Any optional fields provided by the caller override what discovery found. The handler:
1. Calls agent POST /discover {dir: deploy_dir}
2. Maps discovered fields: runtime, compose_file, services[0] -> service_name,
   repo_url, branch. If multiple services found, uses the first; caller can override
   via service_name param.
3. Merges caller-provided overrides on top of discovered values
4. Validates all fields (same as devops_add)
5. Inserts into DB
6. Returns the created record plus the full discovery response so the caller can see
   what was found vs what was stored

devops_bootstrap uses raw SSH (not the agent) to install or upgrade the agent on the
target host. It downloads the agent binary from the GitHub release matching the local
Version constant -- no scp from local machine (different architecture). This is the
only tool that uses direct SSH exec instead of the agent -- because the agent may not
exist yet when bootstrapping. Takes host directly (not app name) since no app record
exists yet.

All configuration files (systemd unit, firewall, SSH hardening, etc.) are embedded in
the local binary and written to the remote host via SSH. See Embedded Server Configuration.

Bootstrap Idempotency

devops_bootstrap is safe to run repeatedly. Behavior depends on current agent state:

1. No agent binary on host (/usr/local/bin/devops does not exist):
   - Fresh install: apply embedded server configs (setup.sh), download binary
     from GitHub release, daemon-reload, enable --now
   - Returns: "installed <version>, server configured"

2. Agent binary exists:
   a. Try to reach agent: SSH channel forward to unix socket, GET /ping
      - If reachable, compare /ping version against Version constant (shared)
      - If version match AND healthy: return "already running <version>, healthy"
      - If version mismatch: update (see step 3)
      - If unreachable (socket missing, connection refused, timeout):
        update (see step 3) -- treats unhealthy agent same as version mismatch

3. Update path (version mismatch OR unhealthy agent):
   - SSH exec: curl/wget GitHub release to /usr/local/bin/devops.new
   - chmod +x devops.new
   - mv devops.new devops (atomic replace)
   - Write systemd unit (always, in case it changed)
   - systemctl daemon-reload && systemctl restart devops-agent
   - Wait 2s, then GET /ping to verify agent came up
   - Returns: "updated <old-version|unknown> -> <new-version>"
   - If post-update /ping fails: return error "agent installed but not responding"

GitHub release URL pattern:
    https://github.com/<owner>/<repo>/releases/download/v<Version>/devops-linux-amd64

The owner/repo are constants in the binary (or derived from go module path). The
Version constant determines which release tag to fetch.

The agent is stateless -- update always installs the latest version even if versions
match (unhealthy path). Restart is always safe.

Key Patterns (from codebase)

- Server struct: holds store + connPool + agentClient (like mem's Server with queries + sqlDB)
- Handler signature: func (s *server) toolName(args map[string]any) (string, bool)
- Tool dispatch: switch params.Name { case "devops_list": ... }
- JSON-RPC loop: channel-based stdin reader + response writer goroutine (mem pattern)
- Signal handling: context cancellation, second signal exits immediately (notab pattern)
- Handle notifications/initialized as no-op (mem pattern)
- Token-efficient JSON tags: svc, rt, compose, dir, bin (short field names in output)
- DB driver: sql.Open("sqlite", path) -- modernc, NOT "sqlite3" (follow hq, not mem)

Reference Files

- /Users/home/Documents/Code/terse-mcp/notab/main.go - signal handling, protocol types
- /Users/home/Documents/Code/terse-mcp/mem/main.go - Multi-tool MCP + SQLite, handler dispatch, response channel

Implementation Phases

Phase 1: Foundation (DB + CRUD + validation)

1. version.go - const Version (shared, no build tag)
2. protocol.go - JSON-RPC types from notab                              //go:build !agent
3. validate.go + validate_test.go - input validation functions          (shared, no tag)
4. db.go - schema, open/close, CRUD, logExec                           //go:build !agent
5. tools.go - all 13 tool definitions                                   //go:build !agent
6. handlers.go - devops_list, devops_add, devops_remove, devops_update  //go:build !agent
7. server.go - MCP loop with dispatch, notifications/initialized        //go:build !agent
8. main.go - entry point, MCP server startup                            //go:build !agent
9. db_test.go - CRUD tests with :memory: SQLite, trigger verification   //go:build !agent

Phase 2: Remote Agent

1. agent_main.go - unix socket listener, signal handling, RuntimeDirectory  //go:build agent
2. agent_handler.go - /ping, /exec, /service, /logs, /health, /discover    //go:build agent
3. agent_handler_test.go - unit tests (httptest over unix socket)           //go:build agent
4. Build: go build -tags agent -o devops-agent .

Phase 3: SSH + Connection Pool

1. ssh.go - connection pool, keepalive, embedded key, per-host key override  //go:build !agent
2. remote.go - HTTP-over-SSH-channel client (call + callStream)              //go:build !agent
3. handlers.go - add devops_status, devops_restart, devops_stop, devops_logs, devops_exec, devops_health
4. handlers_test.go - tests with mock agent (httptest unix socket server in-process)

Phase 4: Deploy + Bootstrap + Import

1. embed/ - all server configuration files (see Embedded Server Configuration)
2. agent_handler.go - POST /deploy endpoint with chunked streaming
3. handlers.go - devops_deploy (reads stream, stores result in DB)
4. handlers.go - devops_bootstrap (embedded configs + idempotent install/upgrade)
5. handlers.go - devops_import (calls /discover, merges overrides, inserts DB record)
6. Integration tests with mock agent

Phase 5: Build + Install

1. go.mod, justfile, .gitignore, CLAUDE.md
2. justfile targets:
   - build: go build -o devops . && GOOS=linux GOARCH=amd64 go build -tags agent -o devops-agent .
   - install: copies devops to /usr/local/bin/devops
   - agent-deploy HOST: scp devops-agent to HOST, restart service
3. Register: claude mcp add --transport stdio devops -- /usr/local/bin/devops
4. GitHub workflow: matrix build for both binaries, release assets per platform

Embedded Server Configuration

All server config files live in embed/ and are compiled into the local binary via
//go:embed (tagged //go:build !agent). Bootstrap writes them to the remote host over
SSH. Every file is idempotent -- safe to re-apply on every bootstrap run.

This means the common server setup path is deterministic and version-controlled.
The agent's /exec endpoint still exists for ad-hoc commands, but the baseline
configuration never requires arbitrary code execution.

    //go:embed embed/*
    var serverFiles embed.FS

Files and their destinations on the remote host:

embed/devops-agent.service -> /etc/systemd/system/devops-agent.service

    [Unit]
    Description=devops remote agent
    After=network.target

    [Service]
    Type=simple
    ExecStart=/usr/local/bin/devops agent
    RuntimeDirectory=devops-agent
    Restart=on-failure
    RestartSec=5

    [Install]
    WantedBy=multi-user.target

    RuntimeDirectory=devops-agent tells systemd to create /run/devops-agent/ with
    correct ownership before starting the process. The agent creates the socket inside it.

embed/ufw.sh -> executed via SSH (not installed)

    Firewall setup for Ubuntu 24 LTS. Idempotent -- checks current state before changing.
    - ufw default deny incoming
    - ufw default allow outgoing
    - ufw allow 22/tcp    (SSH)
    - ufw allow 80/tcp    (HTTP)
    - ufw allow 443/tcp   (HTTPS)
    - ufw --force enable
    Script checks `ufw status` first; skips if already configured correctly.

embed/sshd-hardening.conf -> /etc/ssh/sshd_config.d/90-devops.conf

    Drop-in config, Ubuntu 24 LTS sshd_config.d pattern. Applied changes:
    - PasswordAuthentication no
    - KbdInteractiveAuthentication no
    - PermitRootLogin prohibit-password   (key-only root access)
    - MaxAuthTries 3
    - X11Forwarding no
    After writing, bootstrap runs: systemctl reload sshd

embed/sysctl-tuning.conf -> /etc/sysctl.d/99-devops.conf

    Basic network tuning for a web-facing VPS:
    - net.core.somaxconn = 65535
    - net.ipv4.tcp_max_syn_backlog = 65535
    - net.ipv4.ip_local_port_range = 1024 65535
    - net.ipv4.tcp_tw_reuse = 1
    - net.ipv4.tcp_fin_timeout = 15
    - net.core.netdev_max_backlog = 65535
    - vm.swappiness = 10
    After writing, bootstrap runs: sysctl --system

embed/docker-daemon.json -> /etc/docker/daemon.json

    Docker daemon config. Overwritten on fresh install (not merged -- JSON merge in
    bash is fragile and jq is not guaranteed). If custom Docker settings are needed,
    add them after bootstrap; they will be preserved on agent updates since server
    configs are only applied on fresh install (not re-applied on update).
    Key settings:
    - "log-driver": "json-file"
    - "log-opts": {"max-size": "10m", "max-file": "3"}
    - "live-restore": true
    - "storage-driver": "overlay2"
    After writing, setup.sh runs: systemctl reload docker
    If Docker is not installed (command -v docker fails), setup.sh skips the reload.

embed/unattended-upgrades -> /etc/apt/apt.conf.d/50unattended-upgrades (partial)

    Enables automatic security updates only (not full dist-upgrade):
    - Unattended-Upgrade::Allowed-Origins { "${distro_id}:${distro_codename}-security"; };
    - Unattended-Upgrade::AutoFixInterruptedDpkg "true";
    - Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
    - Unattended-Upgrade::Remove-Unused-Dependencies "true";
    - Unattended-Upgrade::Automatic-Reboot "false";
    Bootstrap also ensures unattended-upgrades package is installed:
    dpkg -s unattended-upgrades || apt-get install -y unattended-upgrades

embed/setup.sh -> executed via SSH (not installed)

    Activator only -- does NOT write files. Bootstrap writes all files first,
    then setup.sh activates them:
    1. systemctl reload sshd                                    (sshd-hardening.conf)
    2. sysctl --system                                          (sysctl-tuning.conf)
    3. bash /tmp/devops-ufw.sh && rm /tmp/devops-ufw.sh         (ufw.sh)
    4. command -v docker && systemctl reload docker || true      (docker-daemon.json)
    5. dpkg -s unattended-upgrades || apt-get install -y unattended-upgrades
    6. systemctl daemon-reload                                  (devops-agent.service)
    Each step is idempotent. Each step logs what it did or skipped to stderr.
    setup.sh itself is embedded, piped to bash via SSH, not installed on the host.

Bootstrap applies embedded configs by:
1. Writing all config files to their destinations via SSH (cat > path << 'EOF' ... EOF)
   - sshd-hardening.conf -> /etc/ssh/sshd_config.d/90-devops.conf
   - sysctl-tuning.conf  -> /etc/sysctl.d/99-devops.conf
   - docker-daemon.json  -> /etc/docker/daemon.json (overwrite, see below)
   - unattended-upgrades -> /etc/apt/apt.conf.d/50unattended-upgrades
   - devops-agent.service -> /etc/systemd/system/devops-agent.service
   - ufw.sh -> /tmp/devops-ufw.sh (temporary, deleted by setup.sh)
2. Executing setup.sh which activates them (reloads, enables, applies)
3. Then proceeding with the agent binary install/upgrade (see Bootstrap Idempotency)

The config files are applied on every fresh install. On update (agent already exists
and healthy), only the agent binary and systemd unit are updated -- server configs are
not re-applied unless a force flag is passed. Rationale: server configs may have been
intentionally modified on the host; re-applying on every update would overwrite them.

Agent Deployment

The remote binary is named "devops" on the host (same binary name, started with "agent"
subcommand). The systemd unit runs "devops agent". Binary is downloaded from GitHub
releases on the remote host (not scp'd from local -- architectures differ).

Fresh install (via devops_bootstrap):
    1. Write all embedded config files via SSH
    2. Execute setup.sh via SSH (applies configs)
    3. Download agent binary from GitHub release
    4. systemctl daemon-reload && systemctl enable --now devops-agent

Updates (via devops_bootstrap idempotent path):
    1. Download new binary to /usr/local/bin/devops.new
    2. chmod +x, mv (atomic replace)
    3. Write systemd unit (always, in case it changed)
    4. systemctl daemon-reload && systemctl restart devops-agent

The agent is stateless -- restart is always safe.

Verification

1. go test -v -count=1 ./... - local tests (no build tag)
2. go test -v -count=1 -tags agent ./... - agent tests
3. just build - both binaries compile (devops for macOS, devops-agent for linux/amd64)
4. echo '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | ./devops - MCP handshake works
5. Add a test app, verify CRUD operations via MCP
6. ./devops-agent (or go run -tags agent . agent) for local dev testing
7. devops_bootstrap against a real VPS (manual, installs agent)
8. devops_bootstrap again (manual, verifies idempotent no-op)
9. devops_import against a real VPS with existing deployment (manual, after agent installed)
10. devops_status, devops_logs, devops_deploy against a real VPS (manual, after import)

Threat Model

- Local machine is fully trusted (MCP stdin is the trust boundary)
- SSH key authenticates the tunnel; unix socket permissions (0600 root) prevent local
  escalation on the VPS
- The agent validates service names and rejects malformed input server-side
- devops_exec allows arbitrary commands -- this is intentional for a single-user tool.
  The blast radius is equivalent to having SSH access, which the user already has
- ssh.InsecureIgnoreHostKey() is acceptable: MITM risk is low for direct-IP VPS
  connections on reputable providers, and the operational cost of host key management
  exceeds the security benefit for a single-user fleet
