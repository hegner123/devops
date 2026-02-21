//go:build !agent

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const agentSocket = "/run/devops-agent/agent.sock"

// remoteAgent defines the interface for communicating with the remote agent.
type remoteAgent interface {
	call(ctx context.Context, app *App, endpoint string, req any) (*AgentResponse, error)
	callStream(ctx context.Context, app *App, endpoint string, req any) (io.ReadCloser, error)
}

// AgentResponse is the standard response envelope from the agent.
type AgentResponse struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Stdout string          `json:"stdout,omitempty"`
	Stderr string          `json:"stderr,omitempty"`
	Exit   *int            `json:"exit,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Output string          `json:"output,omitempty"`
	Status int             `json:"status,omitempty"`
	Body   string          `json:"body,omitempty"`
}

type agentClient struct {
	pool *connPool
}

func (a *agentClient) call(ctx context.Context, app *App, endpoint string, req any) (*AgentResponse, error) {
	resp, err := a.doCall(ctx, app, endpoint, req, 30*time.Second)
	if err != nil {
		// Retry once: pool.get handles invalidation and redialing
		resp, err = a.doCall(ctx, app, endpoint, req, 30*time.Second)
	}
	return resp, err
}

func (a *agentClient) doCall(ctx context.Context, app *App, endpoint string, reqBody any, timeout time.Duration) (*AgentResponse, error) {
	client, err := a.pool.get(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("ssh connect: %w", err)
	}

	conn, err := client.Dial("unix", agentSocket)
	if err != nil {
		return nil, fmt.Errorf("dial agent socket: %w", err)
	}
	defer conn.Close()

	pr, pw := io.Pipe()
	go func() {
		err := json.NewEncoder(pw).Encode(reqBody)
		pw.CloseWithError(err)
	}()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://agent"+endpoint, pr)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return conn, nil
			},
		},
	}

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agent request %s: %w", endpoint, err)
	}
	defer httpResp.Body.Close()

	var agentResp AgentResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&agentResp); err != nil {
		return nil, fmt.Errorf("decode agent response: %w", err)
	}

	return &agentResp, nil
}

func (a *agentClient) callStream(ctx context.Context, app *App, endpoint string, reqBody any) (io.ReadCloser, error) {
	reader, err := a.doCallStream(ctx, app, endpoint, reqBody)
	if err != nil {
		// Retry once
		reader, err = a.doCallStream(ctx, app, endpoint, reqBody)
	}
	return reader, err
}

func (a *agentClient) doCallStream(ctx context.Context, app *App, endpoint string, reqBody any) (io.ReadCloser, error) {
	client, err := a.pool.get(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("ssh connect: %w", err)
	}

	conn, err := client.Dial("unix", agentSocket)
	if err != nil {
		return nil, fmt.Errorf("dial agent socket: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		err := json.NewEncoder(pw).Encode(reqBody)
		pw.CloseWithError(err)
	}()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://agent"+endpoint, pr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// No timeout for streaming - the caller manages via context
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
		return nil, fmt.Errorf("agent request %s: %w", endpoint, err)
	}

	// Return body for streaming; caller must close
	// Wrap to also close the SSH channel conn when body is closed
	return &streamReader{body: httpResp.Body, conn: conn}, nil
}

type streamReader struct {
	body io.ReadCloser
	conn net.Conn
}

func (s *streamReader) Read(p []byte) (int, error) {
	return s.body.Read(p)
}

func (s *streamReader) Close() error {
	err := s.body.Close()
	s.conn.Close()
	return err
}
