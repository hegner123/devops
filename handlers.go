//go:build !agent

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Phase 1: local CRUD handlers (devops_list, devops_add, devops_remove, devops_update)
// Phase 3/4 will add remote agent handlers

func (s *server) devopsList(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)

	apps, err := s.store.listApps(host)
	if err != nil {
		return fmt.Sprintf("failed to list apps: %v", err), true
	}

	if len(apps) == 0 {
		if host != "" {
			return fmt.Sprintf("no apps on host %s", host), false
		}
		return "no apps registered", false
	}

	type appSummary struct {
		Name    string `json:"name"`
		Host    string `json:"host"`
		Svc     string `json:"svc"`
		RT      string `json:"rt"`
		Deploy  string `json:"deploy,omitempty"`
		Status  string `json:"status,omitempty"`
	}

	summaries := make([]appSummary, len(apps))
	for i, a := range apps {
		summary := appSummary{
			Name: a.Name,
			Host: a.Host,
			Svc:  a.ServiceName,
			RT:   a.Runtime,
		}
		if a.LastDeployAt.Valid {
			summary.Deploy = a.LastDeployAt.String
		}
		if a.LastDeployOK.Valid {
			if a.LastDeployOK.Int64 == 1 {
				summary.Status = "ok"
			} else {
				summary.Status = "fail"
			}
		}
		summaries[i] = summary
	}

	out, err := json.Marshal(summaries)
	if err != nil {
		return fmt.Sprintf("failed to marshal apps: %v", err), true
	}
	return string(out), false
}

func (s *server) devopsAdd(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	host, _ := args["host"].(string)
	serviceName, _ := args["service_name"].(string)

	if name == "" || host == "" || serviceName == "" {
		return "name, host, and service_name are required", true
	}

	app := &App{
		Name:        name,
		Host:        host,
		Port:        intArg(args, "port", 22),
		User:        strArg(args, "user", "root"),
		Runtime:     strArg(args, "runtime", "docker"),
		ServiceName: serviceName,
		ComposeFile: strArg(args, "compose_file", ""),
		RepoURL:     strArg(args, "repo_url", ""),
		Branch:      strArg(args, "branch", "main"),
		DeployDir:   strArg(args, "deploy_dir", ""),
		BinaryPath:  strArg(args, "binary_path", ""),
		HealthCheckURL: strArg(args, "health_check_url", ""),
		DeployCommands: strArg(args, "deploy_commands", "[]"),
		Notes:       strArg(args, "notes", ""),
		KeyPath:     strArg(args, "key_path", ""),
	}

	if err := s.store.addApp(app); err != nil {
		return err.Error(), true
	}

	return fmt.Sprintf("added app %q on %s", name, host), false
}

func (s *server) devopsRemove(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	if err := s.store.removeApp(name); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("removed app %q", name), false
}

func (s *server) devopsUpdate(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	// Read-modify-write: apply only provided fields
	if v, ok := args["host"].(string); ok && v != "" {
		app.Host = v
	}
	if v, ok := args["port"]; ok {
		app.Port = toInt(v, app.Port)
	}
	if v, ok := args["user"].(string); ok && v != "" {
		app.User = v
	}
	if v, ok := args["runtime"].(string); ok && v != "" {
		app.Runtime = v
	}
	if v, ok := args["service_name"].(string); ok && v != "" {
		app.ServiceName = v
	}
	if v, ok := args["compose_file"].(string); ok {
		app.ComposeFile = v
	}
	if v, ok := args["repo_url"].(string); ok {
		app.RepoURL = v
	}
	if v, ok := args["branch"].(string); ok && v != "" {
		app.Branch = v
	}
	if v, ok := args["deploy_dir"].(string); ok {
		app.DeployDir = v
	}
	if v, ok := args["binary_path"].(string); ok {
		app.BinaryPath = v
	}
	if v, ok := args["health_check_url"].(string); ok {
		app.HealthCheckURL = v
	}
	if v, ok := args["deploy_commands"].(string); ok && v != "" {
		app.DeployCommands = v
	}
	if v, ok := args["notes"].(string); ok {
		app.Notes = v
	}
	if v, ok := args["key_path"].(string); ok {
		app.KeyPath = v
	}

	if err := s.store.updateApp(app); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("updated app %q", name), false
}

// Phase 3: remote agent handlers

