package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testMux() *http.ServeMux {
	return testMuxWithStore(nil)
}

func testMuxWithStore(st *store) *http.ServeMux {
	mux := http.NewServeMux()
	registerHandlers(mux, st)
	return mux
}

func doPost(mux *http.ServeMux, path string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func doGet(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body.String())
	}
	return resp
}

func TestPing(t *testing.T) {
	mux := testMux()
	rec := doGet(mux, "/ping")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	resp := decodeResponse(t, rec)
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["version"] != Version {
		t.Errorf("version = %v, want %s", resp["version"], Version)
	}
	if resp["hostname"] == nil || resp["hostname"] == "" {
		t.Error("hostname should not be empty")
	}
}

func TestExec(t *testing.T) {
	mux := testMux()

	t.Run("echo", func(t *testing.T) {
		rec := doPost(mux, "/exec", map[string]any{
			"cmd":  "echo",
			"args": []string{"hello", "world"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeResponse(t, rec)
		if resp["ok"] != true {
			t.Errorf("ok = %v, want true", resp["ok"])
		}
		stdout := resp["stdout"].(string)
		if strings.TrimSpace(stdout) != "hello world" {
			t.Errorf("stdout = %q, want 'hello world'", stdout)
		}
	})

	t.Run("missing cmd", func(t *testing.T) {
		rec := doPost(mux, "/exec", map[string]any{})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("shell string allowed", func(t *testing.T) {
		rec := doPost(mux, "/exec", map[string]any{
			"cmd": "echo hello | cat",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var resp AgentResponse
		json.NewDecoder(rec.Body).Decode(&resp)
		if !resp.OK {
			t.Errorf("ok = false, want true")
		}
		if resp.Stdout != "hello\n" {
			t.Errorf("stdout = %q, want \"hello\\n\"", resp.Stdout)
		}
	})

	t.Run("nonexistent command", func(t *testing.T) {
		rec := doPost(mux, "/exec", map[string]any{
			"cmd": "nonexistent-binary-xyz",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("exit code", func(t *testing.T) {
		rec := doPost(mux, "/exec", map[string]any{
			"cmd":  "false",
			"args": []string{},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeResponse(t, rec)
		if resp["ok"] != false {
			t.Errorf("ok = %v, want false", resp["ok"])
		}
		exit := resp["exit"].(float64)
		if exit == 0 {
			t.Error("exit should be non-zero for 'false'")
		}
	})
}

func TestHealth(t *testing.T) {
	mux := testMux()

	// Disable SSRF check so httptest.NewServer on 127.0.0.1 works
	ssrfCheckEnabled = false
	t.Cleanup(func() { ssrfCheckEnabled = true })

	// Create a test HTTP server to check against
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer healthServer.Close()

	t.Run("healthy", func(t *testing.T) {
		rec := doPost(mux, "/health", map[string]any{
			"url": healthServer.URL,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeResponse(t, rec)
		if resp["ok"] != true {
			t.Errorf("ok = %v, want true", resp["ok"])
		}
		if resp["status"].(float64) != 200 {
			t.Errorf("status = %v, want 200", resp["status"])
		}
		if resp["body"] != "OK" {
			t.Errorf("body = %q, want 'OK'", resp["body"])
		}
	})

	t.Run("missing url", func(t *testing.T) {
		rec := doPost(mux, "/health", map[string]any{})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		// Use a port that's almost certainly not listening
		rec := doPost(mux, "/health", map[string]any{
			"url": "http://127.0.0.1:1",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeResponse(t, rec)
		if resp["ok"] != false {
			t.Errorf("ok = %v, want false", resp["ok"])
		}
	})
}

func TestHealthSSRF(t *testing.T) {
	mux := testMux()

	// Ensure SSRF check is enabled
	ssrfCheckEnabled = true
	t.Cleanup(func() { ssrfCheckEnabled = true })

	t.Run("blocks private IP", func(t *testing.T) {
		rec := doPost(mux, "/health", map[string]any{
			"url": "http://127.0.0.1:8080/health",
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("blocks metadata endpoint", func(t *testing.T) {
		rec := doPost(mux, "/health", map[string]any{
			"url": "http://169.254.169.254/latest/meta-data/",
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("blocks file scheme", func(t *testing.T) {
		rec := doPost(mux, "/health", map[string]any{
			"url": "file:///etc/shadow",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("blocks internal network", func(t *testing.T) {
		rec := doPost(mux, "/health", map[string]any{
			"url": "http://10.0.0.1:6379/",
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("agent-side tests use Unix paths from t.TempDir()")
	}
}

func TestDiscover(t *testing.T) {
	skipWindows(t)
	mux := testMux()

	t.Run("empty dir", func(t *testing.T) {
		dir := t.TempDir()
		rec := doPost(mux, "/discover", map[string]any{
			"dir": dir,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeResponse(t, rec)
		if resp["ok"] != true {
			t.Errorf("ok = %v, want true", resp["ok"])
		}
		data := resp["data"].(map[string]any)
		if data["runtime"] != "unknown" {
			t.Errorf("runtime = %v, want unknown", data["runtime"])
		}
	})

	t.Run("invalid dir", func(t *testing.T) {
		rec := doPost(mux, "/discover", map[string]any{
			"dir": "/nonexistent/path",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		rec := doPost(mux, "/discover", map[string]any{
			"dir": "relative/path",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("dir with service file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "myapp.service"), []byte("[Unit]\n"), 0644)
		rec := doPost(mux, "/discover", map[string]any{
			"dir": dir,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		resp := decodeResponse(t, rec)
		data := resp["data"].(map[string]any)
		if data["runtime"] != "systemd" {
			t.Errorf("runtime = %v, want systemd", data["runtime"])
		}
	})

	t.Run("dir with git repo", func(t *testing.T) {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, ".git"), 0755)
		rec := doPost(mux, "/discover", map[string]any{
			"dir": dir,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		// Should succeed even if git commands fail (no real repo)
		resp := decodeResponse(t, rec)
		if resp["ok"] != true {
			t.Errorf("ok = %v, want true", resp["ok"])
		}
	})
}

func TestDeploy(t *testing.T) {
	skipWindows(t)
	mux := testMux()

	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		rec := doPost(mux, "/deploy", map[string]any{
			"dir":      dir,
			"commands": []string{"echo step1", "echo step2"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		// Parse NDJSON stream
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		// Should have 4 lines: running+done for each of 2 commands
		if len(lines) != 4 {
			t.Fatalf("got %d lines, want 4:\n%s", len(lines), rec.Body.String())
		}

		// Check first running line
		var chunk map[string]any
		json.Unmarshal([]byte(lines[0]), &chunk)
		if chunk["status"] != "running" {
			t.Errorf("first chunk status = %v, want running", chunk["status"])
		}
		if chunk["step"].(float64) != 1 {
			t.Errorf("first chunk step = %v, want 1", chunk["step"])
		}

		// Check first done line
		json.Unmarshal([]byte(lines[1]), &chunk)
		if chunk["status"] != "done" {
			t.Errorf("second chunk status = %v, want done", chunk["status"])
		}
		if strings.TrimSpace(chunk["stdout"].(string)) != "step1" {
			t.Errorf("stdout = %q, want 'step1'", chunk["stdout"])
		}
	})

	t.Run("failure stops", func(t *testing.T) {
		dir := t.TempDir()
		rec := doPost(mux, "/deploy", map[string]any{
			"dir":      dir,
			"commands": []string{"false", "echo should-not-run"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		// Should have 2 lines: running+failed for the first command, no second command
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2:\n%s", len(lines), rec.Body.String())
		}

		var chunk map[string]any
		json.Unmarshal([]byte(lines[1]), &chunk)
		if chunk["status"] != "failed" {
			t.Errorf("status = %v, want failed", chunk["status"])
		}
	})

	t.Run("empty commands", func(t *testing.T) {
		dir := t.TempDir()
		rec := doPost(mux, "/deploy", map[string]any{
			"dir":      dir,
			"commands": []string{},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid dir", func(t *testing.T) {
		rec := doPost(mux, "/deploy", map[string]any{
			"dir":      "/nonexistent/path",
			"commands": []string{"echo hi"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestServiceValidation(t *testing.T) {
	mux := testMux()

	t.Run("missing fields", func(t *testing.T) {
		rec := doPost(mux, "/service", map[string]any{
			"action": "status",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid service name", func(t *testing.T) {
		rec := doPost(mux, "/service", map[string]any{
			"action":  "status",
			"name":    "bad service!",
			"runtime": "systemd",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid runtime", func(t *testing.T) {
		rec := doPost(mux, "/service", map[string]any{
			"action":  "status",
			"name":    "nginx",
			"runtime": "podman",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("docker without compose_file", func(t *testing.T) {
		rec := doPost(mux, "/service", map[string]any{
			"action":  "status",
			"name":    "nginx",
			"runtime": "docker",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		rec := doPost(mux, "/service", map[string]any{
			"action":  "destroy",
			"name":    "nginx",
			"runtime": "systemd",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestLogsValidation(t *testing.T) {
	mux := testMux()

	t.Run("missing name", func(t *testing.T) {
		rec := doPost(mux, "/logs", map[string]any{
			"runtime": "systemd",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid service name", func(t *testing.T) {
		rec := doPost(mux, "/logs", map[string]any{
			"name":    "bad name!",
			"runtime": "systemd",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestContainerJSONParsing(t *testing.T) {
	t.Run("json array", func(t *testing.T) {
		data := `[{"Name":"web","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:8080->8080/tcp"}]`
		containers := parseContainerJSON([]byte(data))
		if len(containers) != 1 {
			t.Fatalf("len = %d, want 1", len(containers))
		}
		if containers[0].Name != "web" {
			t.Errorf("name = %q, want 'web'", containers[0].Name)
		}
		if containers[0].State != "running" {
			t.Errorf("state = %q, want 'running'", containers[0].State)
		}
	})

	t.Run("ndjson", func(t *testing.T) {
		data := "{\"Name\":\"web\",\"State\":\"running\"}\n{\"Name\":\"db\",\"State\":\"running\"}\n"
		containers := parseContainerJSON([]byte(data))
		if len(containers) != 2 {
			t.Fatalf("len = %d, want 2", len(containers))
		}
	})

	t.Run("empty", func(t *testing.T) {
		containers := parseContainerJSON([]byte(""))
		if len(containers) != 0 {
			t.Fatalf("len = %d, want 0", len(containers))
		}
	})

	t.Run("lowercase keys", func(t *testing.T) {
		data := `[{"name":"web","state":"running","status":"Up","ports":"8080"}]`
		containers := parseContainerJSON([]byte(data))
		if len(containers) != 1 {
			t.Fatalf("len = %d, want 1", len(containers))
		}
		if containers[0].Name != "web" {
			t.Errorf("name = %q, want 'web'", containers[0].Name)
		}
	})
}

func TestExecBlocked(t *testing.T) {
	mux := testMux()

	tests := []struct {
		name       string
		cmd        string
		wantStatus int
	}{
		{"rm -rf /", "rm -rf /", http.StatusForbidden},
		{"ufw disable", "ufw disable", http.StatusForbidden},
		{"systemctl stop ssh", "systemctl stop ssh", http.StatusForbidden},
		{"echo hello", "echo hello", http.StatusOK},
		{"ls -la", "ls", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPost(mux, "/exec", map[string]any{"cmd": tt.cmd})
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestDeployBlocked(t *testing.T) {
	skipWindows(t)
	mux := testMux()
	dir := t.TempDir()

	rec := doPost(mux, "/deploy", map[string]any{
		"dir":      dir,
		"commands": []string{"echo step1", "rm -rf /", "echo should-not-run"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	// step1 running + done = 2 lines, then rm -rf / blocked = 1 line = 3 total
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), rec.Body.String())
	}

	// Last line should be "blocked"
	var chunk map[string]any
	json.Unmarshal([]byte(lines[2]), &chunk)
	if chunk["status"] != "blocked" {
		t.Errorf("last chunk status = %v, want blocked", chunk["status"])
	}
	if chunk["step"].(float64) != 2 {
		t.Errorf("blocked step = %v, want 2", chunk["step"])
	}
}

func TestFiltersEndpoint(t *testing.T) {
	mux := testMux()
	rec := doGet(mux, "/filters")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	resp := decodeResponse(t, rec)
	hardBlocked, ok := resp["hard_blocked"].([]any)
	if !ok {
		t.Fatal("hard_blocked should be an array")
	}
	if len(hardBlocked) == 0 {
		t.Error("hard_blocked should not be empty")
	}
}

// TestUnixSocketServer verifies the agent works over a real unix socket.
func TestUnixSocketServer(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "test.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	mux := testMux()
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}

	resp, err := client.Get("http://unix/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, string(body))
	}
	if result["ok"] != true {
		t.Errorf("ok = %v, want true", result["ok"])
	}
	if result["version"] != Version {
		t.Errorf("version = %v, want %s", result["version"], Version)
	}
}

func TestSyncApps(t *testing.T) {
	st := testStore(t)
	mux := testMuxWithStore(st)

	apps := []map[string]any{
		{
			"name":            "app1",
			"host":            "10.0.0.1",
			"port":            22,
			"user":            "root",
			"runtime":         "docker",
			"service_name":    "web",
			"compose_file":    "/opt/app1/compose.yml",
			"branch":          "main",
			"deploy_dir":      "/opt/app1",
			"deploy_commands": `["git pull","docker compose up -d"]`,
		},
		{
			"name":         "app2",
			"host":         "10.0.0.1",
			"port":         22,
			"user":         "root",
			"runtime":      "systemd",
			"service_name": "app2",
			"branch":       "main",
		},
	}

	rec := doPost(mux, "/sync-apps", map[string]any{"apps": apps})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	resp := decodeResponse(t, rec)
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["synced"].(float64) != 2 {
		t.Errorf("synced = %v, want 2", resp["synced"])
	}

	// Verify apps are in DB
	got1, err := st.getApp("app1")
	if err != nil {
		t.Fatalf("getApp app1: %v", err)
	}
	if got1.Host != "10.0.0.1" {
		t.Errorf("app1 host = %q, want 10.0.0.1", got1.Host)
	}
	if got1.DeployCommands != `["git pull","docker compose up -d"]` {
		t.Errorf("app1 deploy_commands = %q", got1.DeployCommands)
	}
}

func TestSyncAppsRemovesStale(t *testing.T) {
	st := testStore(t)
	mux := testMuxWithStore(st)

	// Sync 3 apps
	apps := []map[string]any{
		{"name": "a", "host": "h", "port": 22, "user": "root", "runtime": "systemd", "service_name": "a", "branch": "main"},
		{"name": "b", "host": "h", "port": 22, "user": "root", "runtime": "systemd", "service_name": "b", "branch": "main"},
		{"name": "c", "host": "h", "port": 22, "user": "root", "runtime": "systemd", "service_name": "c", "branch": "main"},
	}
	rec := doPost(mux, "/sync-apps", map[string]any{"apps": apps})
	if rec.Code != http.StatusOK {
		t.Fatalf("first sync: status = %d; body: %s", rec.Code, rec.Body.String())
	}

	// Sync only 2 apps (remove c)
	apps = apps[:2]
	rec = doPost(mux, "/sync-apps", map[string]any{"apps": apps})
	if rec.Code != http.StatusOK {
		t.Fatalf("second sync: status = %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp["removed"].(float64) != 1 {
		t.Errorf("removed = %v, want 1", resp["removed"])
	}

	// Verify c is gone
	_, err := st.getApp("c")
	if err == nil {
		t.Error("app c should have been removed")
	}
}

func TestRedeployNotFound(t *testing.T) {
	st := testStore(t)
	mux := testMuxWithStore(st)

	rec := doPost(mux, "/redeploy", map[string]any{"name": "nonexistent"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRedeployNoCommands(t *testing.T) {
	st := testStore(t)
	mux := testMuxWithStore(st)

	app := &App{
		Name:           "nocommands",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "systemd",
		ServiceName:    "nocommands",
		Branch:         "main",
		DeployDir:      "/opt/nocommands",
		DeployCommands: "[]",
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	rec := doPost(mux, "/redeploy", map[string]any{"name": "nocommands"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRedeploySuccess(t *testing.T) {
	skipWindows(t)
	st := testStore(t)
	mux := testMuxWithStore(st)

	dir := t.TempDir()
	app := &App{
		Name:           "redeployable",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "docker",
		ServiceName:    "web",
		ComposeFile:    "/opt/app/compose.yml",
		Branch:         "main",
		DeployDir:      dir,
		DeployCommands: `["echo step1","echo step2"]`,
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	rec := doPost(mux, "/redeploy", map[string]any{"name": "redeployable"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Parse NDJSON: should have running+done for each command (4 lines)
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), rec.Body.String())
	}

	// Verify deploy result was recorded in DB
	got, err := st.getApp("redeployable")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if !got.LastDeployOK.Valid || got.LastDeployOK.Int64 != 1 {
		t.Errorf("last_deploy_ok = %v, want 1", got.LastDeployOK)
	}
	if !got.LastDeployAt.Valid {
		t.Error("last_deploy_at should be set")
	}
}

func TestRedeployRetryProtection(t *testing.T) {
	skipWindows(t)
	st := testStore(t)
	mux := testMuxWithStore(st)

	dir := t.TempDir()
	app := &App{
		Name:           "failing-app",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "docker",
		ServiceName:    "web",
		ComposeFile:    "/opt/app/compose.yml",
		Branch:         "main",
		DeployDir:      dir,
		DeployCommands: `["echo hello"]`,
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	// Simulate a recent failed deploy
	if err := st.updateDeployResult("failing-app", sql.NullInt64{Int64: 0, Valid: true}, "error"); err != nil {
		t.Fatalf("updateDeployResult: %v", err)
	}

	// Should be rejected (within backoff window)
	rec := doPost(mux, "/redeploy", map[string]any{"name": "failing-app"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body: %s", rec.Code, rec.Body.String())
	}

	// Should succeed with force=true
	rec = doPost(mux, "/redeploy", map[string]any{"name": "failing-app", "force": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("force status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRedeployRetryExpired(t *testing.T) {
	skipWindows(t)
	st := testStore(t)
	mux := testMuxWithStore(st)

	dir := t.TempDir()
	app := &App{
		Name:           "old-fail-app",
		Host:           "10.0.0.1",
		Port:           22,
		User:           "root",
		Runtime:        "docker",
		ServiceName:    "web",
		ComposeFile:    "/opt/app/compose.yml",
		Branch:         "main",
		DeployDir:      dir,
		DeployCommands: `["echo hello"]`,
	}
	if err := st.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	// Simulate a failed deploy from 10 minutes ago
	oldTime := time.Now().Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	st.db.Exec("UPDATE apps SET last_deploy_ok = 0, last_deploy_at = ? WHERE name = ?", oldTime, "old-fail-app")

	// Should succeed (backoff expired)
	rec := doPost(mux, "/redeploy", map[string]any{"name": "old-fail-app"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRedeployMissingName(t *testing.T) {
	st := testStore(t)
	mux := testMuxWithStore(st)

	rec := doPost(mux, "/redeploy", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSyncAppsEmpty(t *testing.T) {
	st := testStore(t)
	mux := testMuxWithStore(st)

	rec := doPost(mux, "/sync-apps", map[string]any{"apps": []any{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}
