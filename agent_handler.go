//go:build agent

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func registerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /ping", handlePing)
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /service", handleService)
	mux.HandleFunc("POST /logs", handleLogs)
	mux.HandleFunc("POST /health", handleHealth)
	mux.HandleFunc("POST /discover", handleDiscover)
	mux.HandleFunc("POST /deploy", handleDeploy)
}

// agentResponse is the standard response envelope.
type agentResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	Data   any    `json:"data,omitempty"`
	Output string `json:"output,omitempty"`
	Status int    `json:"status,omitempty"`
	Body   string `json:"body,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, agentResponse{OK: false, Error: msg})
}

func readBody(r *http.Request, v any) error {
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

// handleExec runs a single binary with arguments.
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

	// cmd must be a single binary name, not a shell string
	if strings.ContainsAny(req.Cmd, " \t|;&$`\\\"'(){}[]<>!#~") {
		writeError(w, http.StatusBadRequest, "cmd must be a single binary name, not a shell string")
		return
	}

	binPath, err := exec.LookPath(req.Cmd)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("command not found: %s", req.Cmd))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, req.Args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec failed: %v", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, agentResponse{
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode := exitErr.ExitCode()
			writeJSON(w, http.StatusOK, agentResponse{
				OK:     false,
				Stdout: stdout.String(),
				Stderr: stderr.String(),
				Exit:   &exitCode,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("exec failed: %v", err))
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

	writeJSON(w, http.StatusOK, agentResponse{OK: true})
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		writeJSON(w, http.StatusOK, agentResponse{
			OK:     false,
			Error:  fmt.Sprintf("logs failed: %v", err),
			Output: stderr.String(),
		})
		return
	}

	writeJSON(w, http.StatusOK, agentResponse{OK: true, Output: stdout.String()})
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

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(req.URL)
	if err != nil {
		writeJSON(w, http.StatusOK, agentResponse{
			OK:    false,
			Error: fmt.Sprintf("health check failed: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	var body bytes.Buffer
	body.ReadFrom(resp.Body)

	// Truncate body to 4KB
	bodyStr := body.String()
	if len(bodyStr) > 4096 {
		bodyStr = bodyStr[:4096]
	}

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
	Runtime     string           `json:"runtime"`
	ComposeFile string           `json:"compose_file,omitempty"`
	Services    []string         `json:"services,omitempty"`
	RepoURL     string           `json:"repo_url,omitempty"`
	Branch      string           `json:"branch,omitempty"`
	Containers  []containerInfo  `json:"containers,omitempty"`
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

	for i, command := range req.Commands {
		step := i + 1

		// Check if client disconnected before starting next command
		select {
		case <-deployCtx.Done():
			return
		default:
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
		cmd.Dir = req.Dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

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
			return // Stop on first failure
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
	}
}