func (s *server) devopsStatus(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	resp, err := s.agent.call(s.ctx, app, "/service", map[string]any{
		"action":       "status",
		"name":         app.ServiceName,
		"runtime":      app.Runtime,
		"compose_file": app.ComposeFile,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}
	if !resp.OK {
		return fmt.Sprintf("status failed: %s", resp.Error), true
	}

	out, err := json.Marshal(resp.Data)
	if err != nil {
		return fmt.Sprintf("marshal data: %v", err), true
	}
	return string(out), false
}

func (s *server) devopsRestart(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	resp, err := s.agent.call(s.ctx, app, "/service", map[string]any{
		"action":       "restart",
		"name":         app.ServiceName,
		"runtime":      app.Runtime,
		"compose_file": app.ComposeFile,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}
	if !resp.OK {
		return fmt.Sprintf("restart failed: %s", resp.Error), true
	}
	return fmt.Sprintf("restarted %s on %s", app.ServiceName, app.Host), false
}

func (s *server) devopsStop(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	resp, err := s.agent.call(s.ctx, app, "/service", map[string]any{
		"action":       "stop",
		"name":         app.ServiceName,
		"runtime":      app.Runtime,
		"compose_file": app.ComposeFile,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}
	if !resp.OK {
		return fmt.Sprintf("stop failed: %s", resp.Error), true
	}
	return fmt.Sprintf("stopped %s on %s", app.ServiceName, app.Host), false
}

func (s *server) devopsLogs(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	lines := intArg(args, "lines", 50)

	resp, err := s.agent.call(s.ctx, app, "/logs", map[string]any{
		"name":         app.ServiceName,
		"lines":        lines,
		"runtime":      app.Runtime,
		"compose_file": app.ComposeFile,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}
	if !resp.OK {
		return fmt.Sprintf("logs failed: %s", resp.Error), true
	}
	return resp.Output, false
}

func (s *server) devopsExec(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	command, _ := args["command"].(string)
	if name == "" || command == "" {
		return "name and command are required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	resp, err := s.agent.call(s.ctx, app, "/exec", map[string]any{
		"cmd": command,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}

	// Log to exec_log
	exitCode := 0
	if resp.Exit != nil {
		exitCode = *resp.Exit
	}
	if logErr := s.store.logExec(app.Name, app.Host, command, "", exitCode); logErr != nil {
		fmt.Fprintf(os.Stderr, "log exec: %v\n", logErr)
	}

	if !resp.OK {
		result := resp.Stderr
		if result == "" {
			result = resp.Error
		}
		return fmt.Sprintf("exit %d: %s", exitCode, result), true
	}
	return resp.Stdout, false
}

func (s *server) devopsHealth(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	if app.HealthCheckURL == "" {
		return "no health_check_url configured for this app", true
	}

	resp, err := s.agent.call(s.ctx, app, "/health", map[string]any{
		"url": app.HealthCheckURL,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return fmt.Sprintf("marshal response: %v", err), true
	}
	return string(out), false
}

// Phase 4: deploy, bootstrap, import handlers

func (s *server) devopsDeploy(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	// Get commands: override or from DB
	var commands []string
	if cmdsStr, ok := args["commands"].(string); ok && cmdsStr != "" {
		if err := json.Unmarshal([]byte(cmdsStr), &commands); err != nil {
			return fmt.Sprintf("invalid commands JSON: %v", err), true
		}
	} else {
		if app.DeployCommands == "" || app.DeployCommands == "[]" {
			return "no deploy commands configured and none provided", true
		}
		if err := json.Unmarshal([]byte(app.DeployCommands), &commands); err != nil {
			return fmt.Sprintf("invalid stored deploy_commands: %v", err), true
		}
	}

	if len(commands) == 0 {
		return "no deploy commands configured and none provided", true
	}

	if app.DeployDir == "" {
		return "deploy_dir is not configured for this app", true
	}

	stream, err := s.agent.callStream(s.ctx, app, "/deploy", map[string]any{
		"dir":      app.DeployDir,
		"commands": commands,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}
	defer stream.Close()

	// Read NDJSON stream
	type deployChunk struct {
		Step    int    `json:"step"`
		Cmd     string `json:"cmd"`
		Status  string `json:"status"`
		Exit    int    `json:"exit"`
		Stdout  string `json:"stdout"`
		Stderr  string `json:"stderr"`
		Elapsed string `json:"elapsed"`
		Error   string `json:"error"`
	}

	var lastChunk deployChunk
	var allOutput strings.Builder
	decoder := json.NewDecoder(stream)
	stepCount := 0

	for {
		var chunk deployChunk
		if err := decoder.Decode(&chunk); err != nil {
			if err == io.EOF {
				break
			}
			// Connection lost mid-stream
			output := allOutput.String()
			if len(output) > 4096 {
				output = output[len(output)-4096:]
			}
			updateErr := s.store.updateDeployResult(name, sql.NullInt64{}, output)
			if updateErr != nil {
				fmt.Fprintf(os.Stderr, "update deploy result: %v\n", updateErr)
			}
			return fmt.Sprintf("connection lost during deploy at step %d, server state unknown -- use devops_status to check", stepCount), true
		}

		lastChunk = chunk

		if chunk.Status == "blocked" {
			stepCount = chunk.Step
			allOutput.WriteString(fmt.Sprintf("step %d: %s (BLOCKED)\n", chunk.Step, chunk.Cmd))
			if chunk.Error != "" {
				allOutput.WriteString(chunk.Error)
				allOutput.WriteString("\n")
			}
		}

		if chunk.Status == "done" || chunk.Status == "failed" {
			stepCount = chunk.Step
			allOutput.WriteString(fmt.Sprintf("step %d: %s (%s, exit %d)\n", chunk.Step, chunk.Cmd, chunk.Elapsed, chunk.Exit))
			if chunk.Stdout != "" {
				allOutput.WriteString(chunk.Stdout)
				if !strings.HasSuffix(chunk.Stdout, "\n") {
					allOutput.WriteString("\n")
				}
			}
			if chunk.Stderr != "" {
				allOutput.WriteString(chunk.Stderr)
				if !strings.HasSuffix(chunk.Stderr, "\n") {
					allOutput.WriteString("\n")
				}
			}
		}
	}

	// Tail-truncate output to 4KB
	output := allOutput.String()
	if len(output) > 4096 {
		output = output[len(output)-4096:]
	}

	if lastChunk.Status == "blocked" {
		updateErr := s.store.updateDeployResult(name, sql.NullInt64{Valid: true, Int64: 0}, output)
		if updateErr != nil {
			fmt.Fprintf(os.Stderr, "update deploy result: %v\n", updateErr)
		}
		return fmt.Sprintf("deploy blocked at step %d (%s): %s", lastChunk.Step, lastChunk.Cmd, lastChunk.Error), true
	}

	if lastChunk.Status == "failed" {
		updateErr := s.store.updateDeployResult(name, sql.NullInt64{Valid: true, Int64: 0}, output)
		if updateErr != nil {
			fmt.Fprintf(os.Stderr, "update deploy result: %v\n", updateErr)
		}
		errMsg := lastChunk.Stderr
		if errMsg == "" {
			errMsg = lastChunk.Stdout
		}
		return fmt.Sprintf("deploy failed at step %d (%s): exit %d\n%s", lastChunk.Step, lastChunk.Cmd, lastChunk.Exit, errMsg), true
	}

	updateErr := s.store.updateDeployResult(name, sql.NullInt64{Valid: true, Int64: 1}, output)
	if updateErr != nil {
		fmt.Fprintf(os.Stderr, "update deploy result: %v\n", updateErr)
	}
	return fmt.Sprintf("deployed %s: %d steps completed", name, stepCount), false
}

// Release URL constants derived from module path.
const (
	releaseOwner = "hegner123"
	releaseRepo  = "devops"
)

func releaseURL() string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/devops-linux-amd64",
		releaseOwner, releaseRepo, Version)
}

// configFiles maps embedded file names to their remote destinations.
var configFiles = []struct {
	embedName  string
	remotePath string
}{
	{"embed/sshd-hardening.conf", "/etc/ssh/sshd_config.d/90-devops.conf"},
	{"embed/sysctl-tuning.conf", "/etc/sysctl.d/99-devops.conf"},
	{"embed/docker-daemon.json", "/etc/docker/daemon.json"},
	{"embed/unattended-upgrades", "/etc/apt/apt.conf.d/50unattended-upgrades"},
	{"embed/devops-agent.service", "/etc/systemd/system/devops-agent.service"},
	{"embed/ufw.sh", "/tmp/devops-ufw.sh"},
}

func (s *server) devopsBootstrap(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)
	if host == "" {
		return "host is required", true
	}

	user := strArg(args, "user", "root")
	port := intArg(args, "port", 22)
	keyPath := strArg(args, "key_path", "")

	// Create temporary App for SSH pool
	app := &App{
		Host:    host,
		Port:    port,
		User:    user,
		KeyPath: keyPath,
	}

	client, err := s.pool.get(s.ctx, app)
	if err != nil {
		return fmt.Sprintf("ssh connect: %v", err), true
	}

	// Check if agent binary exists
	_, _, exitCode, err := sshExec(client, "test -f /usr/local/bin/devops")
	if err != nil {
		return fmt.Sprintf("ssh exec: %v", err), true
	}

	if exitCode != 0 {
		// Fresh install: apply configs, download binary, start service
		return s.bootstrapFreshInstall(client)
	}

	// Agent binary exists -- check if reachable and version
	version, pingErr := agentPing(client)
	if pingErr == nil && version == Version {
		return fmt.Sprintf("already running %s, healthy", version), false
	}

	// Version mismatch or unreachable -- update
	oldVersion := version
	if oldVersion == "" {
		oldVersion = "unknown"
	}
	return s.bootstrapUpdate(client, oldVersion)
}

func (s *server) bootstrapFreshInstall(client *ssh.Client) (string, bool) {
	// 1. Write all embedded config files to remote host
	for _, cf := range configFiles {
		content, err := readEmbedFile(cf.embedName)
		if err != nil {
			return fmt.Sprintf("read embedded %s: %v", cf.embedName, err), true
		}
		if err := sshWriteFile(client, cf.remotePath, content); err != nil {
			return fmt.Sprintf("write %s: %v", cf.remotePath, err), true
		}
	}

	// 2. Execute setup.sh to activate configs
	setupScript, err := readEmbedFile("embed/setup.sh")
	if err != nil {
		return fmt.Sprintf("read setup.sh: %v", err), true
	}
	_, stderr, exitCode, err := sshExecStdin(client, "bash -s", setupScript)
	if err != nil {
		return fmt.Sprintf("setup.sh exec error: %v", err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("setup.sh failed (exit %d): %s", exitCode, stderr), true
	}

	// 3. Write default filter config
	defaultFilterConfig := `{"allowed_ports":[],"allow_reboot":false,"allow_shutdown":false,"custom_blocked":[]}`
	if err := sshWriteFile(client, "/etc/devops-agent/filters.json", defaultFilterConfig); err != nil {
		return fmt.Sprintf("write filter config: %v", err), true
	}

	// 4. Download agent binary from GitHub release
	dlCmd := fmt.Sprintf("curl -fsSL -o /usr/local/bin/devops '%s' && chmod +x /usr/local/bin/devops", releaseURL())
	_, stderr, exitCode, err = sshExec(client, dlCmd)
	if err != nil {
		return fmt.Sprintf("download agent: %v", err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("download agent failed (exit %d): %s", exitCode, stderr), true
	}

	// 5. Enable and start the agent service
	_, stderr, exitCode, err = sshExec(client, "systemctl daemon-reload && systemctl enable --now devops-agent")
	if err != nil {
		return fmt.Sprintf("enable service: %v", err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("enable service failed (exit %d): %s", exitCode, stderr), true
	}

	return fmt.Sprintf("installed %s, server configured", Version), false
}

func (s *server) bootstrapUpdate(client *ssh.Client, oldVersion string) (string, bool) {
	// 1. Download new binary to temp location
	dlCmd := fmt.Sprintf("curl -fsSL -o /usr/local/bin/devops.new '%s' && chmod +x /usr/local/bin/devops.new && mv /usr/local/bin/devops.new /usr/local/bin/devops", releaseURL())
	_, stderr, exitCode, err := sshExec(client, dlCmd)
	if err != nil {
		return fmt.Sprintf("download agent: %v", err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("download agent failed (exit %d): %s", exitCode, stderr), true
	}

	// 2. Write systemd unit (always, in case it changed)
	content, err := readEmbedFile("embed/devops-agent.service")
	if err != nil {
		return fmt.Sprintf("read service unit: %v", err), true
	}
	if err := sshWriteFile(client, "/etc/systemd/system/devops-agent.service", content); err != nil {
		return fmt.Sprintf("write service unit: %v", err), true
	}

	// 3. Reload and restart
	_, stderr, exitCode, err = sshExec(client, "systemctl daemon-reload && systemctl restart devops-agent")
	if err != nil {
		return fmt.Sprintf("restart agent: %v", err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("restart agent failed (exit %d): %s", exitCode, stderr), true
	}

	// 4. Wait and verify
	time.Sleep(2 * time.Second)
	newVersion, pingErr := agentPing(client)
	if pingErr != nil {
		return fmt.Sprintf("agent installed but not responding: %v", pingErr), true
	}

	return fmt.Sprintf("updated %s -> %s", oldVersion, newVersion), false
}

func (s *server) devopsImport(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	host, _ := args["host"].(string)
	deployDir, _ := args["deploy_dir"].(string)

	if name == "" || host == "" || deployDir == "" {
		return "name, host, and deploy_dir are required", true
	}

	// Create temporary App for agent communication
	app := &App{
		Host:    host,
		Port:    intArg(args, "port", 22),
		User:    strArg(args, "user", "root"),
		KeyPath: strArg(args, "key_path", ""),
	}

	// Call agent /discover
	resp, err := s.agent.call(s.ctx, app, "/discover", map[string]any{
		"dir": deployDir,
	})
	if err != nil {
		return fmt.Sprintf("agent error: %v", err), true
	}
	if !resp.OK {
		return fmt.Sprintf("discover failed: %s", resp.Error), true
	}

	// Parse discovery data
	var discovered struct {
		Runtime     string `json:"runtime"`
		ComposeFile string `json:"compose_file"`
		Services    []string `json:"services"`
		RepoURL     string `json:"repo_url"`
		Branch      string `json:"branch"`
	}
	if resp.Data != nil {
		if err := json.Unmarshal(resp.Data, &discovered); err != nil {
			return fmt.Sprintf("parse discovery data: %v", err), true
		}
	}

	// Build app from discovered + overrides
	newApp := &App{
		Name:        name,
		Host:        host,
		Port:        app.Port,
		User:        app.User,
		Runtime:     discovered.Runtime,
		ComposeFile: discovered.ComposeFile,
		RepoURL:     discovered.RepoURL,
		Branch:      discovered.Branch,
		DeployDir:   deployDir,
		KeyPath:     app.KeyPath,
		DeployCommands: "[]",
	}

	// Use first discovered service name
	if len(discovered.Services) > 0 {
		newApp.ServiceName = discovered.Services[0]
	}

	// Default branch
	if newApp.Branch == "" {
		newApp.Branch = "main"
	}

	// Apply caller overrides
	if v, ok := args["service_name"].(string); ok && v != "" {
		newApp.ServiceName = v
	}
	if v, ok := args["runtime"].(string); ok && v != "" {
		newApp.Runtime = v
	}
	if v, ok := args["compose_file"].(string); ok && v != "" {
		newApp.ComposeFile = v
	}
	if v, ok := args["repo_url"].(string); ok && v != "" {
		newApp.RepoURL = v
	}
	if v, ok := args["branch"].(string); ok && v != "" {
		newApp.Branch = v
	}
	if v, ok := args["health_check_url"].(string); ok && v != "" {
		newApp.HealthCheckURL = v
	}
	if v, ok := args["deploy_commands"].(string); ok && v != "" {
		newApp.DeployCommands = v
	}
	if v, ok := args["notes"].(string); ok {
		newApp.Notes = v
	}

	// Validate service_name is set
	if newApp.ServiceName == "" {
		return "no service discovered and service_name not provided", true
	}

	// For unknown runtime, require caller to specify
	if newApp.Runtime == "unknown" || newApp.Runtime == "" {
		if v, ok := args["runtime"].(string); ok && v != "" {
			newApp.Runtime = v
		} else {
			return "could not detect runtime, provide runtime override", true
		}
	}

	if err := s.store.addApp(newApp); err != nil {
		return err.Error(), true
	}

	// Return created record + discovery data
	type importResult struct {
		App       string          `json:"app"`
		Host      string          `json:"host"`
		Runtime   string          `json:"rt"`
		Service   string          `json:"svc"`
		Compose   string          `json:"compose,omitempty"`
		Repo      string          `json:"repo,omitempty"`
		Branch    string          `json:"branch,omitempty"`
		DeployDir string          `json:"dir"`
		Discovered json.RawMessage `json:"discovered"`
	}

	result := importResult{
		App:       name,
		Host:      host,
		Runtime:   newApp.Runtime,
		Service:   newApp.ServiceName,
		Compose:   newApp.ComposeFile,
		Repo:      newApp.RepoURL,
		Branch:    newApp.Branch,
		DeployDir: deployDir,
		Discovered: resp.Data,
	}

	out, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("imported %s but failed to marshal result: %v", name, err), false
	}
	return string(out), false
}

// Argument helpers

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		return toInt(v, def)
	}
	return def
}

func toInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return def
		}
		return int(i)
	}
	return def
}

func boolArg(args map[string]any, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

// Filter management handlers

func (s *server) devopsFilterAdd(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)
	pattern, _ := args["pattern"].(string)
	if host == "" || pattern == "" {
		return "host and pattern are required", true
	}

	category := strArg(args, "category", "custom")
	reason := strArg(args, "reason", "")

	if err := s.store.addFilter(host, pattern, category, reason); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("added filter %q for host %s", pattern, host), false
}

func (s *server) devopsFilterList(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)

	filters, err := s.store.listFilters(host)
	if err != nil {
		return fmt.Sprintf("failed to list filters: %v", err), true
	}

	if len(filters) == 0 {
		if host != "" {
			return fmt.Sprintf("no filters for host %s", host), false
		}
		return "no filters configured", false
	}

	type filterSummary struct {
		Host     string `json:"host"`
		Pattern  string `json:"pattern"`
		Category string `json:"category"`
		Reason   string `json:"reason,omitempty"`
	}

	summaries := make([]filterSummary, len(filters))
	for i, f := range filters {
		summaries[i] = filterSummary{
			Host:     f.Host,
			Pattern:  f.Pattern,
			Category: f.Category,
			Reason:   f.Reason,
		}
	}

	out, err := json.Marshal(summaries)
	if err != nil {
		return fmt.Sprintf("failed to marshal filters: %v", err), true
	}
	return string(out), false
}

func (s *server) devopsFilterRemove(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)
	pattern, _ := args["pattern"].(string)
	if host == "" || pattern == "" {
		return "host and pattern are required", true
	}

	if err := s.store.removeFilter(host, pattern); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("removed filter %q from host %s", pattern, host), false
}

