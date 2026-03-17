//go:build !agent

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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

func poolKey(user, host string, port int, keyPath string) string {
	if keyPath == "" {
		return fmt.Sprintf("%s@%s:%d", user, host, port)
	}
	h := sha256.Sum256([]byte(keyPath))
	return fmt.Sprintf("%s@%s:%d:%s", user, host, port, hex.EncodeToString(h[:8]))
}

func (p *connPool) get(ctx context.Context, app *App) (*ssh.Client, error) {
	key := poolKey(app.User, app.Host, app.Port, app.KeyPath)

	// Check for existing connection and do keepalive outside the lock.
	p.mu.Lock()
	entry, found := p.conns[key]
	p.mu.Unlock()

	if found {
		// Liveness check outside mutex to avoid pool-wide blocking on slow connections.
		_, _, err := entry.client.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			return entry.client, nil
		}
		// Dead connection, clean up under lock.
		p.mu.Lock()
		if current, ok := p.conns[key]; ok && current == entry {
			entry.cancel()
			entry.client.Close()
			delete(p.conns, key)
		}
		p.mu.Unlock()
	}

	signer, err := p.signerForApp(app)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := tofuHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("host key setup: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            app.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
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
	newEntry := &connEntry{
		client: client,
		cancel: kaCancel,
	}

	p.mu.Lock()
	p.conns[key] = newEntry
	p.mu.Unlock()

	go p.keepalive(kaCtx, key, newEntry)

	return client, nil
}

// tofuHostKeyCallback implements Trust On First Use host key verification.
// On first connection to a host, the key is accepted and saved to known_hosts.
// On subsequent connections, the key is verified against the stored key.
func tofuHostKeyCallback() (ssh.HostKeyCallback, error) {
	khPath := filepath.Join(dataDir(), "known_hosts")

	// Ensure the directory and file exist.
	if err := os.MkdirAll(filepath.Dir(khPath), 0o700); err != nil {
		return nil, fmt.Errorf("create known_hosts dir: %w", err)
	}
	if _, err := os.Stat(khPath); os.IsNotExist(err) {
		f, createErr := os.OpenFile(khPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if createErr != nil {
			return nil, fmt.Errorf("create known_hosts: %w", createErr)
		}
		f.Close()
	}

	callback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// Try to verify against existing known_hosts.
		kh, err := knownhosts.New(khPath)
		if err != nil {
			return fmt.Errorf("read known_hosts: %w", err)
		}

		err = kh(hostname, remote, key)
		if err == nil {
			return nil // Known host, key matches.
		}

		// Check if it's a key-changed error (possible MITM).
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
			return fmt.Errorf("host key changed for %s — possible MITM attack (run devops to inspect)", hostname)
		}

		// Unknown host — trust on first use: append the key.
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, openErr := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr != nil {
			return fmt.Errorf("open known_hosts for writing: %w", openErr)
		}
		defer f.Close()

		if _, writeErr := fmt.Fprintln(f, line); writeErr != nil {
			return fmt.Errorf("write known_hosts: %w", writeErr)
		}
		return nil
	}

	return callback, nil
}

func (p *connPool) signerForApp(app *App) (ssh.Signer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

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

func (p *connPool) close(user, host string, port int, keyPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(user, host, port, keyPath)
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

// shellQuote wraps a string in single quotes for safe use in shell commands.
// Single quotes within the string are escaped as '\” (end quote, escaped quote, start quote).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// sshExec runs a command string on the remote host via SSH session.
// The command is executed by the remote shell, so shell syntax (pipes, redirects) works.
// A 2-minute timeout is enforced via context.
func sshExec(ctx context.Context, client *ssh.Client, cmd string) (stdout, stderr string, exitCode int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	session, sessErr := client.NewSession()
	if sessErr != nil {
		return "", "", -1, fmt.Errorf("new session: %w", sessErr)
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return "", "", -1, fmt.Errorf("ssh exec timed out: %w", ctx.Err())
	case runErr := <-done:
		if runErr != nil {
			if exitErr, ok := runErr.(*ssh.ExitError); ok {
				return outBuf.String(), errBuf.String(), exitErr.ExitStatus(), nil
			}
			return "", "", -1, fmt.Errorf("ssh exec: %w", runErr)
		}
		return outBuf.String(), errBuf.String(), 0, nil
	}
}

// sshExecStdin runs a command on the remote host with stdin content.
// A 2-minute timeout is enforced via context.
func sshExecStdin(ctx context.Context, client *ssh.Client, cmd, stdin string) (stdout, stderr string, exitCode int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	session, sessErr := client.NewSession()
	if sessErr != nil {
		return "", "", -1, fmt.Errorf("new session: %w", sessErr)
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf
	session.Stdin = strings.NewReader(stdin)

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return "", "", -1, fmt.Errorf("ssh exec timed out: %w", ctx.Err())
	case runErr := <-done:
		if runErr != nil {
			if exitErr, ok := runErr.(*ssh.ExitError); ok {
				return outBuf.String(), errBuf.String(), exitErr.ExitStatus(), nil
			}
			return "", "", -1, fmt.Errorf("ssh exec: %w", runErr)
		}
		return outBuf.String(), errBuf.String(), 0, nil
	}
}

// sshWriteFile writes content to a file on the remote host via SSH.
func sshWriteFile(ctx context.Context, client *ssh.Client, remotePath, content string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	session.Stdin = strings.NewReader(content)
	dirPath := remotePath[:strings.LastIndex(remotePath, "/")]
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dirPath), shellQuote(remotePath))

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return fmt.Errorf("write %s timed out: %w", remotePath, ctx.Err())
	case runErr := <-done:
		if runErr != nil {
			return fmt.Errorf("write %s: %w", remotePath, runErr)
		}
		return nil
	}
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
