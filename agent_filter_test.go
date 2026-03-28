package main

import (
	"testing"
)

func TestCheckCommandHardBlocked(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		blocked bool
	}{
		// Destructive
		{"mkfs", "mkfs.ext4 /dev/sda1", true},
		{"dd", "dd if=/dev/zero of=/dev/sda", true},
		{"shred", "shred /dev/sda", true},
		{"wipefs", "wipefs -a /dev/sda", true},
		// Firewall
		{"ufw disable", "ufw disable", true},
		{"ufw delete", "ufw delete allow 80", true},
		{"ufw reset", "ufw reset", true},
		{"iptables flush", "iptables -F", true},
		{"iptables delete", "iptables -X", true},
		{"iptables accept input", "iptables -P INPUT ACCEPT", true},
		{"iptables accept forward", "iptables -P FORWARD ACCEPT", true},
		{"nft flush", "nft flush ruleset", true},
		{"nft delete", "nft delete table inet filter", true},
		{"firewall-cmd remove", "firewall-cmd --remove-port=80/tcp", true},
		// Firewall service
		{"stop ufw", "systemctl stop ufw", true},
		{"disable ufw", "systemctl disable ufw", true},
		{"stop firewalld", "systemctl stop firewalld", true},
		{"disable firewalld", "systemctl disable firewalld", true},
		{"stop nftables", "systemctl stop nftables", true},
		{"disable nftables", "systemctl disable nftables", true},
		// SSH
		{"stop ssh", "systemctl stop ssh", true},
		{"disable ssh", "systemctl disable ssh", true},
		{"stop sshd", "systemctl stop sshd", true},
		{"disable sshd", "systemctl disable sshd", true},
		// Allowed commands
		{"ls", "ls -la", false},
		{"docker ps", "docker ps", false},
		{"git pull", "git pull origin main", false},
		{"systemctl restart nginx", "systemctl restart nginx", false},
		{"echo hello", "echo hello", false},
		{"df -h", "df -h", false},
		{"cat /etc/hosts", "cat /etc/hosts", false},
		{"apt update", "apt update", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCommand(tt.cmd)
			if tt.blocked && err == nil {
				t.Errorf("expected %q to be blocked", tt.cmd)
			}
			if !tt.blocked && err != nil {
				t.Errorf("expected %q to be allowed, got: %v", tt.cmd, err)
			}
		})
	}
}

func TestIsDestructiveRm(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		blocked bool
	}{
		{"rm -rf /", "rm -rf /", true},
		{"rm -rf / with space", "rm -rf / ", true},
		{"rm -rf / with semicolon", "rm -rf /; echo done", true},
		{"rm -rf / with pipe", "rm -rf / | true", true},
		{"rm -rf / with ampersand", "rm -rf / &", true},
		{"rm -rf /*", "rm -rf /*", true},
		{"rm -rf /opt/myapp", "rm -rf /opt/myapp", false},
		{"rm -rf /var/log/old", "rm -rf /var/log/old", false},
		{"rm -rf /tmp/test", "rm -rf /tmp/test", false},
		{"rm -rf /home/user/dir", "rm -rf /home/user/dir", false},
		{"rm -rf ./localdir", "rm -rf ./localdir", false},
		{"rm file.txt", "rm file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCommand(tt.cmd)
			if tt.blocked && err == nil {
				t.Errorf("expected %q to be blocked", tt.cmd)
			}
			if !tt.blocked && err != nil {
				t.Errorf("expected %q to be allowed, got: %v", tt.cmd, err)
			}
		})
	}
}

func TestCheckCommandConfigurable(t *testing.T) {
	// With no config file, reboot and shutdown should be blocked
	tests := []struct {
		name string
		cmd  string
	}{
		{"reboot", "reboot"},
		{"shutdown", "shutdown -h now"},
		{"poweroff", "poweroff"},
		{"halt", "halt"},
		{"init 0", "init 0"},
		{"init 6", "init 6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCommand(tt.cmd)
			if err == nil {
				t.Errorf("expected %q to be blocked by default", tt.cmd)
			}
		})
	}
}

func TestCheckCommandPortDetection(t *testing.T) {
	// With no config (no allowed ports), port-opening commands should be blocked
	tests := []struct {
		name    string
		cmd     string
		blocked bool
	}{
		{"ufw allow 8080", "ufw allow 8080", true},
		{"ufw allow 80/tcp", "ufw allow 80/tcp", true},
		{"dport 3000", "iptables -A INPUT -p tcp --dport 3000 -j ACCEPT", true},
		{"firewall-cmd add-port", "firewall-cmd --add-port=8080/tcp", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCommand(tt.cmd)
			if tt.blocked && err == nil {
				t.Errorf("expected %q to be blocked", tt.cmd)
			}
			if !tt.blocked && err != nil {
				t.Errorf("expected %q to be allowed, got: %v", tt.cmd, err)
			}
		})
	}
}

func TestExtractPort(t *testing.T) {
	tests := []struct {
		name    string
		lower   string
		pattern string
		want    int
	}{
		{"ufw allow 80/tcp", "ufw allow 80/tcp", "ufw allow", 80},
		{"ufw allow 8080", "ufw allow 8080", "ufw allow", 8080},
		{"dport 3000", "iptables -a input -p tcp --dport 3000 -j accept", "--dport", 3000},
		{"firewall-cmd port", "firewall-cmd --add-port=8080/tcp", "firewall-cmd --add-port", 8080},
		{"no digits", "ufw allow http", "ufw allow", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPort(tt.lower, tt.pattern)
			if got != tt.want {
				t.Errorf("extractPort(%q, %q) = %d, want %d", tt.lower, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestCheckCommandCaseInsensitive(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
	}{
		{"UFW DISABLE", "UFW DISABLE"},
		{"Ufw Disable", "Ufw Disable"},
		{"Reboot", "Reboot"},
		{"REBOOT", "REBOOT"},
		{"SYSTEMCTL STOP SSH", "SYSTEMCTL STOP SSH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCommand(tt.cmd)
			if err == nil {
				t.Errorf("expected %q to be blocked (case insensitive)", tt.cmd)
			}
		})
	}
}
