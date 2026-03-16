//go:build !agent

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS apps (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT UNIQUE NOT NULL,
    host TEXT NOT NULL,
    port INTEGER NOT NULL DEFAULT 22,
    user TEXT NOT NULL DEFAULT 'root',
    runtime TEXT NOT NULL DEFAULT 'docker',
    service_name TEXT NOT NULL,
    compose_file TEXT NOT NULL DEFAULT '',
    repo_url TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT 'main',
    deploy_dir TEXT NOT NULL DEFAULT '',
    binary_path TEXT NOT NULL DEFAULT '',
    health_check_url TEXT NOT NULL DEFAULT '',
    deploy_commands TEXT NOT NULL DEFAULT '[]',
    notes TEXT NOT NULL DEFAULT '',
    key_path TEXT NOT NULL DEFAULT '',
    last_deploy_at TEXT,
    last_deploy_ok INTEGER,
    last_deploy_output TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_apps_host ON apps(host);

CREATE TABLE IF NOT EXISTS exec_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    app_name TEXT NOT NULL,
    host TEXT NOT NULL,
    command TEXT NOT NULL,
    args TEXT NOT NULL DEFAULT '[]',
    exit_code INTEGER,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS command_filters (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    host TEXT NOT NULL,
    pattern TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT 'custom',
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(host, pattern)
);

CREATE INDEX IF NOT EXISTS idx_command_filters_host ON command_filters(host);

CREATE TRIGGER IF NOT EXISTS apps_updated_at
AFTER UPDATE ON apps
BEGIN
    UPDATE apps SET updated_at = datetime('now') WHERE id = NEW.id;
END;
`

// App represents a deployed application record.
type App struct {
	ID               int64
	Name             string
	Host             string
	Port             int
	User             string
	Runtime          string
	ServiceName      string
	ComposeFile      string
	RepoURL          string
	Branch           string
	DeployDir        string
	BinaryPath       string
	HealthCheckURL   string
	DeployCommands   string
	Notes            string
	KeyPath          string
	LastDeployAt     sql.NullString
	LastDeployOK     sql.NullInt64
	LastDeployOutput string
	CreatedAt        string
	UpdatedAt        string
}

// store wraps the SQLite database connection.
type store struct {
	db *sql.DB
}

func newStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := conn.Exec(p); err != nil {
			closeErr := conn.Close()
			return nil, errors.Join(fmt.Errorf("set %s: %w", p, err), closeErr)
		}
	}

	if _, err := conn.Exec(schema); err != nil {
		closeErr := conn.Close()
		return nil, errors.Join(fmt.Errorf("create schema: %w", err), closeErr)
	}

	return &store{db: conn}, nil
}

func (s *store) close() error {
	return s.db.Close()
}

func dbPath() string {
	if p := os.Getenv("DEVOPS_DB_PATH"); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "devops.db")
}

// validateApp checks all fields that touch shell commands or SSH parameters.
func validateApp(a *App) error {
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := validateHostname(a.Host); err != nil {
		return fmt.Errorf("host: %w", err)
	}
	if err := validatePort(a.Port); err != nil {
		return err
	}
	if err := validateUsername(a.User); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if err := validateRuntime(a.Runtime); err != nil {
		return err
	}
	if err := validateServiceName(a.ServiceName); err != nil {
		return fmt.Errorf("service_name: %w", err)
	}
	if a.ComposeFile != "" {
		if err := validatePath(a.ComposeFile); err != nil {
			return fmt.Errorf("compose_file: %w", err)
		}
	}
	if a.Runtime == "docker" && a.ComposeFile == "" {
		return fmt.Errorf("compose_file is required when runtime is docker")
	}
	if a.DeployDir != "" {
		if err := validatePath(a.DeployDir); err != nil {
			return fmt.Errorf("deploy_dir: %w", err)
		}
	}
	if a.BinaryPath != "" {
		if err := validatePath(a.BinaryPath); err != nil {
			return fmt.Errorf("binary_path: %w", err)
		}
	}
	if a.DeployCommands != "" && a.DeployCommands != "[]" {
		if err := validateDeployCommands(a.DeployCommands); err != nil {
			return err
		}
	}
	if a.KeyPath != "" {
		if err := validateLocalPath(a.KeyPath); err != nil {
			return fmt.Errorf("key_path: %w", err)
		}
	}
	return nil
}

func (s *store) addApp(a *App) error {
	if err := validateApp(a); err != nil {
		return err
	}

	_, err := s.db.Exec(`
		INSERT INTO apps (name, host, port, user, runtime, service_name, compose_file,
			repo_url, branch, deploy_dir, binary_path, health_check_url, deploy_commands,
			notes, key_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.Host, a.Port, a.User, a.Runtime, a.ServiceName, a.ComposeFile,
		a.RepoURL, a.Branch, a.DeployDir, a.BinaryPath, a.HealthCheckURL, a.DeployCommands,
		a.Notes, a.KeyPath,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("app %q already exists, use devops_update to modify", a.Name)
		}
		return fmt.Errorf("insert app: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && len(err.Error()) > 6 && contains(err.Error(), "UNIQUE constraint failed")
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func (s *store) getApp(name string) (*App, error) {
	row := s.db.QueryRow(`
		SELECT id, name, host, port, user, runtime, service_name, compose_file,
			repo_url, branch, deploy_dir, binary_path, health_check_url, deploy_commands,
			notes, key_path, last_deploy_at, last_deploy_ok, last_deploy_output,
			created_at, updated_at
		FROM apps WHERE name = ?`, name)

	var a App
	err := row.Scan(
		&a.ID, &a.Name, &a.Host, &a.Port, &a.User, &a.Runtime, &a.ServiceName, &a.ComposeFile,
		&a.RepoURL, &a.Branch, &a.DeployDir, &a.BinaryPath, &a.HealthCheckURL, &a.DeployCommands,
		&a.Notes, &a.KeyPath, &a.LastDeployAt, &a.LastDeployOK, &a.LastDeployOutput,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("app %q not found", name)
		}
		return nil, fmt.Errorf("query app: %w", err)
	}
	return &a, nil
}

func (s *store) listApps(host string) (_ []App, err error) {
	var rows *sql.Rows

	if host != "" {
		rows, err = s.db.Query(`
			SELECT id, name, host, port, user, runtime, service_name, compose_file,
				repo_url, branch, deploy_dir, binary_path, health_check_url, deploy_commands,
				notes, key_path, last_deploy_at, last_deploy_ok, last_deploy_output,
				created_at, updated_at
			FROM apps WHERE host = ? ORDER BY name`, host)
	} else {
		rows, err = s.db.Query(`
			SELECT id, name, host, port, user, runtime, service_name, compose_file,
				repo_url, branch, deploy_dir, binary_path, health_check_url, deploy_commands,
				notes, key_path, last_deploy_at, last_deploy_ok, last_deploy_output,
				created_at, updated_at
			FROM apps ORDER BY name`)
	}
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Host, &a.Port, &a.User, &a.Runtime, &a.ServiceName, &a.ComposeFile,
			&a.RepoURL, &a.Branch, &a.DeployDir, &a.BinaryPath, &a.HealthCheckURL, &a.DeployCommands,
			&a.Notes, &a.KeyPath, &a.LastDeployAt, &a.LastDeployOK, &a.LastDeployOutput,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

func (s *store) removeApp(name string) error {
	result, err := s.db.Exec("DELETE FROM apps WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete app: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("app %q not found", name)
	}
	return nil
}

func (s *store) updateApp(a *App) error {
	if err := validateApp(a); err != nil {
		return err
	}

	_, err := s.db.Exec(`
		UPDATE apps SET host=?, port=?, user=?, runtime=?, service_name=?, compose_file=?,
			repo_url=?, branch=?, deploy_dir=?, binary_path=?, health_check_url=?,
			deploy_commands=?, notes=?, key_path=?
		WHERE name=?`,
		a.Host, a.Port, a.User, a.Runtime, a.ServiceName, a.ComposeFile,
		a.RepoURL, a.Branch, a.DeployDir, a.BinaryPath, a.HealthCheckURL,
		a.DeployCommands, a.Notes, a.KeyPath, a.Name,
	)
	if err != nil {
		return fmt.Errorf("update app: %w", err)
	}
	return nil
}

func (s *store) updateDeployResult(name string, ok sql.NullInt64, output string) error {
	_, err := s.db.Exec(`
		UPDATE apps SET last_deploy_at = datetime('now'), last_deploy_ok = ?, last_deploy_output = ?
		WHERE name = ?`, ok, output, name)
	if err != nil {
		return fmt.Errorf("update deploy result: %w", err)
	}
	return nil
}

func (s *store) logExec(appName, host, command, args string, exitCode int) error {
	_, err := s.db.Exec(`
		INSERT INTO exec_log (app_name, host, command, args, exit_code)
		VALUES (?, ?, ?, ?, ?)`, appName, host, command, args, exitCode)
	if err != nil {
		return fmt.Errorf("log exec: %w", err)
	}
	return nil
}

// CommandFilter represents a custom command filter rule.
type CommandFilter struct {
	ID        int64
	Host      string
	Pattern   string
	Category  string
	Reason    string
	CreatedAt string
}

func (s *store) addFilter(host, pattern, category, reason string) error {
	if category == "" {
		category = "custom"
	}
	_, err := s.db.Exec(`
		INSERT INTO command_filters (host, pattern, category, reason)
		VALUES (?, ?, ?, ?)`, host, pattern, category, reason)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("filter %q already exists for host %q", pattern, host)
		}
		return fmt.Errorf("insert filter: %w", err)
	}
	return nil
}

func (s *store) listFilters(host string) (_ []CommandFilter, err error) {
	var rows *sql.Rows
	if host != "" {
		rows, err = s.db.Query(`
			SELECT id, host, pattern, category, reason, created_at
			FROM command_filters WHERE host = ? ORDER BY host, pattern`, host)
	} else {
		rows, err = s.db.Query(`
			SELECT id, host, pattern, category, reason, created_at
			FROM command_filters ORDER BY host, pattern`)
	}
	if err != nil {
		return nil, fmt.Errorf("list filters: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	var filters []CommandFilter
	for rows.Next() {
		var f CommandFilter
		if err := rows.Scan(&f.ID, &f.Host, &f.Pattern, &f.Category, &f.Reason, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan filter: %w", err)
		}
		filters = append(filters, f)
	}
	return filters, rows.Err()
}

func (s *store) removeFilter(host, pattern string) error {
	result, err := s.db.Exec("DELETE FROM command_filters WHERE host = ? AND pattern = ?", host, pattern)
	if err != nil {
		return fmt.Errorf("delete filter: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("filter %q not found for host %q", pattern, host)
	}
	return nil
}

func (s *store) filtersForHost(host string) ([]CommandFilter, error) {
	return s.listFilters(host)
}
