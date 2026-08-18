#!/bin/sh
set -eu

GITHUB_REPO="${DUP_GITHUB_REPO:-9technologygroup/docker-updater}"

SVC_USER="dup"
API_BIN="dup"
AGENT_BIN="dup-agent"
BIN_DIR="/usr/bin"
LEGACY_BIN_DIR="/usr/local/bin"
CONF_DIR="/etc/dup"
CONF_FILE="${CONF_DIR}/config.yml"
TOKEN_FILE="${CONF_DIR}/bearer.token"
GH_SECRET_FILE="${CONF_DIR}/github.secret"
UNIT_DIR="/etc/systemd/system"
PKG_UNIT_DIR="/lib/systemd/system"
STATE_DIR="/var/lib/dup"

SRC_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo .)"
DOWNLOAD_DIR=""
BUILD_DIR=""
SECRETS_GENERATED=no

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
    if [ -n "${DOWNLOAD_DIR}" ]; then rm -rf "${DOWNLOAD_DIR}"; fi
    if [ -n "${BUILD_DIR}" ]; then rm -rf "${BUILD_DIR}"; fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- preflight

[ "$(id -u)" -eq 0 ] || die "run this as root (it creates a service account and installs systemd units)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v docker >/dev/null 2>&1 || die "docker is required"
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"

case "$(uname -m)" in
    x86_64|amd64)          ARCH=amd64 ;;
    aarch64|arm64)         ARCH=arm64 ;;
    armv7l|armv7|armhf|arm) ARCH=armv7 ;;
    i386|i486|i586|i686|x86) ARCH=386 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac

# ------------------------------------------------------------ obtain binaries

resolve_binary() {
    for rb_candidate in \
        "${SRC_DIR}/dist/$1-linux-${ARCH}" \
        "${SRC_DIR}/$1-linux-${ARCH}" \
        "${SRC_DIR}/$1"; do
        if [ -f "${rb_candidate}" ]; then
            echo "${rb_candidate}"
            return 0
        fi
    done
    return 1
}

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$2" "$1"
    else
        return 1
    fi
}

latest_version() {
    lv_body="$(mktemp)"
    if ! fetch "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" "${lv_body}" >/dev/null 2>&1; then
        rm -f "${lv_body}"
        return 1
    fi
    sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "${lv_body}" | head -1
    rm -f "${lv_body}"
}

verify_checksum() {
    # busybox sha256sum has no --ignore-missing, so compare the one line we care about.
    vc_expected="$(grep " \{1,\}\*\{0,1\}$2\$" "$1" 2>/dev/null | awk '{print $1}' | head -1)"
    if [ -z "${vc_expected}" ]; then
        die "no checksum listed for $2 in checksums.txt; refusing to install an unverified binary"
    fi
    vc_actual="$(sha256sum "$2" | awk '{print $1}')"
    if [ "${vc_expected}" != "${vc_actual}" ]; then
        warn "checksum MISMATCH for $2"
        warn "  expected ${vc_expected}"
        warn "  actual   ${vc_actual}"
        die "refusing to install a download that does not match its checksum"
    fi
    log "checksum verified" >&2
}

download_release() {
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || return 1

    dr_version="${DUP_VERSION:-}"
    if [ -z "${dr_version}" ]; then
        dr_version="$(latest_version)" || true
    fi
    if [ -z "${dr_version}" ]; then
        warn "no published release found for ${GITHUB_REPO}"
        return 1
    fi

    dr_tarball="dup-linux-${ARCH}.tar.gz"
    dr_base="https://github.com/${GITHUB_REPO}/releases/download/${dr_version}"

    DOWNLOAD_DIR="$(mktemp -d)"
    chmod 0700 "${DOWNLOAD_DIR}"

    log "downloading dup ${dr_version} (${ARCH})" >&2
    fetch "${dr_base}/${dr_tarball}" "${DOWNLOAD_DIR}/${dr_tarball}" || return 1

    # A download that cannot be verified is not installed. This script is piped
    # straight into a root shell; a silent "skipping verification" is not a
    # trade-off worth offering.
    command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required to verify the download"
    fetch "${dr_base}/checksums.txt" "${DOWNLOAD_DIR}/checksums.txt" 2>/dev/null \
        || die "could not fetch checksums.txt from ${dr_base}"
    ( cd "${DOWNLOAD_DIR}" && verify_checksum checksums.txt "${dr_tarball}" ) || exit 1

    tar -xzf "${DOWNLOAD_DIR}/${dr_tarball}" -C "${DOWNLOAD_DIR}" || return 1
    SRC_DIR="${DOWNLOAD_DIR}"
    return 0
}

