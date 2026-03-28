package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxOutputBytes = 10 << 20 // 10MB

// limitedBuffer wraps bytes.Buffer with a maximum size. Writes beyond the
// limit are silently discarded (no error), so command execution is not
// interrupted by verbose output.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newLimitedBuffer(max int) *limitedBuffer {
	return &limitedBuffer{max: max}
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	remaining := lb.max - lb.buf.Len()
	if remaining <= 0 {
		return len(p), nil // silently discard
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	lb.buf.Write(p)
	return len(p), nil
}

func (lb *limitedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
}

// ssrfCheckEnabled controls whether health checks block private IPs.
// Tests may set this to false to allow httptest.NewServer on 127.0.0.1.
var ssrfCheckEnabled = true

// privateIPBlocks contains CIDR ranges that must not be targeted by health checks.
var privateIPBlocks []*net.IPNet

func init() {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"fd00::/8",
		"::1/128",
	}
	for _, cidr := range cidrs {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("bad CIDR %q: %v", cidr, err))
		}
		privateIPBlocks = append(privateIPBlocks, block)
	}
}

func isPrivateIP(ip net.IP) bool {
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func registerHandlers(mux *http.ServeMux, st *store) {
	mux.HandleFunc("GET /ping", handlePing)
	mux.HandleFunc("GET /filters", handleFilters)
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /service", handleService)
	mux.HandleFunc("POST /logs", handleLogs)
	mux.HandleFunc("POST /health", handleHealth)
	mux.HandleFunc("POST /discover", handleDiscover)
	mux.HandleFunc("POST /deploy", handleDeploy)
	if st != nil {
		mux.HandleFunc("POST /sync-apps", syncAppsHandler(st))
		mux.HandleFunc("POST /redeploy", redeployHandler(st))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, AgentResponse{OK: false, Error: msg})
}

func readBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1MB limit
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// handlePing returns agent version and hostname.
func handlePing(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  Version,
		"hostname": hostname,
	})
}

// handleExec runs a command on the host. If cmd contains shell syntax (spaces,
// pipes, etc.) it runs via sh -c. Otherwise it resolves the binary and passes
// args directly.
func handleExec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cmd  string   `json:"cmd"`
		Args []string `json:"args"`
	}
	if err := readBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}

	// Build full command string for filter check
	full := req.Cmd
	for _, a := range req.Args {
		full += " " + a
	}
	if err := checkCommand(full); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	isShell := strings.ContainsAny(req.Cmd, " \t|;&$`\\\"'(){}[]<>!#~/")
	if isShell {
		// Shell string: run via sh -c (args appended to command string)
		cmd = exec.CommandContext(ctx, "sh", "-c", full)
	} else {
		binPath, err := exec.LookPath(req.Cmd)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("command not found: %s", req.Cmd))
			return
		}
		cmd = exec.CommandContext(ctx, binPath, req.Args...)
	}

	stdout := newLimitedBuffer(maxOutputBytes)
	stderr := newLimitedBuffer(maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec failed: %v", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, AgentResponse{
		OK:     exitCode == 0,
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Exit:   &exitCode,
	})
}

// handleService manages systemd or docker compose services.
func handleService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action      string `json:"action"`
		Name        string `json:"name"`
		Runtime     string `json:"runtime"`
		ComposeFile string `json:"compose_file"`
	}
	if err := readBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Action == "" || req.Name == "" || req.Runtime == "" {
		writeError(w, http.StatusBadRequest, "action, name, and runtime are required")
		return
	}
	if err := validateServiceName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRuntime(req.Runtime); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Runtime == "docker" {
		if req.ComposeFile == "" {
			writeError(w, http.StatusBadRequest, "compose_file is required for docker runtime")
			return
		}
		if err := validatePath(req.ComposeFile); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("compose_file: %v", err))
			return
		}
		if _, err := os.Stat(req.ComposeFile); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("compose_file not found: %s", req.ComposeFile))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	switch req.Runtime {
	case "systemd":
		switch req.Action {
		case "status":
			cmd = exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState,MainPID,MemoryCurrent", req.Name)
		case "restart":
			cmd = exec.CommandContext(ctx, "systemctl", "restart", req.Name)
		case "stop":
			cmd = exec.CommandContext(ctx, "systemctl", "stop", req.Name)
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action: %s", req.Action))
			return
		}
	case "docker":
		switch req.Action {
		case "status":
			cmd = exec.CommandContext(ctx, "docker", "compose", "-f", req.ComposeFile, "ps", req.Name, "--format", "json")
		case "restart":
			cmd = exec.CommandContext(ctx, "docker", "compose", "-f", req.ComposeFile, "restart", req.Name)
		case "stop":
			cmd = exec.CommandContext(ctx, "docker", "compose", "-f", req.ComposeFile, "stop", req.Name)
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action: %s", req.Action))
			return
		}
	}

	stdout := newLimitedBuffer(maxOutputBytes)
	stderr := newLimitedBuffer(maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode := exitErr.ExitCode()
			errMsg := stderr.String()
			if errMsg == "" {
				errMsg = stdout.String()
			}
			if errMsg == "" {
				errMsg = fmt.Sprintf("%s %s %s exited %d", req.Runtime, req.Action, req.Name, exitCode)
			}
			writeJSON(w, http.StatusOK, AgentResponse{
				OK:     false,
				Error:  errMsg,
				Stdout: stdout.String(),
				Stderr: stderr.String(),
				Exit:   &exitCode,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("%s %s %s: %v", req.Runtime, req.Action, req.Name, err))
		return
	}

	if req.Action == "status" {
		// Return structured status data
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"data": parseStatusOutput(req.Runtime, stdout.String()),
		})
		return
	}

	writeJSON(w, http.StatusOK, AgentResponse{OK: true})
}

