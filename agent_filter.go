//go:build agent

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const filterConfigPath = "/etc/devops-agent/filters.json"

// FilterConfig holds user-extensible filter settings.
type FilterConfig struct {
	AllowedPorts  []int          `json:"allowed_ports"`
	AllowReboot   bool           `json:"allow_reboot"`
	AllowShutdown bool           `json:"allow_shutdown"`
	CustomBlocked []CustomFilter `json:"custom_blocked"`
}

// CustomFilter is a user-defined blocked pattern.
type CustomFilter struct {
	Pattern  string `json:"pattern"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// Hard-coded blocked patterns grouped by category.
var hardBlockedPatterns = []struct {
	pattern  string
	category string
}{
	// Firewall disable/delete
	{"ufw disable", "firewall"},
	{"ufw delete", "firewall"},
	{"ufw reset", "firewall"},
	{"iptables -f", "firewall"},
	{"iptables -x", "firewall"},
	{"iptables -p input accept", "firewall"},
	{"iptables -p forward accept", "firewall"},
	{"nft flush", "firewall"},
	{"nft delete", "firewall"},
	{"firewall-cmd --remove", "firewall"},
	// Firewall service stop/disable
	{"systemctl stop ufw", "firewall-svc"},
	{"systemctl disable ufw", "firewall-svc"},
	{"systemctl stop firewalld", "firewall-svc"},
	{"systemctl disable firewalld", "firewall-svc"},
	{"systemctl stop nftables", "firewall-svc"},
	{"systemctl disable nftables", "firewall-svc"},
	// SSH service stop/disable
	{"systemctl stop ssh", "ssh"},
	{"systemctl disable ssh", "ssh"},
	{"systemctl stop sshd", "ssh"},
	{"systemctl disable sshd", "ssh"},
	// Destructive (non-rm patterns)
	{"mkfs", "destructive"},
	{"dd if=", "destructive"},
	{"shred", "destructive"},
	{"wipefs", "destructive"},
}

// Configurable patterns blocked by default, toggleable via config.
var configurablePatterns = []struct {
	pattern string
	toggle  string // "allow_shutdown" or "allow_reboot"
}{
	{"shutdown", "allow_shutdown"},
	{"poweroff", "allow_shutdown"},
	{"halt", "allow_shutdown"},
	{"init 0", "allow_shutdown"},
	{"reboot", "allow_reboot"},
	{"init 6", "allow_reboot"},
}

// Port-opening detection substrings.
var portPatterns = []string{
	"ufw allow",
	"--dport",
	"firewall-cmd --add-port",
}

// checkCommand returns an error if the command is blocked.
func checkCommand(cmd string) error {
	lower := strings.ToLower(cmd)

	// Destructive rm checks
	if isDestructiveRm(lower) {
		return fmt.Errorf("blocked [destructive]: command targets root filesystem")
	}

	// Hard-coded patterns
	for _, p := range hardBlockedPatterns {
		if strings.Contains(lower, p.pattern) {
			return fmt.Errorf("blocked [%s]: matches prohibited pattern %q", p.category, p.pattern)
		}
	}

	config := loadFilterConfig()

	// Configurable patterns
	for _, p := range configurablePatterns {
		if strings.Contains(lower, p.pattern) {
			switch p.toggle {
			case "allow_shutdown":
				if !config.AllowShutdown {
					return fmt.Errorf("blocked [power]: %q is disabled (set allow_shutdown to enable)", p.pattern)
				}
			case "allow_reboot":
				if !config.AllowReboot {
					return fmt.Errorf("blocked [power]: %q is disabled (set allow_reboot to enable)", p.pattern)
				}
			}
		}
	}

	// Port-opening detection
	for _, pp := range portPatterns {
		if strings.Contains(lower, pp) {
			port := extractPort(lower, pp)
			if port == 0 {
				return fmt.Errorf("blocked [port]: could not parse port from %q", pp)
			}
			if !isPortAllowed(port, config.AllowedPorts) {
				return fmt.Errorf("blocked [port]: port %d is not in the allowed list", port)
			}
		}
	}

	// Custom blocked patterns from config
	for _, custom := range config.CustomBlocked {
		if strings.Contains(lower, strings.ToLower(custom.Pattern)) {
			reason := custom.Reason
			if reason == "" {
				reason = "matches custom filter"
			}
			return fmt.Errorf("blocked [%s]: %s", custom.Category, reason)
		}
	}

	return nil
}

// isDestructiveRm detects rm with both recursive and force flags targeting root.
// Handles all flag forms: -rf, -r -f, -f -r, --recursive --force, --force --recursive,
// and mixed forms like -r --force or --recursive -f.
func isDestructiveRm(lower string) bool {
	// Find all "rm" tokens in the command (handles pipes, semicolons, etc.)
	// Split by common shell separators to isolate individual commands.
	commands := splitShellCommands(lower)
	for _, cmd := range commands {
		cmd = strings.TrimSpace(cmd)
		fields := strings.Fields(cmd)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != "rm" {
			continue
		}

		hasRecursive := false
		hasForce := false
		var targets []string

		for _, f := range fields[1:] {
			if f == "--recursive" {
				hasRecursive = true
				continue
			}
			if f == "--force" {
				hasForce = true
				continue
			}
			if f == "--" {
				// Everything after -- is a target
				continue
			}
			if len(f) > 1 && f[0] == '-' && f[1] != '-' {
				// Short flags like -rf, -r, -f, -fr
				for _, ch := range f[1:] {
					switch ch {
					case 'r', 'R':
						hasRecursive = true
					case 'f':
						hasForce = true
					}
				}
				continue
			}
			// Not a flag — it's a target path
			targets = append(targets, f)
		}

		if !hasRecursive || !hasForce {
			continue
		}

		for _, t := range targets {
			if t == "/" || t == "/*" {
				return true
			}
		}
	}
	return false
}

// splitShellCommands splits a command line by shell separators (;, &&, ||, |)
// to isolate individual commands for analysis.
func splitShellCommands(s string) []string {
	var result []string
	current := strings.Builder{}
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch == ';' || ch == '|' || ch == '&' {
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
			// Skip doubled operators (&&, ||)
			if i+1 < len(s) && s[i+1] == ch {
				i++
			}
			i++
			continue
		}
		current.WriteByte(ch)
		i++
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

func isPathChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') ||
		(ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') ||
		ch == '.' || ch == '_' || ch == '-'
}

// loadFilterConfig reads the JSON config from disk. Returns empty config if missing.
// Logs a warning to stderr if the file exists but contains malformed JSON.
func loadFilterConfig() FilterConfig {
	data, err := os.ReadFile(filterConfigPath)
	if err != nil {
		return FilterConfig{}
	}
	var config FilterConfig
	if err := json.Unmarshal(data, &config); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: filter config %s exists but is malformed JSON: %v (falling back to empty config)\n", filterConfigPath, err)
		return FilterConfig{}
	}
	return config
}

// extractPort scans for a numeric port after the matched pattern substring.
func extractPort(lower, matchedPattern string) int {
	idx := strings.Index(lower, matchedPattern)
	if idx == -1 {
		return 0
	}
	rest := lower[idx+len(matchedPattern):]
	rest = strings.TrimLeft(rest, " =")

	// Extract digits
	var digits strings.Builder
	for _, ch := range rest {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		} else {
			break
		}
	}
	if digits.Len() == 0 {
		return 0
	}
	port, err := strconv.Atoi(digits.String())
	if err != nil {
		return 0
	}
	return port
}

// isPortAllowed checks if port is in the allowed list.
func isPortAllowed(port int, allowed []int) bool {
	for _, p := range allowed {
		if p == port {
			return true
		}
	}
	return false
}

// activeFilters returns a summary of all active filters for the /filters endpoint.
func activeFilters() map[string]any {
	hardBlocked := make([]map[string]string, 0, len(hardBlockedPatterns))
	for _, p := range hardBlockedPatterns {
		hardBlocked = append(hardBlocked, map[string]string{
			"pattern":  p.pattern,
			"category": p.category,
		})
	}

	configurable := make([]map[string]string, 0, len(configurablePatterns))
	for _, p := range configurablePatterns {
		configurable = append(configurable, map[string]string{
			"pattern": p.pattern,
			"toggle":  p.toggle,
		})
	}

	config := loadFilterConfig()

	return map[string]any{
		"hard_blocked":  hardBlocked,
		"configurable":  configurable,
		"port_patterns": portPatterns,
		"config":        config,
		"rm_root_check": true,
	}
}
