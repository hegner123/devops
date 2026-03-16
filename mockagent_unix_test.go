//go:build !agent && !windows

package main

import (
	"net"
	"net/http"
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

func dialMockAgent(addr string) (net.Conn, error) {
	return net.Dial("unix", addr)
}
