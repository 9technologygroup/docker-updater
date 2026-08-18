#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop dup dup-agent 2>/dev/null || true
    systemctl disable dup dup-agent 2>/dev/null || true
fi
