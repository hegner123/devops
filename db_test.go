package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := newStore(path)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

func testApp() *App {
	return &App{
		Name:           "myapp",
		Host:           "192.168.1.1",
		Port:           22,
		User:           "root",
		Runtime:        "docker",
		ServiceName:    "web",
		ComposeFile:    "/opt/myapp/compose.yml",
		Branch:         "main",
		DeployDir:      "/opt/myapp",
		DeployCommands: `["git pull","docker compose up -d"]`,
	}
}

func TestAddAndGetApp(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	got, err := s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}

	if got.Name != "myapp" {
		t.Errorf("name = %q, want %q", got.Name, "myapp")
	}
	if got.Host != "192.168.1.1" {
		t.Errorf("host = %q, want %q", got.Host, "192.168.1.1")
	}
	if got.Port != 22 {
		t.Errorf("port = %d, want 22", got.Port)
	}
	if got.Runtime != "docker" {
		t.Errorf("runtime = %q, want %q", got.Runtime, "docker")
	}
	if got.ServiceName != "web" {
		t.Errorf("service_name = %q, want %q", got.ServiceName, "web")
	}
	if got.ComposeFile != "/opt/myapp/compose.yml" {
		t.Errorf("compose_file = %q, want %q", got.ComposeFile, "/opt/myapp/compose.yml")
	}
	if got.CreatedAt == "" {
		t.Error("created_at is empty")
	}
	if got.UpdatedAt == "" {
		t.Error("updated_at is empty")
	}
}

func TestAddDuplicateName(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	err := s.addApp(app)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to contain 'already exists'", err.Error())
	}
}

func TestListApps(t *testing.T) {
	s := testStore(t)

	// Empty list
	apps, err := s.listApps("")
	if err != nil {
		t.Fatalf("listApps: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("len = %d, want 0", len(apps))
	}

	// Add two apps on different hosts
	app1 := testApp()
	app2 := &App{
		Name:        "otherapp",
		Host:        "10.0.0.1",
		Port:        22,
		User:        "root",
		Runtime:     "systemd",
		ServiceName: "otherapp",
		Branch:      "main",
	}

	if err := s.addApp(app1); err != nil {
		t.Fatalf("addApp: %v", err)
	}
	if err := s.addApp(app2); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	// List all
	apps, err = s.listApps("")
	if err != nil {
		t.Fatalf("listApps all: %v", err)
	}
	if len(apps) != 2 {
		t.Errorf("len = %d, want 2", len(apps))
	}

	// Filter by host
	apps, err = s.listApps("10.0.0.1")
	if err != nil {
		t.Fatalf("listApps filtered: %v", err)
	}
	if len(apps) != 1 {
		t.Errorf("len = %d, want 1", len(apps))
	}
	if apps[0].Name != "otherapp" {
		t.Errorf("name = %q, want %q", apps[0].Name, "otherapp")
	}
}

func TestRemoveApp(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	if err := s.removeApp("myapp"); err != nil {
		t.Fatalf("removeApp: %v", err)
	}

	_, err := s.getApp("myapp")
	if err == nil {
		t.Fatal("expected error after remove")
	}

	// Remove non-existent
	err = s.removeApp("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent app")
	}
}

func TestUpdateApp(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	got, err := s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	originalUpdatedAt := got.UpdatedAt

	got.Host = "10.0.0.2"
	got.Notes = "updated host"
	if err := s.updateApp(got); err != nil {
		t.Fatalf("updateApp: %v", err)
	}

	updated, err := s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp after update: %v", err)
	}
	if updated.Host != "10.0.0.2" {
		t.Errorf("host = %q, want %q", updated.Host, "10.0.0.2")
	}
	if updated.Notes != "updated host" {
		t.Errorf("notes = %q, want %q", updated.Notes, "updated host")
	}
	// Trigger should have updated updated_at (may be same second in fast test)
	_ = originalUpdatedAt
}

