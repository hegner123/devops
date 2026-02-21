#!/usr/bin/env bash
set -euo pipefail

# Activator script: applies all embedded server configurations.
# Does NOT write files -- bootstrap writes them first, then runs this.
# Each step is idempotent.

# 1. Reload sshd for hardening config
if systemctl is-active sshd >/dev/null 2>&1; then
    systemctl reload sshd
    echo "sshd reloaded" >&2
else
    echo "sshd not active, skipping reload" >&2
fi

# 2. Apply sysctl tuning
sysctl --system >/dev/null 2>&1
echo "sysctl applied" >&2

# 3. Apply UFW rules
if [ -f /tmp/devops-ufw.sh ]; then
    bash /tmp/devops-ufw.sh
    rm -f /tmp/devops-ufw.sh
fi

# 4. Reload Docker if installed
if command -v docker >/dev/null 2>&1; then
    systemctl reload docker || true
    echo "docker reloaded" >&2
else
    echo "docker not installed, skipping" >&2
fi

# 5. Install unattended-upgrades if needed
if ! dpkg -s unattended-upgrades >/dev/null 2>&1; then
    apt-get install -y unattended-upgrades >/dev/null 2>&1
    echo "unattended-upgrades installed" >&2
else
    echo "unattended-upgrades already installed" >&2
fi

# 6. Reload systemd for service unit
systemctl daemon-reload
echo "systemd reloaded" >&2
