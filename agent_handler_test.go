//go:build agent

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerHandlers(mux)
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

	t.Run("shell string rejected", func(t *testing.T) {
		rec := doPost(mux, "/exec", map[string]any{
			"cmd": "echo hello | cat",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
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

func TestDiscover(t *testing.T) {
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
