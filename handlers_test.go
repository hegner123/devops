//go:build !agent

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// mockAgent starts a unix socket HTTP server that mimics the remote agent.
func mockAgent(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "agent.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: handler}
	go srv.Serve(listener)
	t.Cleanup(func() {
		srv.Close()
		listener.Close()
	})

	return sock
}

// directAgentClient connects to a local unix socket, bypassing SSH for testing.
type directAgentClient struct {
	sockPath string
}

func (d *directAgentClient) call(ctx context.Context, app *App, endpoint string, req any) (*AgentResponse, error) {
	conn, err := net.Dial("unix", d.sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial mock agent: %w", err)
	}
	defer conn.Close()

	pr, pw := io.Pipe()
	go func() {
		err := json.NewEncoder(pw).Encode(req)
		pw.CloseWithError(err)
	}()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://agent"+endpoint, pr)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return conn, nil
			},
		},
	}

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agent request: %w", err)
	}
	defer httpResp.Body.Close()

	var resp AgentResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

func (d *directAgentClient) callStream(ctx context.Context, app *App, endpoint string, req any) (io.ReadCloser, error) {
	conn, err := net.Dial("unix", d.sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial mock agent: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		err := json.NewEncoder(pw).Encode(req)
		pw.CloseWithError(err)
	}()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://agent"+endpoint, pr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return conn, nil
			},
		},
	}

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("agent request: %w", err)
	}

	return &testStreamReader{body: httpResp.Body, conn: conn}, nil
}

type testStreamReader struct {
	body io.ReadCloser
	conn net.Conn
}

func (t *testStreamReader) Read(p []byte) (int, error) {
	return t.body.Read(p)
}

func (t *testStreamReader) Close() error {
	err := t.body.Close()
	t.conn.Close()
	return err
}

// testServer creates a server with in-memory DB for testing handler validation.
func testServer(t *testing.T) *server {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	return &server{
		store: st,
		ctx:   context.Background(),
	}
}

func addTestApp(t *testing.T, s *server, name, host string) *App {
	t.Helper()
	app := &App{
		Name:           name,
		Host:           host,
		Port:           22,
		User:           "root",
		Runtime:        "systemd",
		ServiceName:    name,
		DeployCommands: "[]",
	}
	if err := s.store.addApp(app); err != nil {
		t.Fatalf("add app: %v", err)
	}
	return app
}

// Handler validation tests

func TestHandlerStatusMissing(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsStatus(map[string]any{})
	if !isError {
		t.Error("expected error for missing name")
	}
	if result != "name is required" {
		t.Errorf("result = %q, want 'name is required'", result)
	}
}

func TestHandlerStatusAppNotFound(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsStatus(map[string]any{"name": "nonexistent"})
	if !isError {
		t.Error("expected error for nonexistent app")
	}
	if result == "" {
		t.Error("expected non-empty error message")
	}
}

func TestHandlerRestartMissing(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsRestart(map[string]any{})
	if !isError {
		t.Error("expected error for missing name")
	}
	if result != "name is required" {
		t.Errorf("result = %q", result)
	}
}

func TestHandlerStopMissing(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsStop(map[string]any{})
	if !isError {
		t.Error("expected error for missing name")
	}
	if result != "name is required" {
		t.Errorf("result = %q", result)
	}
}

func TestHandlerLogsMissing(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsLogs(map[string]any{})
	if !isError {
		t.Error("expected error for missing name")
	}
	if result != "name is required" {
		t.Errorf("result = %q", result)
	}
}

