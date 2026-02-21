package main

import (
	"encoding/json"
	"fmt"
)

func validateServiceName(s string) error {
	if s == "" {
		return fmt.Errorf("service name is required")
	}
	if len(s) > 256 {
		return fmt.Errorf("service name exceeds 256 characters")
	}
	for _, r := range s {
		if !isServiceNameRune(r) {
			return fmt.Errorf("service name contains invalid character: %c", r)
		}
	}
	return nil
}

func isServiceNameRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '.', '_', '@', ':', '-':
		return true
	}
	return false
}

func validateHostname(s string) error {
	if s == "" {
		return fmt.Errorf("hostname is required")
	}
	if len(s) > 253 {
		return fmt.Errorf("hostname exceeds 253 characters")
	}
	for _, r := range s {
		if !isHostnameRune(r) {
			return fmt.Errorf("hostname contains invalid character: %c", r)
		}
	}
	return nil
}

func isHostnameRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '.', '-':
		return true
	}
	return false
}

func validateUsername(s string) error {
	if s == "" {
		return fmt.Errorf("username is required")
	}
	if len(s) > 32 {
		return fmt.Errorf("username exceeds 32 characters")
	}
	for _, r := range s {
		if !isUsernameRune(r) {
			return fmt.Errorf("username contains invalid character: %c", r)
		}
	}
	return nil
}

func isUsernameRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '.', '_', '-':
		return true
	}
	return false
}

func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func validateRuntime(s string) error {
	if s != "docker" && s != "systemd" {
		return fmt.Errorf("runtime must be 'docker' or 'systemd'")
	}
	return nil
}

func validatePath(s string) error {
	if s == "" {
		return fmt.Errorf("path is required")
	}
	if len(s) > 4096 {
		return fmt.Errorf("path exceeds 4096 characters")
	}
	if s[0] != '/' {
		return fmt.Errorf("path must be absolute")
	}
	for _, r := range s {
		if r == 0 {
			return fmt.Errorf("path contains null byte")
		}
	}
	return nil
}

func validateDeployCommands(s string) error {
	if s == "" {
		return fmt.Errorf("deploy commands is required")
	}
	var cmds []string
	if err := json.Unmarshal([]byte(s), &cmds); err != nil {
		return fmt.Errorf("deploy commands must be a valid JSON array of strings: %w", err)
	}
	if len(cmds) == 0 {
		return fmt.Errorf("deploy commands array must not be empty")
	}
	return nil
}