func (s *server) devopsFilterSync(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)
	if host == "" {
		return "host is required", true
	}

	// Get SSH connection: try explicit name first, then find first app on host
	var app *App
	if name, ok := args["name"].(string); ok && name != "" {
		a, err := s.store.getApp(name)
		if err != nil {
			return err.Error(), true
		}
		app = a
	} else {
		apps, err := s.store.listApps(host)
		if err != nil {
			return fmt.Sprintf("failed to list apps: %v", err), true
		}
		if len(apps) == 0 {
			return fmt.Sprintf("no apps registered on host %s, provide name for SSH config", host), true
		}
		app = &apps[0]
	}

	// Build filter config
	filters, err := s.store.filtersForHost(host)
	if err != nil {
		return fmt.Sprintf("failed to get filters: %v", err), true
	}

	// Parse allowed_ports from args
	var allowedPorts []int
	if portsStr, ok := args["allowed_ports"].(string); ok && portsStr != "" {
		if err := json.Unmarshal([]byte(portsStr), &allowedPorts); err != nil {
			return fmt.Sprintf("invalid allowed_ports JSON: %v", err), true
		}
	}

	config := struct {
		AllowedPorts  []int `json:"allowed_ports"`
		AllowReboot   bool  `json:"allow_reboot"`
		AllowShutdown bool  `json:"allow_shutdown"`
		CustomBlocked []struct {
			Pattern  string `json:"pattern"`
			Category string `json:"category"`
			Reason   string `json:"reason"`
		} `json:"custom_blocked"`
	}{
		AllowedPorts:  allowedPorts,
		AllowReboot:   boolArg(args, "allow_reboot", false),
		AllowShutdown: boolArg(args, "allow_shutdown", false),
	}
	if config.AllowedPorts == nil {
		config.AllowedPorts = []int{}
	}

	for _, f := range filters {
		config.CustomBlocked = append(config.CustomBlocked, struct {
			Pattern  string `json:"pattern"`
			Category string `json:"category"`
			Reason   string `json:"reason"`
		}{
			Pattern:  f.Pattern,
			Category: f.Category,
			Reason:   f.Reason,
		})
	}
	if config.CustomBlocked == nil {
		config.CustomBlocked = make([]struct {
			Pattern  string `json:"pattern"`
			Category string `json:"category"`
			Reason   string `json:"reason"`
		}, 0)
	}

	configJSON, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal config: %v", err), true
	}

	client, err := s.pool.get(s.ctx, app)
	if err != nil {
		return fmt.Sprintf("ssh connect: %v", err), true
	}

	if err := sshWriteFile(client, "/etc/devops-agent/filters.json", string(configJSON)); err != nil {
		return fmt.Sprintf("write filter config: %v", err), true
	}

	return fmt.Sprintf("synced %d custom filters to %s", len(filters), host), false
}