func TestHandlerExecMissing(t *testing.T) {
	s := testServer(t)

	t.Run("no args", func(t *testing.T) {
		result, isError := s.devopsExec(map[string]any{})
		if !isError {
			t.Error("expected error")
		}
		if result != "name and command are required" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("no command", func(t *testing.T) {
		result, isError := s.devopsExec(map[string]any{"name": "test"})
		if !isError {
			t.Error("expected error")
		}
		if result != "name and command are required" {
			t.Errorf("result = %q", result)
		}
	})
}

func TestHandlerHealthMissing(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsHealth(map[string]any{})
	if !isError {
		t.Error("expected error for missing name")
	}
	if result != "name is required" {
		t.Errorf("result = %q", result)
	}
}

func TestHandlerHealthNoURL(t *testing.T) {
	s := testServer(t)
	addTestApp(t, s, "myapp", "10.0.0.1")

	result, isError := s.devopsHealth(map[string]any{"name": "myapp"})
	if !isError {
		t.Error("expected error for missing health_check_url")
	}
	if result != "no health_check_url configured for this app" {
		t.Errorf("result = %q", result)
	}
}

func TestHandlerExecAppNotFound(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsExec(map[string]any{"name": "nonexistent", "command": "ls"})
	if !isError {
		t.Error("expected error for nonexistent app")
	}
	if result == "" {
		t.Error("expected non-empty error")
	}
}

// Mock agent integration tests -- verify request/response format compatibility.

func TestMockAgentIntegration(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /service", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action  string `json:"action"`
			Name    string `json:"name"`
			Runtime string `json:"runtime"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		switch req.Action {
		case "status":
			json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"data": map[string]string{"ActiveState": "active", "MainPID": "1234"},
			})
		case "restart":
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "stop":
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown action"})
		}
	})

	mux.HandleFunc("POST /logs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"output": "line1\nline2\nline3",
		})
	})

	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cmd  string   `json:"cmd"`
			Args []string `json:"args"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		exitCode := 0
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"stdout": fmt.Sprintf("executed: %s %v", req.Cmd, req.Args),
			"stderr": "",
			"exit":   exitCode,
		})
	})

	mux.HandleFunc("POST /health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"status": 200,
			"body":   "OK",
		})
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	t.Run("status", func(t *testing.T) {
		app := &App{Name: "test", Host: "10.0.0.1", ServiceName: "myapp", Runtime: "systemd"}
		resp, err := client.call(context.Background(), app, "/service", map[string]any{
			"action":  "status",
			"name":    app.ServiceName,
			"runtime": app.Runtime,
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !resp.OK {
			t.Error("ok = false, want true")
		}
		if resp.Data == nil {
			t.Fatal("data should not be nil")
		}
		var data map[string]string
		json.Unmarshal(resp.Data, &data)
		if data["ActiveState"] != "active" {
			t.Errorf("ActiveState = %q, want active", data["ActiveState"])
		}
	})

	t.Run("restart", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/service", map[string]any{
			"action": "restart", "name": "myapp", "runtime": "systemd",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !resp.OK {
			t.Error("ok = false, want true")
		}
	})

	t.Run("stop", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/service", map[string]any{
			"action": "stop", "name": "myapp", "runtime": "systemd",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !resp.OK {
			t.Error("ok = false, want true")
		}
	})

	t.Run("logs", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/logs", map[string]any{
			"name": "myapp", "lines": 50, "runtime": "systemd",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !resp.OK {
			t.Error("ok = false, want true")
		}
		if resp.Output != "line1\nline2\nline3" {
			t.Errorf("output = %q", resp.Output)
		}
	})

	t.Run("exec", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/exec", map[string]any{
			"cmd": "df", "args": []string{"-h"},
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !resp.OK {
			t.Error("ok = false, want true")
		}
		if resp.Stdout == "" {
			t.Error("stdout should not be empty")
		}
	})

	t.Run("health", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/health", map[string]any{
			"url": "http://localhost:8080/health",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !resp.OK {
			t.Error("ok = false, want true")
		}
		if resp.Status != 200 {
			t.Errorf("status = %d, want 200", resp.Status)
		}
	})

	t.Run("exec_log recording", func(t *testing.T) {
		err := st.logExec("myapp", "10.0.0.1", "df", `["-h"]`, 0)
		if err != nil {
			t.Fatalf("logExec: %v", err)
		}
		var count int
		err = st.db.QueryRow("SELECT COUNT(*) FROM exec_log WHERE app_name = ?", "myapp").Scan(&count)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
	})
}

