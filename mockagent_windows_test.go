//go:build !agent && windows

package main

import (
	"net"
	"net/http"
	"testing"
)

// mockAgent starts a TCP loopback HTTP server that mimics the remote agent.
// Windows AF_UNIX socket paths from t.TempDir() can exceed the 108-char limit,
// so we use TCP loopback for test reliability.
func mockAgent(t *testing.T, handler http.Handler) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: handler}
	go srv.Serve(listener)
	t.Cleanup(func() {
		srv.Close()
		listener.Close()
	})

	return listener.Addr().String()
}

func dialMockAgent(addr string) (net.Conn, error) {
	return net.Dial("tcp", addr)
}