build_from_source() {
    command -v go >/dev/null 2>&1 || return 1
    [ -f "${SRC_DIR}/go.mod" ] || return 1
    log "no prebuilt binaries found, building from source" >&2
    BUILD_DIR="$(mktemp -d)"
    chmod 0700 "${BUILD_DIR}"
    ( cd "${SRC_DIR}" && \
      CGO_ENABLED=0 go build -trimpath -o "${BUILD_DIR}/${API_BIN}" ./cmd/dup && \
      CGO_ENABLED=0 go build -trimpath -o "${BUILD_DIR}/${AGENT_BIN}" ./cmd/dup-agent ) >&2 || return 1
    return 0
}

API_SRC="$(resolve_binary "${API_BIN}" || true)"
AGENT_SRC="$(resolve_binary "${AGENT_BIN}" || true)"

if [ -z "${API_SRC}" ] || [ -z "${AGENT_SRC}" ]; then
    if download_release; then
        API_SRC="$(resolve_binary "${API_BIN}" || true)"
        AGENT_SRC="$(resolve_binary "${AGENT_BIN}" || true)"
    fi
fi

if [ -z "${API_SRC}" ] || [ -z "${AGENT_SRC}" ]; then
    if build_from_source; then
        API_SRC="${BUILD_DIR}/${API_BIN}"
        AGENT_SRC="${BUILD_DIR}/${AGENT_BIN}"
    else
        echo >&2
        warn "Could not obtain the dup binaries."
        warn ""
        warn "If ${GITHUB_REPO} has no published release yet, there is nothing to download."
        warn "Cut one from a clone of the repo:"
        warn "    git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0"
        warn ""
        warn "Or install from a clone without waiting for a release:"
        warn "    git clone https://github.com/${GITHUB_REPO}"
        warn "    cd docker-updater && make build && sudo ./install.sh"
        warn ""
        warn "Or point at a specific tag:  DUP_VERSION=v0.1.0 sudo -E sh install.sh"
        exit 1
    fi
fi

# Everything this script installs must be present before anything is written.
# Discovering a missing unit file after the binaries are in place leaves a host
# with a new dup and no way to run it.
for required in deploy/config.example.yml "deploy/${API_BIN}.service" "deploy/${AGENT_BIN}.service"; do
    [ -f "${SRC_DIR}/${required}" ] || die "${required} not found next to this script; the download or checkout is incomplete"
done

chmod 0755 "${API_SRC}" "${AGENT_SRC}" 2>/dev/null || true

# ------------------------------------------------------------- install state

UPGRADE=no
if [ -x "${BIN_DIR}/${API_BIN}" ] || [ -x "${LEGACY_BIN_DIR}/${API_BIN}" ]; then UPGRADE=yes; fi

if [ -f "${PKG_UNIT_DIR}/${API_BIN}.service" ]; then
    warn "dup is also installed from a package (${PKG_UNIT_DIR}/${API_BIN}.service exists)."
    warn "This installer writes units to ${UNIT_DIR}, which take precedence over the"
    warn "package's. Both now run ${BIN_DIR}/dup, so upgrades either way work, but"
    warn "consider picking one route: 'apt install --only-upgrade dup' or this script."
fi

if id -u "${SVC_USER}" >/dev/null 2>&1; then
    log "service account ${SVC_USER} already exists"
else
    log "creating system account ${SVC_USER}"
    useradd --system --user-group \
            --no-create-home --home-dir /nonexistent \
            --shell /usr/sbin/nologin \
            --comment "Docker updater" \
            "${SVC_USER}"
fi

if id -nG "${SVC_USER}" | tr ' ' '\n' | grep -qx docker; then
    warn "${SVC_USER} is in the 'docker' group. That is root-equivalent and defeats the whole point"
    warn "of the privileged/unprivileged split. Remove it with:"
    warn "    gpasswd -d ${SVC_USER} docker && systemctl restart ${API_BIN}"
fi

log "preparing ${CONF_DIR}"
install -d -m 0750 -o root -g "${SVC_USER}" "${CONF_DIR}"
install -d -m 0755 -o "${SVC_USER}" -g "${SVC_USER}" "${STATE_DIR}"

install_secret() {
    if [ -f "$1" ]; then
        log "keeping existing $2"
    else
        ( umask 077 && gen_secret > "$1" )
        log "generated $2"
        SECRETS_GENERATED=yes
    fi
    chown root:"${SVC_USER}" "$1"
    chmod 0640 "$1"
}

gen_secret() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    else
        head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
    fi
}

install_secret "${TOKEN_FILE}" "bearer token"
install_secret "${GH_SECRET_FILE}" "github webhook secret"

EXAMPLE_FILE="${CONF_DIR}/config.example.yml"
install -m 0640 -o root -g "${SVC_USER}" "${SRC_DIR}/deploy/config.example.yml" "${EXAMPLE_FILE}"
log "installed reference config to ${EXAMPLE_FILE}"

# ------------------------------------------------------------- tls by default