func TestHandlerErrorPaths(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /service", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "service not found",
		})
	})

	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"stderr": "command failed",
			"exit":   1,
		})
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	t.Run("service error", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/service", map[string]any{
			"action": "status", "name": "bad", "runtime": "systemd",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if resp.OK {
			t.Error("expected ok = false")
		}
		if resp.Error != "service not found" {
			t.Errorf("error = %q, want 'service not found'", resp.Error)
		}
	})

	t.Run("exec error", func(t *testing.T) {
		resp, err := client.call(context.Background(), nil, "/exec", map[string]any{
			"cmd": "badcmd",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if resp.OK {
			t.Error("expected ok = false")
		}
		if resp.Stderr != "command failed" {
			t.Errorf("stderr = %q", resp.Stderr)
		}
	})
}

func TestCRUDHandlersStillWork(t *testing.T) {
	s := testServer(t)

	result, isError := s.devopsAdd(map[string]any{
		"name": "webapp", "host": "10.0.0.1", "service_name": "webapp", "runtime": "systemd",
	})
	if isError {
		t.Fatalf("add: %s", result)
	}

	result, isError = s.devopsList(map[string]any{})
	if isError {
		t.Fatalf("list: %s", result)
	}
	var apps []map[string]any
	json.Unmarshal([]byte(result), &apps)
	if len(apps) != 1 {
		t.Fatalf("list returned %d apps, want 1", len(apps))
	}
	if apps[0]["name"] != "webapp" {
		t.Errorf("name = %v", apps[0]["name"])
	}

	result, isError = s.devopsUpdate(map[string]any{"name": "webapp", "notes": "updated"})
	if isError {
		t.Fatalf("update: %s", result)
	}

	result, isError = s.devopsRemove(map[string]any{"name": "webapp"})
	if isError {
		t.Fatalf("remove: %s", result)
	}

	result, isError = s.devopsList(map[string]any{})
	if isError {
		t.Fatalf("list: %s", result)
	}
	if result != "no apps registered" {
		t.Errorf("result = %q, want 'no apps registered'", result)
	}
}

// Phase 4 tests: deploy, import, bootstrap

func TestDeploySuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dir      string   `json:"dir"`
			Commands []string `json:"commands"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)

		for i, cmd := range req.Commands {
			step := i + 1
			enc.Encode(map[string]any{"step": step, "cmd": cmd, "status": "running"})
			flusher.Flush()
			enc.Encode(map[string]any{
				"step": step, "cmd": cmd, "status": "done",
				"exit": 0, "stdout": "ok", "stderr": "", "elapsed": "0.1s",
			})
			flusher.Flush()
		}
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	s := &server{store: st, agent: client, ctx: context.Background()}

	app := &App{
		Name:           "webapp",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "systemd",
		ServiceName:    "webapp",
		DeployDir:      "/opt/webapp",
		DeployCommands: `["git pull","make build"]`,
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("add app: %v", err)
	}

	result, isError := s.devopsDeploy(map[string]any{"name": "webapp"})
	if isError {
		t.Fatalf("deploy failed: %s", result)
	}
	if !contains(result, "deployed webapp") {
		t.Errorf("result = %q, want to contain 'deployed webapp'", result)
	}
	if !contains(result, "2 steps") {
		t.Errorf("result = %q, want to contain '2 steps'", result)
	}

	// Verify DB was updated
	updated, err := st.getApp("webapp")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if !updated.LastDeployOK.Valid || updated.LastDeployOK.Int64 != 1 {
		t.Errorf("last_deploy_ok = %v, want 1", updated.LastDeployOK)
	}
	if !updated.LastDeployAt.Valid {
		t.Error("last_deploy_at should be set")
	}
}

func TestDeployFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)

		enc.Encode(map[string]any{"step": 1, "cmd": "git pull", "status": "running"})
		flusher.Flush()
		enc.Encode(map[string]any{
			"step": 1, "cmd": "git pull", "status": "done",
			"exit": 0, "stdout": "ok", "elapsed": "0.1s",
		})
		flusher.Flush()
		enc.Encode(map[string]any{"step": 2, "cmd": "make build", "status": "running"})
		flusher.Flush()
		enc.Encode(map[string]any{
			"step": 2, "cmd": "make build", "status": "failed",
			"exit": 1, "stderr": "build error", "elapsed": "1.0s",
		})
		flusher.Flush()
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	s := &server{store: st, agent: client, ctx: context.Background()}

	app := &App{
		Name:           "webapp",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "systemd",
		ServiceName:    "webapp",
		DeployDir:      "/opt/webapp",
		DeployCommands: `["git pull","make build"]`,
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("add app: %v", err)
	}

	result, isError := s.devopsDeploy(map[string]any{"name": "webapp"})
	if !isError {
		t.Fatal("expected error for failed deploy")
	}
	if !contains(result, "deploy failed at step 2") {
		t.Errorf("result = %q, want to contain 'deploy failed at step 2'", result)
	}

	// Verify DB records failure
	updated, err := st.getApp("webapp")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if !updated.LastDeployOK.Valid || updated.LastDeployOK.Int64 != 0 {
		t.Errorf("last_deploy_ok = %v, want 0", updated.LastDeployOK)
	}
}

func TestDeployMissingParams(t *testing.T) {
	s := testServer(t)

	t.Run("no name", func(t *testing.T) {
		result, isError := s.devopsDeploy(map[string]any{})
		if !isError {
			t.Error("expected error")
		}
		if result != "name is required" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("no commands", func(t *testing.T) {
		addTestApp(t, s, "nocommands", "10.0.0.1")
		result, isError := s.devopsDeploy(map[string]any{"name": "nocommands"})
		if !isError {
			t.Error("expected error")
		}
		if result != "no deploy commands configured and none provided" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("no deploy_dir", func(t *testing.T) {
		app := &App{
			Name:           "nodeploy",
			Host:           "10.0.0.1",
			Port:           22,
			User:           "root",
			Runtime:        "systemd",
			ServiceName:    "nodeploy",
			DeployCommands: `["echo hi"]`,
		}
		if err := s.store.addApp(app); err != nil {
			t.Fatalf("add app: %v", err)
		}
		result, isError := s.devopsDeploy(map[string]any{"name": "nodeploy"})
		if !isError {
			t.Error("expected error")
		}
		if result != "deploy_dir is not configured for this app" {
			t.Errorf("result = %q", result)
		}
	})
}

func TestDeployOverrideCommands(t *testing.T) {
	mux := http.NewServeMux()
	var receivedCommands []string
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Commands []string `json:"commands"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		receivedCommands = req.Commands

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		enc.Encode(map[string]any{
			"step": 1, "cmd": req.Commands[0], "status": "done",
			"exit": 0, "stdout": "ok", "elapsed": "0.1s",
		})
		flusher.Flush()
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	s := &server{store: st, agent: client, ctx: context.Background()}

	app := &App{
		Name:           "webapp",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "systemd",
		ServiceName:    "webapp",
		DeployDir:      "/opt/webapp",
		DeployCommands: `["git pull"]`,
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("add app: %v", err)
	}

	result, isError := s.devopsDeploy(map[string]any{
		"name":     "webapp",
		"commands": `["custom deploy"]`,
	})
	if isError {
		t.Fatalf("deploy failed: %s", result)
	}
	if len(receivedCommands) != 1 || receivedCommands[0] != "custom deploy" {
		t.Errorf("commands = %v, want [custom deploy]", receivedCommands)
	}
}

func TestImportSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /discover", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"runtime":      "docker",
				"compose_file": "/root/myapp/compose.yml",
				"services":     []string{"web", "db"},
				"repo_url":     "git@github.com:user/myapp.git",
				"branch":       "main",
			},
		})
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	s := &server{store: st, agent: client, ctx: context.Background()}

	result, isError := s.devopsImport(map[string]any{
		"name":       "myapp",
		"host":       "10.0.0.1",
		"deploy_dir": "/root/myapp",
	})
	if isError {
		t.Fatalf("import failed: %s", result)
	}

	// Verify the result contains expected data
	var importResult map[string]any
	if err := json.Unmarshal([]byte(result), &importResult); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if importResult["app"] != "myapp" {
		t.Errorf("app = %v", importResult["app"])
	}
	if importResult["rt"] != "docker" {
		t.Errorf("rt = %v", importResult["rt"])
	}
	if importResult["svc"] != "web" {
		t.Errorf("svc = %v, want web (first discovered service)", importResult["svc"])
	}

	// Verify DB record was created
	app, err := st.getApp("myapp")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if app.Runtime != "docker" {
		t.Errorf("runtime = %q, want docker", app.Runtime)
	}
	if app.ComposeFile != "/root/myapp/compose.yml" {
		t.Errorf("compose_file = %q", app.ComposeFile)
	}
	if app.ServiceName != "web" {
		t.Errorf("service_name = %q, want web", app.ServiceName)
	}
	if app.RepoURL != "git@github.com:user/myapp.git" {
		t.Errorf("repo_url = %q", app.RepoURL)
	}
}

