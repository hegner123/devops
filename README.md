# devops

MCP tool for managing deployed applications on Linux VPS servers. Single codebase, two binaries: a local MCP tool with SQLite metadata and persistent SSH connections, and a remote agent on each VPS.

## Installation

```bash
go build -o devops .
cp devops /usr/local/bin/
claude mcp add --transport stdio devops -- /usr/local/bin/devops
```

## Usage

```bash
# MCP server mode (default, used by Claude)
echo '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | ./devops

# Build remote agent
GOOS=linux GOARCH=amd64 go build -tags agent -o devops-agent .
```

