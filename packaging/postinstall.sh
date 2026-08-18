#!/bin/sh
set -e

if ! id -u dup >/dev/null 2>&1; then
    useradd --system --user-group \
            --no-create-home --home-dir /nonexistent \
            --shell /usr/sbin/nologin \
            --comment "Docker updater" dup 2>/dev/null || \
    addgroup -S dup 2>/dev/null && adduser -S -G dup -H -s /sbin/nologin dup 2>/dev/null || true
fi

mkdir -p /etc/dup
chown root:dup /etc/dup
chmod 0750 /etc/dup

for secret in bearer.token github.secret; do
    if [ ! -f "/etc/dup/${secret}" ]; then
        if command -v openssl >/dev/null 2>&1; then
            ( umask 077 && openssl rand -hex 32 > "/etc/dup/${secret}" )
        else
            ( umask 077 && head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "/etc/dup/${secret}" )
        fi
    fi
    chown root:dup "/etc/dup/${secret}"
    chmod 0640 "/etc/dup/${secret}"
done

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    if systemctl is-enabled dup-agent >/dev/null 2>&1; then
        systemctl restart dup-agent dup || true
    fi
fi

if [ -f /etc/dup/config.yml ]; then
    cat <<'BANNER'

dup upgraded. Validate and restart:

  sudo dup check
  sudo dup audit
  sudo systemctl restart dup-agent dup
  systemctl status dup-agent dup

BANNER
else
    cat <<'BANNER'

dup installed. Nothing is running yet, because there is no config.

  1. Copy the reference config and edit it for this host:
       sudo cp /etc/dup/config.example.yml /etc/dup/config.yml
       sudo chown root:dup /etc/dup/config.yml && sudo chmod 0640 /etc/dup/config.yml
       sudo $EDITOR /etc/dup/config.yml

  2. Validate it:
       sudo dup check

  3. Prove the dup account cannot rewrite what runs as root:
       sudo dup audit

  4. Start it:
       sudo systemctl enable --now dup-agent dup

  5. Confirm it is up:
       systemctl status dup-agent dup
       dup list
       journalctl -u dup -u dup-agent -f

  bearer token:  sudo cat /etc/dup/bearer.token
  docs:          https://github.com/9technologygroup/docker-updater

BANNER
fi
