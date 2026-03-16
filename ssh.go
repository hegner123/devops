//go:build !agent

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

//go:embed embed/*
var serverFiles embed.FS

const defaultKeyCacheKey = "__default__"

type connEntry struct {
	client *ssh.Client
	cancel context.CancelFunc
}

type connPool struct {
	mu       sync.Mutex
	conns    map[string]*connEntry
	hostKeys map[string]ssh.Signer // per-host key cache (keyed by file path)
}

func newConnPool() *connPool {
	return &connPool{
		conns:    make(map[string]*connEntry),
		hostKeys: make(map[string]ssh.Signer),
	}
}

func poolKey(user, host string, port int) string {
	return fmt.Sprintf("%s@%s:%d", user, host, port)
}

func (p *connPool) get(ctx context.Context, app *App) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(app.User, app.Host, app.Port)

	if entry, ok := p.conns[key]; ok {
		// Liveness check
		_, _, err := entry.client.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			return entry.client, nil
		}
		// Dead connection, clean up
		entry.cancel()
		entry.client.Close()
		delete(p.conns, key)
	}

	signer, err := p.signerForApp(app)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:            app.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(app.Host, fmt.Sprintf("%d", app.Port))

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	var d net.Dialer
	rawConn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, config)
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}

	client := ssh.NewClient(sshConn, chans, reqs)

	kaCtx, kaCancel := context.WithCancel(context.Background())
	entry := &connEntry{
		client: client,
		cancel: kaCancel,
	}
	p.conns[key] = entry

	go p.keepalive(kaCtx, key, entry)

	return client, nil
}

func (p *connPool) signerForApp(app *App) (ssh.Signer, error) {
	cacheKey := app.KeyPath
	if cacheKey == "" {
		cacheKey = defaultKeyCacheKey
	}

	// Check cache
	if signer, ok := p.hostKeys[cacheKey]; ok {
		return signer, nil
	}

	if app.KeyPath != "" {
		return p.loadAndCacheKey(app.KeyPath, cacheKey)
	}

	// Try default key paths
	for _, path := range getDefaultKeyPaths() {
		signer, err := p.loadAndCacheKey(path, cacheKey)
		if err == nil {
			return signer, nil
		}
	}

	return nil, fmt.Errorf("no SSH key found: set key_path per app or place a key in your .ssh directory (id_ed25519 or id_rsa)")
}

func (p *connPool) loadAndCacheKey(path, cacheKey string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}

	p.hostKeys[cacheKey] = signer
	return signer, nil
}

func (p *connPool) keepalive(ctx context.Context, key string, entry *connEntry) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _, err := entry.client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				p.mu.Lock()
				if current, ok := p.conns[key]; ok && current == entry {
					delete(p.conns, key)
				}
				p.mu.Unlock()
				entry.client.Close()
				return
			}
		}
	}
}

func (p *connPool) close(user, host string, port int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(user, host, port)
	if entry, ok := p.conns[key]; ok {
		entry.cancel()
		entry.client.Close()
		delete(p.conns, key)
	}
}

func (p *connPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for key, entry := range p.conns {
		entry.cancel()
		entry.client.Close()
		delete(p.conns, key)
	}
}

// sshExec runs a command string on the remote host via SSH session.
// The command is executed by the remote shell, so shell syntax (pipes, redirects) works.
func sshExec(client *ssh.Client, cmd string) (stdout, stderr string, exitCode int, err error) {
	session, sessErr := client.NewSession()
	if sessErr != nil {
		return "", "", -1, fmt.Errorf("new session: %w", sessErr)
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf

	runErr := session.Run(cmd)
	if runErr != nil {
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			return outBuf.String(), errBuf.String(), exitErr.ExitStatus(), nil
		}
		return "", "", -1, fmt.Errorf("ssh exec: %w", runErr)
	}
	return outBuf.String(), errBuf.String(), 0, nil
}

// sshExecStdin runs a command on the remote host with stdin content.
func sshExecStdin(client *ssh.Client, cmd, stdin string) (stdout, stderr string, exitCode int, err error) {
	session, sessErr := client.NewSession()
	if sessErr != nil {
		return "", "", -1, fmt.Errorf("new session: %w", sessErr)
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf
	session.Stdin = strings.NewReader(stdin)

	runErr := session.Run(cmd)
	if runErr != nil {
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			return outBuf.String(), errBuf.String(), exitErr.ExitStatus(), nil
		}
		return "", "", -1, fmt.Errorf("ssh exec: %w", runErr)
	}
	return outBuf.String(), errBuf.String(), 0, nil
}

// sshWriteFile writes content to a file on the remote host via SSH.
func sshWriteFile(client *ssh.Client, remotePath, content string) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	session.Stdin = strings.NewReader(content)
	cmd := fmt.Sprintf("mkdir -p '%s' && cat > '%s'",
		remotePath[:strings.LastIndex(remotePath, "/")], remotePath)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("write %s: %w", remotePath, err)
	}
	return nil
}

// readEmbedFile reads a file from the embedded server config filesystem.
func readEmbedFile(name string) (string, error) {
	data, err := fs.ReadFile(serverFiles, name)
	if err != nil {
		return "", fmt.Errorf("read embedded %s: %w", name, err)
	}
	return string(data), nil
}

// agentPing pings the agent via SSH channel forwarding to the unix socket.
// Returns the agent version string on success.
func agentPing(client *ssh.Client) (string, error) {
	conn, err := client.Dial("unix", agentSocket)
	if err != nil {
		return "", fmt.Errorf("dial agent socket: %w", err)
	}
	defer conn.Close()

	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return conn, nil
			},
		},
	}

	resp, err := httpClient.Get("http://agent/ping")
	if err != nil {
		return "", fmt.Errorf("ping agent: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode ping: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("agent ping returned ok=false")
	}
	return result.Version, nil
}