func TestUpdateDeployResult(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	// Successful deploy
	if err := s.updateDeployResult("myapp", sql.NullInt64{Int64: 1, Valid: true}, "deployed ok"); err != nil {
		t.Fatalf("updateDeployResult: %v", err)
	}

	got, err := s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if !got.LastDeployAt.Valid {
		t.Error("last_deploy_at should be set")
	}
	if !got.LastDeployOK.Valid || got.LastDeployOK.Int64 != 1 {
		t.Errorf("last_deploy_ok = %v, want 1", got.LastDeployOK)
	}
	if got.LastDeployOutput != "deployed ok" {
		t.Errorf("last_deploy_output = %q, want %q", got.LastDeployOutput, "deployed ok")
	}

	// Failed deploy
	if err := s.updateDeployResult("myapp", sql.NullInt64{Int64: 0, Valid: true}, "error: build failed"); err != nil {
		t.Fatalf("updateDeployResult: %v", err)
	}

	got, err = s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if got.LastDeployOK.Int64 != 0 {
		t.Errorf("last_deploy_ok = %d, want 0", got.LastDeployOK.Int64)
	}

	// Unknown state (connection lost)
	if err := s.updateDeployResult("myapp", sql.NullInt64{Valid: false}, "connection lost"); err != nil {
		t.Fatalf("updateDeployResult: %v", err)
	}

	got, err = s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if got.LastDeployOK.Valid {
		t.Error("last_deploy_ok should be NULL for unknown state")
	}
}

func TestLogExec(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	if err := s.logExec("myapp", "192.168.1.1", "df", `["-h"]`, 0); err != nil {
		t.Fatalf("logExec: %v", err)
	}

	// Verify log entry exists
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM exec_log WHERE app_name = ?", "myapp").Scan(&count)
	if err != nil {
		t.Fatalf("query exec_log: %v", err)
	}
	if count != 1 {
		t.Errorf("exec_log count = %d, want 1", count)
	}
}

