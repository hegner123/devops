//go:build agent

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const socketPath = "/run/devops-agent/agent.sock"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: devops agent|version\n")
		os.Exit(1)
	}
	if os.Args[1] == "version" {
		fmt.Println(Version)
		return
	}
	if os.Args[1] != "agent" {
		fmt.Fprintf(os.Stderr, "usage: devops agent|version\n")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		fmt.Fprintf(os.Stderr, "received %v, shutting down\n", sig)
		cancel()
		go func() {
			<-sigChan
			os.Exit(1)
		}()
	}()

	// Remove stale socket if it exists
	os.Remove(socketPath)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "create socket directory: %v\n", err)
		os.Exit(1)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}

	if err := os.Chmod(socketPath, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "chmod socket: %v\n", err)
		listener.Close()
		os.Exit(1)
	}

	mux := http.NewServeMux()
	registerHandlers(mux)

	// Limit to 10 concurrent requests
	sem := make(chan struct{}, 10)
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			mux.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"ok":false,"error":"too many concurrent requests"}`))
		}
	})

	srv := &http.Server{
		Handler:      limited,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 35 * time.Minute, // must exceed deploy timeout (30m)
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		srv.Shutdown(shutdownCtx)
	}()

	hostname, _ := os.Hostname()
	fmt.Fprintf(os.Stderr, "devops agent %s listening on %s (host: %s)\n", Version, socketPath, hostname)

	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
