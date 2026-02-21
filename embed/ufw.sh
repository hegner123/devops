#!/usr/bin/env bash
set -euo pipefail

# Idempotent UFW setup for Ubuntu 24 LTS.
# Checks current state before making changes.

if ufw status | grep -q "Status: active"; then
    has_ssh=$(ufw status | grep -c "22/tcp" || true)
    has_http=$(ufw status | grep -c "80/tcp" || true)
    has_https=$(ufw status | grep -c "443/tcp" || true)

    if [ "$has_ssh" -gt 0 ] && [ "$has_http" -gt 0 ] && [ "$has_https" -gt 0 ]; then
        echo "UFW already configured, skipping" >&2
        exit 0
    fi
fi

ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable
echo "UFW configured" >&2