# The reference config ships with self_signed: true, so the pair has to exist
# before anyone copies it into place. An existing certificate is never touched.
if [ ! -f "${CONF_FILE}" ]; then
    "${API_SRC}" cert --defaults >/dev/null || die "could not create the self-signed certificate"
    log "self-signed certificate ready at ${CONF_DIR}/self-signed.crt"
fi

# ------------------------------------------------- validate before installing

# The new binary validates the existing config while the old one is still in
# place. A config that does not pass must not leave a replaced binary on disk,
# because the next restart or reboot would pick it up.
if [ -f "${CONF_FILE}" ]; then
    log "validating ${CONF_FILE} with the new binary before replacing anything"

    if grep -qE '^[[:space:]]*self_signed:[[:space:]]*true' "${CONF_FILE}"; then
        "${API_SRC}" cert --config "${CONF_FILE}" || exit 1
    fi

    if ! "${API_SRC}" check --config "${CONF_FILE}"; then
        echo >&2
        warn "The config did not validate, so nothing has been changed."
        warn "The installed binaries and the running services are untouched."
        warn "Fix the problems above and re-run this installer."
        exit 1
    fi

    if ! "${API_SRC}" audit --config "${CONF_FILE}"; then
        echo >&2
        warn "REFUSING TO INSTALL."
        warn "The ${SVC_USER} account can write files that decide what the root agent runs,"
        warn "so the privilege split would be decoration. Fix the paths listed above."
        warn "Typically:  sudo chown -R root:root <stack dir> && sudo chmod -R go-w <stack dir>"
        exit 1
    fi
fi

# --------------------------------------------------------------- install

log "installing binaries to ${BIN_DIR}"
install -m 0755 -o root -g root "${API_SRC}"   "${BIN_DIR}/${API_BIN}"
install -m 0755 -o root -g root "${AGENT_SRC}" "${BIN_DIR}/${AGENT_BIN}"

# Earlier versions of this script installed to /usr/local/bin. Leaving those in
# place shadows the real binaries on any PATH that prefers /usr/local/bin, so
# 'dup version' would report one build while the service ran another.
REMOVED_STALE=no
for stale in "${LEGACY_BIN_DIR}/${API_BIN}" "${LEGACY_BIN_DIR}/${AGENT_BIN}"; do
    if [ -f "${stale}" ]; then
        rm -f "${stale}"
        log "removed stale ${stale} from an earlier install"
        REMOVED_STALE=yes
    fi
done

if [ "${REMOVED_STALE}" = "yes" ]; then
    warn "Your current shell may still have the old path cached. If the next dup"
    warn "command says '${LEGACY_BIN_DIR}/dup: No such file or directory', run:"
    warn "    hash -r"
fi

log "installing systemd units"
for unit in "${AGENT_BIN}.service" "${API_BIN}.service"; do
    install -m 0644 -o root -g root "${SRC_DIR}/deploy/${unit}" "${UNIT_DIR}/${unit}"
done
systemctl daemon-reload

DUP_VER="$(DUP_NO_UPDATE_CHECK=1 "${BIN_DIR}/${API_BIN}" version 2>/dev/null || echo dup)"

# ------------------------------------------------------- fresh install, no config

if [ ! -f "${CONF_FILE}" ]; then
    echo
    log "${DUP_VER} installed. Nothing is running yet, because there is no config."
    echo
    echo "  binaries      ${BIN_DIR}/${API_BIN}, ${BIN_DIR}/${AGENT_BIN}"
    echo "  units         ${API_BIN}.service, ${AGENT_BIN}.service  (installed, not enabled)"
    echo "  config dir    ${CONF_DIR}  (root:${SVC_USER} 0750)"
    echo "  reference     ${EXAMPLE_FILE}"
    echo "  tls cert      ${CONF_DIR}/self-signed.crt  (generated, TLS is on by default)"
    echo "  bearer token  ${TOKEN_FILE}"
    echo "  github secret ${GH_SECRET_FILE}"
    echo
    echo "Next steps, in this order"
    echo
    echo "  1. Copy the reference config and edit it so the stacks match this host."
    echo "     Every stack needs a name and the directory its compose file lives in."
    echo
    echo "       sudo cp ${EXAMPLE_FILE} ${CONF_FILE}"
    echo "       sudo chown root:${SVC_USER} ${CONF_FILE} && sudo chmod 0640 ${CONF_FILE}"
    echo "       sudo nano ${CONF_FILE}          # or vim, or whatever you use"
    echo
    echo "     TLS is already on in the reference config, and the certificate above"
    echo "     is already generated, so there is nothing to do for HTTPS. To turn it"
    echo "     off, set 'self_signed: false' under tls."
    echo
    echo "  2. Check the config parses and matches this host."
    echo
    echo "       sudo ${API_BIN} check"
    echo
    echo "  3. Prove the ${SVC_USER} account cannot rewrite anything that runs as root."
    echo "     This must pass, or the privilege split buys you nothing."
    echo
    echo "       sudo ${API_BIN} audit"
    echo
    echo "  4. Start it."
    echo
    echo "       sudo systemctl enable --now ${AGENT_BIN} ${API_BIN}"
    echo
    echo "  5. Confirm it is up."
    echo
    echo "       systemctl status ${AGENT_BIN} ${API_BIN}"
    echo "       ${API_BIN} list"
    echo
    echo "  bearer token   $(cat "${TOKEN_FILE}")"
    echo "  github secret  $(cat "${GH_SECRET_FILE}")"
    echo
    echo "  Re-running this installer later upgrades the binaries and leaves your"
    echo "  config and secrets untouched."
    echo
    exit 0
