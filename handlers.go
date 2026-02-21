//go:build !agent

package main

import (
	"encoding/json"
	"fmt"
)

// Phase 1: local CRUD handlers (devops_list, devops_add, devops_remove, devops_update)
// Phase 3/4 will add remote agent handlers

func (s *server) devopsList(args map[string]any) (string, bool) {
	host, _ := args["host"].(string)

	apps, err := s.store.listApps(host)
	if err != nil {
		return fmt.Sprintf("failed to list apps: %v", err), true
	}

	if len(apps) == 0 {
		if host != "" {
			return fmt.Sprintf("no apps on host %s", host), false
		}
		return "no apps registered", false
	}

	type appSummary struct {
		Name    string `json:"name"`
		Host    string `json:"host"`
		Svc     string `json:"svc"`
		RT      string `json:"rt"`
		Deploy  string `json:"deploy,omitempty"`
		Status  string `json:"status,omitempty"`
	}

	summaries := make([]appSummary, len(apps))
	for i, a := range apps {
		summary := appSummary{
			Name: a.Name,
			Host: a.Host,
			Svc:  a.ServiceName,
			RT:   a.Runtime,
		}
		if a.LastDeployAt.Valid {
			summary.Deploy = a.LastDeployAt.String
		}
		if a.LastDeployOK.Valid {
			if a.LastDeployOK.Int64 == 1 {
				summary.Status = "ok"
			} else {
				summary.Status = "fail"
			}
		}
		summaries[i] = summary
	}

	out, err := json.Marshal(summaries)
	if err != nil {
		return fmt.Sprintf("failed to marshal apps: %v", err), true
	}
	return string(out), false
}

func (s *server) devopsAdd(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	host, _ := args["host"].(string)
	serviceName, _ := args["service_name"].(string)

	if name == "" || host == "" || serviceName == "" {
		return "name, host, and service_name are required", true
	}

	app := &App{
		Name:        name,
		Host:        host,
		Port:        intArg(args, "port", 22),
		User:        strArg(args, "user", "root"),
		Runtime:     strArg(args, "runtime", "docker"),
		ServiceName: serviceName,
		ComposeFile: strArg(args, "compose_file", ""),
		RepoURL:     strArg(args, "repo_url", ""),
		Branch:      strArg(args, "branch", "main"),
		DeployDir:   strArg(args, "deploy_dir", ""),
		BinaryPath:  strArg(args, "binary_path", ""),
		HealthCheckURL: strArg(args, "health_check_url", ""),
		DeployCommands: strArg(args, "deploy_commands", "[]"),
		Notes:       strArg(args, "notes", ""),
		KeyPath:     strArg(args, "key_path", ""),
	}

	if err := s.store.addApp(app); err != nil {
		return err.Error(), true
	}

	return fmt.Sprintf("added app %q on %s", name, host), false
}

func (s *server) devopsRemove(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	if err := s.store.removeApp(name); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("removed app %q", name), false
}

func (s *server) devopsUpdate(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}

	app, err := s.store.getApp(name)
	if err != nil {
		return err.Error(), true
	}

	// Read-modify-write: apply only provided fields
	if v, ok := args["host"].(string); ok && v != "" {
		app.Host = v
	}
	if v, ok := args["port"]; ok {
		app.Port = toInt(v, app.Port)
	}
	if v, ok := args["user"].(string); ok && v != "" {
		app.User = v
	}
	if v, ok := args["runtime"].(string); ok && v != "" {
		app.Runtime = v
	}
	if v, ok := args["service_name"].(string); ok && v != "" {
		app.ServiceName = v
	}
	if v, ok := args["compose_file"].(string); ok {
		app.ComposeFile = v
	}
	if v, ok := args["repo_url"].(string); ok {
		app.RepoURL = v
	}
	if v, ok := args["branch"].(string); ok && v != "" {
		app.Branch = v
	}
	if v, ok := args["deploy_dir"].(string); ok {
		app.DeployDir = v
	}
	if v, ok := args["binary_path"].(string); ok {
		app.BinaryPath = v
	}
	if v, ok := args["health_check_url"].(string); ok {
		app.HealthCheckURL = v
	}
	if v, ok := args["deploy_commands"].(string); ok && v != "" {
		app.DeployCommands = v
	}
	if v, ok := args["notes"].(string); ok {
		app.Notes = v
	}
	if v, ok := args["key_path"].(string); ok {
		app.KeyPath = v
	}

	if err := s.store.updateApp(app); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("updated app %q", name), false
}

// Argument helpers

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		return toInt(v, def)
	}
	return def
}

func toInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return def
		}
		return int(i)
	}
	return def
}
