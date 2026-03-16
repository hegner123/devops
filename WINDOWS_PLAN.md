# Windows Compatibility Plan

Make the **local MCP binary** (`devops`) compile and run on Windows. The remote agent stays Linux-only.

## Already Windows-Compatible (No Changes Needed)

- SQLite (`modernc.org/sqlite`) — pure Go
- SSH (`golang.org/x/crypto/ssh`) — pure Go
- SSH channel forwarding to remote unix sockets (`client.Dial("unix", ...)`) — SSH protocol message, not local syscall
- `filepath.Join` — used throughout
- `os.MkdirAll` with Unix permission bits — Go ignores bits on Windows
- `os.UserHomeDir()` — already used in `dbPath()`
- All handler logic, protocol/JSON-RPC, embedded files

## Issues to Fix

### 1. Signal Handling (`server.go:29`)

`syscall.SIGTERM` is undefined on Windows. Windows only supports `os.Interrupt`.

### 2. SSH Key Path Discovery (`ssh.go:27-29`)

`os.Getenv("HOME")` is empty on Windows. Package-level var evaluates once at init — broken paths if HOME is empty. Error message at line 140 hardcodes `~/.ssh/`.

### 3. Database Path (`db.go:141-150`)

`$HOME/.local/share/devops/` is XDG convention. Windows equivalent: `%LOCALAPPDATA%\devops\`.

### 4. Path Validation (`validate.go:118-133`)

`s[0] != '/'` rejects Windows absolute paths like `C:\Users\...`. Only affects `key_path` (local path) — remote paths (`compose_file`, `deploy_dir`, `binary_path`) correctly require `/`.

### 5. Test Unix Sockets (`handlers_test.go:17-35`)

`mockAgent` uses `net.Listen("unix", ...)`. Windows supports AF_UNIX since 10 1803, but `t.TempDir()` paths can exceed the 108-char socket path limit.

### 6. Test Hardcoded Paths (`db_test.go`)

`TestDBPathEnvOverride` uses `/tmp/test-devops.db` — not valid on Windows.

### 7. Build System (`justfile`)

No Windows build target. `install` copies to `/usr/local/bin/`.

### 8. CI Workflows (`release.yml`, `test.yml`)

No Windows in release matrix or test runners.

---

## Phase 1: Core Platform Abstraction

**Goal:** Binary compiles and runs correctly on Windows. No behavioral change on Unix.

**New files:**

`platform_unix.go` (`//go:build !windows`):
- `func getDefaultKeyPaths() []string` — uses `os.UserHomeDir()` + `.ssh/id_ed25519`, `.ssh/id_rsa`
- `func dataDir() string` — returns `filepath.Join(home, ".local", "share", "devops")`
- `func platformSignals() []os.Signal` — returns `[]os.Signal{syscall.SIGINT, syscall.SIGTERM}`

`platform_windows.go` (`//go:build windows`):
- `func getDefaultKeyPaths() []string` — same `.ssh` subdir (Windows OpenSSH uses it too)
- `func dataDir() string` — uses `os.Getenv("LOCALAPPDATA")` with fallback to `os.UserHomeDir()`
- `func platformSignals() []os.Signal` — returns `[]os.Signal{os.Interrupt}`

**Modified files:**

`ssh.go`:
- Change `defaultKeyPaths` from package-level var to call `getDefaultKeyPaths()`
- Update error message to platform-neutral: `"no SSH key found: provide key_path per app or place a key in <home>/.ssh/"`

`server.go`:
- Replace `signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)` with `signal.Notify(sigChan, platformSignals()...)`
- Remove `"syscall"` import

`db.go`:
- Change `dbPath()` to use `dataDir()` instead of hardcoded `.local/share`

## Phase 2: Path Validation

**Goal:** Accept Windows local paths for `key_path` while keeping remote path validation strict.

`validate.go`:
- Add `validateLocalPath(s string) error` using `filepath.IsAbs(s)` — accepts both `/` and `C:\`
- Rename existing `validatePath` to `validateRemotePath` (keeps `/` prefix requirement)

`db.go`:
- In `validateApp()`, use `validateLocalPath` for `a.KeyPath`, `validateRemotePath` for `a.ComposeFile`, `a.DeployDir`, `a.BinaryPath`

## Phase 3: Test Compatibility

**Goal:** Tests pass on `windows-latest`.

`handlers_test.go`:
- Split `mockAgent` into build-tagged files:
  - `mockagent_unix_test.go` — current unix socket approach
  - `mockagent_windows_test.go` — TCP loopback (`net.Listen("tcp", "127.0.0.1:0")`)

`db_test.go`:
- Replace `/tmp/test-devops.db` with `filepath.Join(t.TempDir(), "test-devops.db")`

Agent tests (`agent_handler_test.go`, `agent_filter_test.go`):
- Already guarded by `//go:build agent` — exclude from Windows CI

## Phase 4: Build System and CI

**Goal:** Windows binaries are built, tested, and released.

`justfile`:
- Add `build-windows`: `GOOS=windows GOARCH=amd64 go build -o devops.exe .`
- Add `build-all` recipe

`.github/workflows/release.yml`:
- Add to matrix: `{ goos: windows, goarch: amd64, binary: devops.exe, asset: devops-windows-amd64.exe }`

`.github/workflows/test.yml`:
- Add `windows-latest` to test matrix
- Agent tests (`-tags agent`) only on `ubuntu-latest`

## Phase 5: Documentation

`tools.go`:
- Update `key_path` descriptions to platform-neutral wording

`CLAUDE.md`:
- Add Windows installation and data directory location
- Document MCP registration on Windows

---

## File Change Summary

| File | Change | Phase |
|------|--------|-------|
| `platform_unix.go` (new) | defaultKeyPaths, dataDir, platformSignals | 1 |
| `platform_windows.go` (new) | defaultKeyPaths, dataDir, platformSignals | 1 |
| `ssh.go` | Use platform funcs, fix error msg | 1 |
| `server.go` | Use platformSignals(), drop syscall import | 1 |
| `db.go` | Use dataDir(), use validateLocalPath for key_path | 1, 2 |
| `validate.go` | Add validateLocalPath, rename validatePath | 2 |
| `mockagent_unix_test.go` (new) | Unix socket mock agent | 3 |
| `mockagent_windows_test.go` (new) | TCP loopback mock agent | 3 |
| `handlers_test.go` | Extract mock agent to platform files | 3 |
| `db_test.go` | Fix hardcoded /tmp paths | 3 |
| `justfile` | Add Windows build targets | 4 |
| `release.yml` | Add windows/amd64 to matrix | 4 |
| `test.yml` | Add windows-latest runner | 4 |
| `tools.go` | Platform-neutral descriptions | 5 |
| `CLAUDE.md` | Windows docs | 5 |