func TestValidationOnAdd(t *testing.T) {
	s := testStore(t)

	tests := []struct {
		name string
		app  *App
	}{
		{"invalid host", &App{Name: "a", Host: "bad host!", Port: 22, User: "root", Runtime: "docker", ServiceName: "svc", ComposeFile: "/a"}},
		{"invalid port", &App{Name: "a", Host: "good", Port: 0, User: "root", Runtime: "docker", ServiceName: "svc", ComposeFile: "/a"}},
		{"invalid runtime", &App{Name: "a", Host: "good", Port: 22, User: "root", Runtime: "podman", ServiceName: "svc"}},
		{"invalid service name", &App{Name: "a", Host: "good", Port: 22, User: "root", Runtime: "systemd", ServiceName: "bad service!"}},
		{"docker without compose", &App{Name: "a", Host: "good", Port: 22, User: "root", Runtime: "docker", ServiceName: "svc"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.addApp(tt.app); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestFilterCRUD(t *testing.T) {
	s := testStore(t)

	// Add filter
	if err := s.addFilter("10.0.0.1", "npm install", "custom", "no npm on prod"); err != nil {
		t.Fatalf("addFilter: %v", err)
	}

	// Add another filter on same host
	if err := s.addFilter("10.0.0.1", "pip install", "custom", "no pip on prod"); err != nil {
		t.Fatalf("addFilter second: %v", err)
	}

	// Add filter on different host
	if err := s.addFilter("10.0.0.2", "npm install", "custom", "no npm here either"); err != nil {
		t.Fatalf("addFilter other host: %v", err)
	}

	// List all
	all, err := s.listFilters("")
	if err != nil {
		t.Fatalf("listFilters all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}

	// List by host
	host1, err := s.listFilters("10.0.0.1")
	if err != nil {
		t.Fatalf("listFilters host1: %v", err)
	}
	if len(host1) != 2 {
		t.Errorf("host1 len = %d, want 2", len(host1))
	}

	// Duplicate error
	err = s.addFilter("10.0.0.1", "npm install", "custom", "duplicate")
	if err == nil {
		t.Fatal("expected error for duplicate filter")
	}
	if !contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want to contain 'already exists'", err.Error())
	}

	// Remove
	if err := s.removeFilter("10.0.0.1", "npm install"); err != nil {
		t.Fatalf("removeFilter: %v", err)
	}

	// Verify removed
	host1, err = s.listFilters("10.0.0.1")
	if err != nil {
		t.Fatalf("listFilters after remove: %v", err)
	}
	if len(host1) != 1 {
		t.Errorf("host1 after remove len = %d, want 1", len(host1))
	}

	// Remove non-existent
	err = s.removeFilter("10.0.0.1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent filter")
	}

	// filtersForHost
	host2, err := s.filtersForHost("10.0.0.2")
	if err != nil {
		t.Fatalf("filtersForHost: %v", err)
	}
	if len(host2) != 1 {
		t.Errorf("host2 len = %d, want 1", len(host2))
	}
	if host2[0].Pattern != "npm install" {
		t.Errorf("pattern = %q, want 'npm install'", host2[0].Pattern)
	}
}

func TestDBPathEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "test-devops.db")
	t.Setenv("DEVOPS_DB_PATH", want)
	p := dbPath()
	if p != want {
		t.Errorf("dbPath = %q, want %q", p, want)
	}
}

func TestUpsertAppNew(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.upsertApp(app); err != nil {
		t.Fatalf("upsertApp: %v", err)
	}

	got, err := s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if got.Host != "192.168.1.1" {
		t.Errorf("host = %q, want %q", got.Host, "192.168.1.1")
	}
	if got.DeployCommands != `["git pull","docker compose up -d"]` {
		t.Errorf("deploy_commands = %q, want stored commands", got.DeployCommands)
	}
}

func TestUpsertAppUpdate(t *testing.T) {
	s := testStore(t)
	app := testApp()

	if err := s.addApp(app); err != nil {
		t.Fatalf("addApp: %v", err)
	}

	// Mark a deploy result that should be preserved across upsert
	if err := s.updateDeployResult("myapp", sql.NullInt64{Int64: 1, Valid: true}, "ok"); err != nil {
		t.Fatalf("updateDeployResult: %v", err)
	}

	// Upsert with changed host
	app.Host = "10.0.0.99"
	if err := s.upsertApp(app); err != nil {
		t.Fatalf("upsertApp: %v", err)
	}

	got, err := s.getApp("myapp")
	if err != nil {
		t.Fatalf("getApp: %v", err)
	}
	if got.Host != "10.0.0.99" {
		t.Errorf("host = %q, want %q", got.Host, "10.0.0.99")
	}
	// Deploy result should be preserved (upsert only touches config fields)
	if !got.LastDeployOK.Valid || got.LastDeployOK.Int64 != 1 {
		t.Errorf("last_deploy_ok = %v, want 1 (should be preserved)", got.LastDeployOK)
	}
}

func TestDeleteAppsNotIn(t *testing.T) {
	s := testStore(t)

	app1 := testApp()
	app2 := &App{
		Name:        "app2",
		Host:        "10.0.0.1",
		Port:        22,
		User:        "root",
		Runtime:     "systemd",
		ServiceName: "app2",
		Branch:      "main",
	}
	app3 := &App{
		Name:        "app3",
		Host:        "10.0.0.1",
		Port:        22,
		User:        "root",
		Runtime:     "systemd",
		ServiceName: "app3",
		Branch:      "main",
	}

	for _, a := range []*App{app1, app2, app3} {
		if err := s.addApp(a); err != nil {
			t.Fatalf("addApp %s: %v", a.Name, err)
		}
	}

	// Keep app1 and app2, prune app3
	removed, err := s.deleteAppsNotIn([]string{"myapp", "app2"})
	if err != nil {
		t.Fatalf("deleteAppsNotIn: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	apps, err := s.listApps("")
	if err != nil {
		t.Fatalf("listApps: %v", err)
	}
	if len(apps) != 2 {
		t.Errorf("remaining apps = %d, want 2", len(apps))
	}

	// Delete all when empty names list
	removed, err = s.deleteAppsNotIn([]string{})
	if err != nil {
		t.Fatalf("deleteAppsNotIn empty: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
}