fi

# ----------------------------------------------------------------- start

wait_active() {
    wa_unit="$1"
    wa_deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "${wa_deadline}" ]; do
        if systemctl is-active --quiet "${wa_unit}"; then
            return 0
        fi
        if systemctl is-failed --quiet "${wa_unit}"; then
            return 1
        fi
        sleep 1
    done
    systemctl is-active --quiet "${wa_unit}"
}

log "enabling and starting services"
systemctl enable "${AGENT_BIN}" "${API_BIN}" >/dev/null || die "could not enable the units"

# The agent owns the socket the API talks to, so it comes up first. Its
# ExecStartPre runs check and audit over every stack directory, which is not
# instant on a host with many stacks, so wait for readiness rather than guess.
for unit in "${AGENT_BIN}" "${API_BIN}"; do
    systemctl restart "${unit}"
    if ! wait_active "${unit}"; then
        warn "${unit} failed to start, recent logs:"
        journalctl -u "${unit}" -n 30 --no-pager >&2 || true
        exit 1
    fi
done

LISTEN="$(awk -F'"' '/^listen:/ {print $2}' "${CONF_FILE}" 2>/dev/null || true)"
if [ -z "${LISTEN}" ]; then LISTEN="127.0.0.1:7788"; fi
PORT="${LISTEN##*:}"

MODE="fresh install"
if [ "${UPGRADE}" = "yes" ]; then MODE="upgrade"; fi

SCHEME=http
if grep -qE '^[[:space:]]*(self_signed:[[:space:]]*true|cert_file:[[:space:]]*[^"[:space:]])' "${CONF_FILE}"; then
    SCHEME=https
fi

echo
log "${DUP_VER} running (${MODE})"
echo
echo "  dup         runs as ${SVC_USER}, no docker access"
echo "  dup-agent   runs as root, unix socket only, no network listener"
echo "  listening   ${SCHEME}://${LISTEN}"
echo "  config      ${CONF_FILE}  (root:${SVC_USER} 0640)"
echo
echo "Check on it"
echo
echo "  systemctl status ${AGENT_BIN} ${API_BIN}"
echo "  journalctl -u ${API_BIN} -u ${AGENT_BIN} -f"
echo "  ${API_BIN} list                 stacks, update policy, and what is not covered"
echo "  ${API_BIN} status               recent update jobs"
echo "  ${API_BIN} version --full       version, latest release, licence, source"
echo
echo "Try an update without changing anything"
echo
echo "  sudo ${API_BIN} update <stack> --dry-run"
echo
if [ "${SECRETS_GENERATED}" = "yes" ]; then
    echo "  bearer token   $(cat "${TOKEN_FILE}")"
    echo "  github secret  $(cat "${GH_SECRET_FILE}")"
else
    echo "  Secrets were left alone. Read them with:"
    echo "    sudo cat ${TOKEN_FILE}"
    echo "    sudo cat ${GH_SECRET_FILE}"
fi
echo
if [ "${SCHEME}" = "https" ]; then
    echo "  dup is serving TLS itself, so a reverse proxy is optional."
    echo "  If you put one in front anyway, add its address to trusted_proxies,"
    echo "  otherwise allow_from will see the proxy rather than the real caller."
    echo
fi
echo "  Nginx Proxy Manager host (optional, preferred when you already run one):"
echo "    Domain          deploy.example.com"
echo "    Scheme          ${SCHEME}"
echo "    Forward host    127.0.0.1"
echo "    Forward port    ${PORT}"
echo "    Websockets      off"
echo "    Block exploits  on"
echo "    SSL             request a cert, force SSL, HTTP/2 on"
echo "    Access List     attach one with Basic Auth (satisfy: all)"
if [ "${SCHEME}" = "https" ]; then
    echo
    echo "    dup serves a self-signed certificate, so the proxy will not trust it by"
    echo "    name. In NPM, tick 'Ignore Invalid SSL' on the Advanced tab, or import"
    echo "    ${CONF_DIR}/self-signed.crt. The hop is over loopback either way."
fi
echo