func parseStatusOutput(runtime, output string) map[string]string {
	result := make(map[string]string)
	switch runtime {
	case "systemd":
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				result[parts[0]] = parts[1]
			}
		}
	case "docker":
		// docker compose ps --format json returns JSON; pass through as-is
		result["json"] = strings.TrimSpace(output)
	}
	return result
}

// handleLogs retrieves service logs.
func handleLogs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Lines       int    `json:"lines"`
		Runtime     string `json:"runtime"`
		ComposeFile string `json:"compose_file"`
	}
	if err := readBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Runtime == "" {
		writeError(w, http.StatusBadRequest, "name and runtime are required")
		return
	}
	if err := validateServiceName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Lines <= 0 {
		req.Lines = 50
	}
	if req.Lines > 10000 {
		req.Lines = 10000
	}
	linesStr := strconv.Itoa(req.Lines)

	if req.Runtime == "docker" {
		if req.ComposeFile == "" {
			writeError(w, http.StatusBadRequest, "compose_file is required for docker runtime")
			return
		}
		if err := validatePath(req.ComposeFile); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("compose_file: %v", err))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch req.Runtime {
	case "systemd":
		cmd = exec.CommandContext(ctx, "journalctl", "-u", req.Name, "-n", linesStr, "--no-pager")
	case "docker":
		cmd = exec.CommandContext(ctx, "docker", "compose", "-f", req.ComposeFile, "logs", "--tail", linesStr, req.Name)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown runtime: %s", req.Runtime))
		return
	}

	stdout := newLimitedBuffer(maxOutputBytes)
	stderr := newLimitedBuffer(maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		writeJSON(w, http.StatusOK, AgentResponse{
			OK:     false,
			Error:  fmt.Sprintf("logs failed: %v", err),
			Output: stderr.String(),
		})
		return
	}

	writeJSON(w, http.StatusOK, AgentResponse{OK: true, Output: stdout.String()})
}

// handleHealth checks an HTTP health endpoint.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := readBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}

	// SSRF protection: only allow http/https and block private IPs
	parsed, err := url.Parse(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid URL: %v", err))
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		writeError(w, http.StatusBadRequest, "health check blocked: only http and https schemes are allowed")
		return
	}

	if ssrfCheckEnabled {
		hostname := parsed.Hostname()
		ips, err := net.LookupIP(hostname)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("health check blocked: cannot resolve hostname %q: %v", hostname, err))
			return
		}
		for _, ip := range ips {
			if isPrivateIP(ip) {
				writeError(w, http.StatusForbidden, "health check blocked: URL resolves to private IP range")
				return
			}
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(req.URL)
	if err != nil {
		writeJSON(w, http.StatusOK, AgentResponse{
			OK:    false,
			Error: fmt.Sprintf("health check failed: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyStr := string(bodyBytes)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     resp.StatusCode >= 200 && resp.StatusCode < 400,
		"status": resp.StatusCode,
		"body":   bodyStr,
	})
}

// handleDiscover inspects a directory for deployment metadata.
func handleDiscover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := readBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validatePath(req.Dir); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("dir: %v", err))
		return
	}
	info, err := os.Stat(req.Dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("dir not found: %s", req.Dir))
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("not a directory: %s", req.Dir))
		return
	}

	data := discoverDir(r.Context(), req.Dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"data": data,
	})
}

type discoveryResult struct {
	Runtime     string          `json:"runtime"`
	ComposeFile string          `json:"compose_file,omitempty"`
	Services    []string        `json:"services,omitempty"`
	RepoURL     string          `json:"repo_url,omitempty"`
	Branch      string          `json:"branch,omitempty"`
	Containers  []containerInfo `json:"containers,omitempty"`
}

type containerInfo struct {
	Name   string `json:"name"`
	State  string `json:"state,omitempty"`
	Status string `json:"status,omitempty"`
	Ports  string `json:"ports,omitempty"`
}

