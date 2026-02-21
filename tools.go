//go:build !agent

package main

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "devops_list",
			"description": "List all registered apps. Optionally filter by host.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host": map[string]any{
						"type":        "string",
						"description": "Filter apps by host",
					},
				},
			},
		},
		{
			"name":        "devops_add",
			"description": "Register a new app. Requires name, host, and service_name. Runtime defaults to 'docker'.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":             prop("string", "Unique app name"),
					"host":             prop("string", "Server hostname or IP"),
					"service_name":     prop("string", "Systemd unit or docker compose service name"),
					"port":             propDefault("integer", "SSH port", 22),
					"user":             propDefault("string", "SSH user", "root"),
					"runtime":          propDefault("string", "Runtime: 'docker' or 'systemd'", "docker"),
					"compose_file":     prop("string", "Path to compose file (docker runtime)"),
					"repo_url":         prop("string", "Git repository URL"),
					"branch":           propDefault("string", "Git branch", "main"),
					"deploy_dir":       prop("string", "Deployment directory on server"),
					"binary_path":      prop("string", "Binary path (systemd runtime)"),
					"health_check_url": prop("string", "Health check URL"),
					"deploy_commands":  prop("string", "JSON array of deploy commands"),
					"notes":            prop("string", "Notes about this app"),
					"key_path":         prop("string", "SSH key path override"),
				},
				"required": []string{"name", "host", "service_name"},
			},
		},
		{
			"name":        "devops_import",
			"description": "Import an existing deployment by discovering its configuration via the remote agent. Requires agent to be running (use devops_bootstrap first).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":             prop("string", "Unique app name"),
					"host":             prop("string", "Server hostname or IP"),
					"deploy_dir":       prop("string", "Deployment directory to discover"),
					"port":             propDefault("integer", "SSH port", 22),
					"user":             propDefault("string", "SSH user", "root"),
					"service_name":     prop("string", "Override discovered service name"),
					"runtime":          prop("string", "Override discovered runtime"),
					"compose_file":     prop("string", "Override discovered compose file"),
					"repo_url":         prop("string", "Override discovered repo URL"),
					"branch":           prop("string", "Override discovered branch"),
					"health_check_url": prop("string", "Health check URL"),
					"deploy_commands":  prop("string", "JSON array of deploy commands"),
					"notes":            prop("string", "Notes about this app"),
					"key_path":         prop("string", "SSH key path override"),
				},
				"required": []string{"name", "host", "deploy_dir"},
			},
		},
		{
			"name":        "devops_remove",
			"description": "Remove an app registration. Does not affect the running service.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": prop("string", "App name to remove"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_update",
			"description": "Update an app's configuration. Only provided fields are changed.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":             prop("string", "App name to update"),
					"host":             prop("string", "Server hostname or IP"),
					"service_name":     prop("string", "Systemd unit or docker compose service name"),
					"port":             prop("integer", "SSH port"),
					"user":             prop("string", "SSH user"),
					"runtime":          prop("string", "Runtime: 'docker' or 'systemd'"),
					"compose_file":     prop("string", "Path to compose file"),
					"repo_url":         prop("string", "Git repository URL"),
					"branch":           prop("string", "Git branch"),
					"deploy_dir":       prop("string", "Deployment directory on server"),
					"binary_path":      prop("string", "Binary path (systemd runtime)"),
					"health_check_url": prop("string", "Health check URL"),
					"deploy_commands":  prop("string", "JSON array of deploy commands"),
					"notes":            prop("string", "Notes"),
					"key_path":         prop("string", "SSH key path override"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_status",
			"description": "Get the current status of an app's service via the remote agent.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": prop("string", "App name"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_deploy",
			"description": "Deploy an app by running its deploy commands on the server. Uses streaming output.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     prop("string", "App name"),
					"commands": prop("string", "JSON array of commands (overrides stored deploy_commands)"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_restart",
			"description": "Restart an app's service via the remote agent.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": prop("string", "App name"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_stop",
			"description": "Stop an app's service via the remote agent.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": prop("string", "App name"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_logs",
			"description": "Get recent logs for an app's service.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":  prop("string", "App name"),
					"lines": propDefault("integer", "Number of log lines", 50),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_exec",
			"description": "Execute a command on the server hosting an app. Supports shell syntax (pipes, redirects, paths). Simple binaries can use separate args.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":    prop("string", "App name"),
					"command": prop("string", "Binary name to execute"),
					"args":    prop("array", "Command arguments"),
				},
				"required": []string{"name", "command"},
			},
		},
		{
			"name":        "devops_health",
			"description": "Check the health endpoint of an app.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": prop("string", "App name"),
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "devops_bootstrap",
			"description": "Install or upgrade the devops agent on a host. Idempotent: safe to run repeatedly. Applies server configs on fresh install.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":     prop("string", "Server hostname or IP"),
					"user":     propDefault("string", "SSH user", "root"),
					"port":     propDefault("integer", "SSH port", 22),
					"key_path": prop("string", "SSH key path override"),
				},
				"required": []string{"host"},
			},
		},
	}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{
		"type":        typ,
		"description": desc,
	}
}

func propDefault(typ, desc string, def any) map[string]any {
	return map[string]any{
		"type":        typ,
		"description": desc,
		"default":     def,
	}
}
