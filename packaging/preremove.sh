#!/bin/sh
set -e

# dpkg passes "upgrade" and rpm passes "1" when the package is being replaced.
# Disabling then leaves the units down after the upgrade, with nothing to re-enable them.
case "${1:-}" in
upgrade | 1)
    exit 0
    ;;
esac

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop dup dup-agent 2>/dev/null || true
    systemctl disable dup dup-agent 2>/dev/null || true
fi
