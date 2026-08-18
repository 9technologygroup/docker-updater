#!/bin/sh
set -e

# Sampled before anything below creates it, or every install looks like an upgrade.
FRESH=no
if [ ! -f /etc/dup/config.yml ]; then
    FRESH=yes
fi

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

mkdir -p /var/lib/dup
chown dup:dup /var/lib/dup
chmod 0755 /var/lib/dup

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

# Earlier installs of this project used /usr/local/bin. Leaving those behind
# shadows the packaged binaries on any PATH that prefers /usr/local/bin.
for stale in /usr/local/bin/dup /usr/local/bin/dup-agent; do
    if [ -f "${stale}" ]; then
        rm -f "${stale}"
        echo "removed stale ${stale} from an earlier install"
        echo "if your shell still says '${stale}: No such file or directory', run: hash -r"
    fi
done

# The reference config ships with self_signed: true, so the pair has to exist
# before anyone copies it into place. An existing certificate is never touched.
if [ ! -f /etc/dup/config.yml ] && [ -x /usr/bin/dup ]; then
    /usr/bin/dup cert --defaults >/dev/null 2>&1 || true
    cp /etc/dup/config.example.yml /etc/dup/config.yml
    chown root:dup /etc/dup/config.yml
    chmod 0640 /etc/dup/config.yml
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    if systemctl is-enabled dup-agent >/dev/null 2>&1; then
        systemctl restart dup-agent dup || true
    fi
fi

if [ "${FRESH}" = no ]; then
    cat <<'BANNER'

dup upgraded. Validate and restart, in this order:

  sudo dup check
  sudo dup audit
  sudo systemctl restart dup-agent dup
  systemctl status dup-agent dup

BANNER
else
    cat <<'BANNER'

dup installed. A starter config is at /etc/dup/config.yml with no stacks in it,
and HTTPS is already set up, so there is nothing to edit before starting.

  1. Start it:
       sudo systemctl enable --now dup-agent dup

  2. See every compose project and container on this host:
       sudo dup list

  3. Add the ones you want under 'targets:' in /etc/dup/config.yml. The file
     has a fully commented example showing every field:
       sudo nano /etc/dup/config.yml          # or vim, or whatever you use

  4. Validate, and prove the dup account cannot rewrite what runs as root.
     The audit only means something once real stacks are listed:
       sudo dup check
       sudo dup audit

  5. Apply:
       sudo systemctl restart dup-agent dup

  bearer token:  sudo cat /etc/dup/bearer.token
  docs:          https://github.com/9technologygroup/docker-updater

BANNER
fi