func discoverDir(ctx context.Context, dir string) *discoveryResult {
	result := &discoveryResult{Runtime: "unknown"}

	// 1. Check for compose file
	composeNames := []string{"compose.yml", "docker-compose.yml", "compose.yaml", "docker-compose.yaml"}
	for _, name := range composeNames {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			result.Runtime = "docker"
			result.ComposeFile = path
			break
		}
	}

	// 2. Get service names from compose
	if result.ComposeFile != "" {
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx2, "docker", "compose", "-f", result.ComposeFile, "config", "--services").Output()
		if err == nil {
			for _, svc := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				svc = strings.TrimSpace(svc)
				if svc != "" {
					result.Services = append(result.Services, svc)
				}
			}
		}
	}

	// 3. Check for git repo
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx2, "git", "-C", dir, "config", "--get", "remote.origin.url").Output(); err == nil {
			result.RepoURL = strings.TrimSpace(string(out))
		}
		if out, err := exec.CommandContext(ctx2, "git", "-C", dir, "branch", "--show-current").Output(); err == nil {
			result.Branch = strings.TrimSpace(string(out))
		}
	}

	// 4. Check running containers
	if result.ComposeFile != "" {
		ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx2, "docker", "compose", "-f", result.ComposeFile, "ps", "--format", "json").Output()
		if err == nil {
			result.Containers = parseContainerJSON(out)
		}
	}

	// 5. Check for systemd units (only if no compose file found)
	if result.ComposeFile == "" {
		dirName := filepath.Base(dir)

		// Check for .service files in the directory
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".service") {
					result.Runtime = "systemd"
					break
				}
			}
		}

		// Check if a service matching the directory name is active
		if result.Runtime == "unknown" {
			ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := exec.CommandContext(ctx2, "systemctl", "is-active", dirName).Run(); err == nil {
				result.Runtime = "systemd"
			}
		}
	}

	return result
}

func parseContainerJSON(data []byte) []containerInfo {
	// docker compose ps --format json can return one JSON object per line
	// or a JSON array depending on version
	var containers []containerInfo

	// Try as array first
	var arr []map[string]any
	if json.Unmarshal(data, &arr) == nil {
		for _, obj := range arr {
			containers = append(containers, containerFromMap(obj))
		}
		return containers
	}

	// Try line-by-line JSON objects
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			containers = append(containers, containerFromMap(obj))
		}
	}
	return containers
}

func containerFromMap(obj map[string]any) containerInfo {
	ci := containerInfo{}
	if v, ok := obj["Name"].(string); ok {
		ci.Name = v
	} else if v, ok := obj["name"].(string); ok {
		ci.Name = v
	} else if v, ok := obj["Service"].(string); ok {
		ci.Name = v
	}
	if v, ok := obj["State"].(string); ok {
		ci.State = v
	} else if v, ok := obj["state"].(string); ok {
		ci.State = v
	}
	if v, ok := obj["Status"].(string); ok {
		ci.Status = v
	} else if v, ok := obj["status"].(string); ok {
		ci.Status = v
	}
	if v, ok := obj["Ports"].(string); ok {
		ci.Ports = v
	} else if v, ok := obj["ports"].(string); ok {
		ci.Ports = v
	} else if v, ok := obj["Publishers"].(string); ok {
		ci.Ports = v
	}
	return ci
}

// handleDeploy runs deploy commands with chunked streaming output.
func handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir      string   `json:"dir"`
		Commands []string `json:"commands"`
	}
	if err := readBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validatePath(req.Dir); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("dir: %v", err))
		return
	}
	info, err := os.Stat(req.Dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("dir not found: %s", req.Dir))
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("not a directory: %s", req.Dir))
		return
	}
	if len(req.Commands) == 0 {
		writeError(w, http.StatusBadRequest, "commands array is required and must not be empty")
		return
	}

	executeDeploy(w, r, req.Dir, req.Commands)
}

