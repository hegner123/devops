package main

import "testing"

func TestValidateServiceName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "nginx", false},
		{"valid with dots", "my.service", false},
		{"valid systemd template", "foo@bar", false},
		{"valid with dash", "my-app", false},
		{"valid with underscore", "my_app", false},
		{"valid with colon", "app:web", false},
		{"empty", "", true},
		{"space", "my app", true},
		{"slash", "my/app", true},
		{"too long", string(make([]byte, 257)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServiceName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateServiceName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateHostname(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid ip", "192.168.1.1", false},
		{"valid domain", "example.com", false},
		{"valid subdomain", "app.example.com", false},
		{"empty", "", true},
		{"space", "my host", true},
		{"underscore", "my_host", true},
		{"too long", string(make([]byte, 254)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHostname(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostname(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid root", "root", false},
		{"valid deploy", "deploy-user", false},
		{"valid dotted", "user.name", false},
		{"empty", "", true},
		{"space", "my user", true},
		{"at sign", "user@host", true},
		{"too long", string(make([]byte, 33)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUsername(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateUsername(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePort(t *testing.T) {
	tests := []struct {
		name    string
		input   int
		wantErr bool
	}{
		{"valid ssh", 22, false},
		{"valid http", 80, false},
		{"valid max", 65535, false},
		{"valid min", 1, false},
		{"zero", 0, true},
		{"negative", -1, true},
		{"too high", 65536, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePort(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePort(%d) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateRuntime(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"docker", "docker", false},
		{"systemd", "systemd", false},
		{"empty", "", true},
		{"other", "podman", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRuntime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRuntime(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid absolute", "/usr/local/bin", false},
		{"valid root", "/", false},
		{"empty", "", true},
		{"relative", "foo/bar", true},
		{"null byte", "/foo\x00bar", true},
		{"too long", "/" + string(make([]byte, 4096)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePath(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePath(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateDeployCommands(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid single", `["git pull"]`, false},
		{"valid multiple", `["git pull","docker compose up -d"]`, false},
		{"empty string", "", true},
		{"empty array", "[]", true},
		{"not json", "git pull", true},
		{"not array", `"git pull"`, true},
		{"not string array", `[1,2,3]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDeployCommands(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDeployCommands(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
