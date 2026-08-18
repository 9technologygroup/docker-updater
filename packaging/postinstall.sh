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

if [ ! -f /etc/dup/config.yml ] && [ -f /etc/dup/config.example.yml ]; then
    cp /etc/dup/config.example.yml /etc/dup/config.yml
    chown root:dup /etc/dup/config.yml
    chmod 0640 /etc/dup/config.yml
fi

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

cat <<'BANNER'

dup installed.

  1. edit  /etc/dup/config.yml   so the stacks match this host
  2. run   dup check
  3. run   dup audit
  4. run   systemctl enable --now dup-agent dup

  bearer token:  cat /etc/dup/bearer.token
  docs:          https://github.com/9technologygroup/docker-updater

BANNER
