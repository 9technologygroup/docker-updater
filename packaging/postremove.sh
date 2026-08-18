#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload 2>/dev/null || true
fi

echo "dup removed. /etc/dup was left in place; delete it yourself if you no longer need the config and secrets."