func TestImportWithOverrides(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /discover", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"runtime":      "docker",
				"compose_file": "/root/myapp/compose.yml",
				"services":     []string{"web", "db"},
				"repo_url":     "git@github.com:user/myapp.git",
				"branch":       "main",
			},
		})
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	s := &server{store: st, agent: client, ctx: context.Background()}

	result, isError := s.devopsImport(map[string]any{
		"name":         "myapp",
		"host":         "10.0.0.1",
		"deploy_dir":   "/root/myapp",
		"service_name": "db",
		"branch":       "production",
	})
	if isError {
		t.Fatalf("import failed: %s", result)
	}

	app, err := st.getApp("myapp")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if app.ServiceName != "db" {
		t.Errorf("service_name = %q, want db (override)", app.ServiceName)
	}
	if app.Branch != "production" {
		t.Errorf("branch = %q, want production (override)", app.Branch)
	}
}

func TestImportMissingParams(t *testing.T) {
	s := testServer(t)

	t.Run("no args", func(t *testing.T) {
		result, isError := s.devopsImport(map[string]any{})
		if !isError {
			t.Error("expected error")
		}
		if result != "name, host, and deploy_dir are required" {
			t.Errorf("result = %q", result)
		}
	})

	t.Run("missing deploy_dir", func(t *testing.T) {
		result, isError := s.devopsImport(map[string]any{"name": "x", "host": "10.0.0.1"})
		if !isError {
			t.Error("expected error")
		}
		if result != "name, host, and deploy_dir are required" {
			t.Errorf("result = %q", result)
		}
	})
}

func TestImportUnknownRuntime(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /discover", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"runtime": "unknown",
			},
		})
	})

	sockPath := mockAgent(t, mux)
	client := &directAgentClient{sockPath: sockPath}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := newStore(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.close() })

	s := &server{store: st, agent: client, ctx: context.Background()}

	// Without runtime override, should fail
	result, isError := s.devopsImport(map[string]any{
		"name":         "myapp",
		"host":         "10.0.0.1",
		"deploy_dir":   "/root/myapp",
		"service_name": "myapp",
	})
	if !isError {
		t.Fatal("expected error for unknown runtime without override")
	}
	if !contains(result, "could not detect runtime") {
		t.Errorf("result = %q", result)
	}

	// With runtime override, should succeed
	result, isError = s.devopsImport(map[string]any{
		"name":         "myapp2",
		"host":         "10.0.0.1",
		"deploy_dir":   "/root/myapp",
		"service_name": "myapp2",
		"runtime":      "systemd",
	})
	if isError {
		t.Fatalf("import with override failed: %s", result)
	}
}

func TestBootstrapMissingHost(t *testing.T) {
	s := testServer(t)
	result, isError := s.devopsBootstrap(map[string]any{})
	if !isError {
		t.Error("expected error for missing host")
	}
	if result != "host is required" {
		t.Errorf("result = %q", result)
	}
}

func TestEmbedFilesExist(t *testing.T) {
	expectedFiles := []string{
		"embed/devops-agent.service",
		"embed/ufw.sh",
		"embed/sshd-hardening.conf",
		"embed/sysctl-tuning.conf",
		"embed/docker-daemon.json",
		"embed/unattended-upgrades",
		"embed/setup.sh",
	}
	for _, name := range expectedFiles {
		content, err := readEmbedFile(name)
		if err != nil {
			t.Errorf("missing embedded file %s: %v", name, err)
			continue
		}
		if content == "" {
			t.Errorf("embedded file %s is empty", name)
		}
	}
}

func TestReleaseURL(t *testing.T) {
	url := releaseURL()
	if !contains(url, "github.com") {
		t.Errorf("url = %q, want to contain github.com", url)
	}
	if !contains(url, Version) {
		t.Errorf("url = %q, want to contain version %s", url, Version)
	}
	if !contains(url, "devops-linux-amd64") {
		t.Errorf("url = %q, want to contain devops-linux-amd64", url)
	}
}

// Ensure deploy_key is gitignored.
func TestDeployKeyExists(t *testing.T) {
	if _, err := os.Stat("keys/deploy_key"); err != nil {
		t.Skip("deploy key not present (expected in CI)")
	}
	if len(deployKey) == 0 {
		t.Error("embedded deploy key is empty")
	}
}