// executeDeploy runs deploy commands with NDJSON streaming output.
// Shared by handleDeploy (remote-initiated) and redeployHandler (container-initiated).
// Returns the last status ("done", "failed", "blocked") and accumulated output.
func executeDeploy(w http.ResponseWriter, r *http.Request, dir string, commands []string) (lastStatus string, output string) {
	// Overall 30m timeout
	deployCtx, deployCancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer deployCancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	enc := json.NewEncoder(w)

	writeChunk := func(v any) {
		enc.Encode(v)
		if canFlush {
			flusher.Flush()
		}
	}

	var allOutput strings.Builder
	lastStatus = "done"

	for i, command := range commands {
		step := i + 1

		// Check if client disconnected before starting next command
		select {
		case <-deployCtx.Done():
			lastStatus = "failed"
			return lastStatus, allOutput.String()
		default:
		}

		if err := checkCommand(command); err != nil {
			writeChunk(map[string]any{
				"step":   step,
				"cmd":    command,
				"status": "blocked",
				"error":  err.Error(),
			})
			lastStatus = "blocked"
			return lastStatus, allOutput.String()
		}

		writeChunk(map[string]any{
			"step":   step,
			"cmd":    command,
			"status": "running",
		})

		// Per-command 5m timeout
		cmdCtx, cmdCancel := context.WithTimeout(deployCtx, 5*time.Minute)
		start := time.Now()

		cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
		cmd.Dir = dir
		stdout := newLimitedBuffer(maxOutputBytes)
		stderr := newLimitedBuffer(maxOutputBytes)
		cmd.Stdout = stdout
		cmd.Stderr = stderr

		err := cmd.Run()
		elapsed := fmt.Sprintf("%.1fs", time.Since(start).Seconds())
		cmdCancel()

		if err != nil {
			exitCode := 1
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
			writeChunk(map[string]any{
				"step":    step,
				"cmd":     command,
				"status":  "failed",
				"exit":    exitCode,
				"stdout":  stdout.String(),
				"stderr":  stderr.String(),
				"elapsed": elapsed,
			})
			fmt.Fprintf(&allOutput, "step %d: %s (exit %d, %s)\n%s%s", step, command, exitCode, elapsed, stdout.String(), stderr.String())
			lastStatus = "failed"
			return lastStatus, allOutput.String()
		}

		writeChunk(map[string]any{
			"step":    step,
			"cmd":     command,
			"status":  "done",
			"exit":    0,
			"stdout":  stdout.String(),
			"stderr":  stderr.String(),
			"elapsed": elapsed,
		})
		fmt.Fprintf(&allOutput, "step %d: %s (exit 0, %s)\n", step, command, elapsed)
	}

	return lastStatus, allOutput.String()
}

// syncAppsHandler receives app configs from the local devops and upserts them
// into the agent's SQLite DB. Apps not in the incoming set are pruned.
func syncAppsHandler(st *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Apps []App `json:"apps"`
		}
		if err := readBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(req.Apps) == 0 {
			writeError(w, http.StatusBadRequest, "apps array is required and must not be empty")
			return
		}

		names := make([]string, 0, len(req.Apps))
		for _, a := range req.Apps {
			app := a
			if err := st.upsertApp(&app); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("upsert app %q: %v", app.Name, err))
				return
			}
			names = append(names, app.Name)
		}

		removed, err := st.deleteAppsNotIn(names)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("prune stale apps: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"synced":  len(req.Apps),
			"removed": removed,
		})
	}
}

const redeployBackoff = 5 * time.Minute

// redeployHandler allows a container to trigger its own redeployment.
// The container POSTs {"name": "appname"} and the agent reads deploy commands
// from its local DB and executes them.
func redeployHandler(st *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name  string `json:"name"`
			Force bool   `json:"force"`
		}
		if err := readBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}

		app, err := st.getApp(req.Name)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("app %q not found in agent DB — run devops_app_sync first", req.Name))
			return
		}

		// Retry protection: reject if last deploy failed recently
		if !req.Force && app.LastDeployOK.Valid && app.LastDeployOK.Int64 == 0 && app.LastDeployAt.Valid {
			lastDeploy, parseErr := time.Parse("2006-01-02 15:04:05", app.LastDeployAt.String)
			if parseErr == nil && time.Since(lastDeploy) < redeployBackoff {
				retryAfter := lastDeploy.Add(redeployBackoff)
				writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
					"last deploy failed at %s; retry after %s or pass force=true",
					app.LastDeployAt.String, retryAfter.Format(time.RFC3339),
				))
				return
			}
		}

		var commands []string
		if err := json.Unmarshal([]byte(app.DeployCommands), &commands); err != nil || len(commands) == 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no deploy commands configured for app %q", req.Name))
			return
		}

		if app.DeployDir == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no deploy_dir configured for app %q", req.Name))
			return
		}

		info, statErr := os.Stat(app.DeployDir)
		if statErr != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("deploy_dir not found: %s", app.DeployDir))
			return
		}
		if !info.IsDir() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("deploy_dir is not a directory: %s", app.DeployDir))
			return
		}

		lastStatus, deployOutput := executeDeploy(w, r, app.DeployDir, commands)

		// Truncate output to 4KB for storage
		if len(deployOutput) > 4096 {
			deployOutput = deployOutput[len(deployOutput)-4096:]
		}

		ok := sql.NullInt64{Valid: true, Int64: 1}
		if lastStatus == "failed" || lastStatus == "blocked" {
			ok.Int64 = 0
		}
		st.updateDeployResult(req.Name, ok, deployOutput)
	}
}

// handleFilters returns the current active filter configuration.
func handleFilters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, activeFilters())
}
