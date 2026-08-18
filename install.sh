#!/bin/sh
set -eu

GITHUB_REPO="${DUP_GITHUB_REPO:-9technologygroup/docker-updater}"

SVC_USER="dup"
API_BIN="dup"
AGENT_BIN="dup-agent"
BIN_DIR="/usr/local/bin"
CONF_DIR="/etc/dup"
CONF_FILE="${CONF_DIR}/config.yml"
TOKEN_FILE="${CONF_DIR}/bearer.token"
GH_SECRET_FILE="${CONF_DIR}/github.secret"
UNIT_DIR="/etc/systemd/system"

SRC_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo .)"
DOWNLOAD_DIR=""
BUILD_DIR=""

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
    if [ -n "${DOWNLOAD_DIR}" ]; then rm -rf "${DOWNLOAD_DIR}"; fi
    if [ -n "${BUILD_DIR}" ]; then rm -rf "${BUILD_DIR}"; fi
}
trap cleanup EXIT

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
        warn "no checksum listed for $2, skipping verification"
        return 0
    fi
    vc_actual="$(sha256sum "$2" | awk '{print $1}')"
    if [ "${vc_expected}" != "${vc_actual}" ]; then
        warn "checksum MISMATCH for $2"
        warn "  expected ${vc_expected}"
        warn "  actual   ${vc_actual}"
        return 1
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

    if fetch "${dr_base}/checksums.txt" "${DOWNLOAD_DIR}/checksums.txt" 2>/dev/null; then
        if command -v sha256sum >/dev/null 2>&1; then
            ( cd "${DOWNLOAD_DIR}" && verify_checksum checksums.txt "${dr_tarball}" ) || return 1
        else
            warn "sha256sum not available, skipping checksum verification"
        fi
    else
        warn "could not fetch checksums.txt, skipping verification"
    fi

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
        warn "    cd docker-updater && make build-linux && sudo ./install.sh"
        warn ""
        warn "Or point at a specific tag:  DUP_VERSION=v0.1.0 sudo -E sh install.sh"
        exit 1
    fi
fi

gen_secret() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    else
        head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
    fi
}

UPGRADE=no
if [ -f "${BIN_DIR}/${API_BIN}" ]; then UPGRADE=yes; fi

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

log "installing binaries to ${BIN_DIR}"
install -m 0755 -o root -g root "${API_SRC}"   "${BIN_DIR}/${API_BIN}"
install -m 0755 -o root -g root "${AGENT_SRC}" "${BIN_DIR}/${AGENT_BIN}"

log "preparing ${CONF_DIR}"
install -d -m 0750 -o root -g "${SVC_USER}" "${CONF_DIR}"

install_secret() {
    if [ -f "$1" ]; then
        log "keeping existing $2"
    else
        ( umask 077 && gen_secret > "$1" )
        log "generated $2"
    fi
    chown root:"${SVC_USER}" "$1"
    chmod 0640 "$1"
}

install_secret "${TOKEN_FILE}" "bearer token"
install_secret "${GH_SECRET_FILE}" "github webhook secret"

if [ -f "${CONF_FILE}" ]; then
    log "keeping existing ${CONF_FILE}"
else
    [ -f "${SRC_DIR}/deploy/config.example.yml" ] || die "deploy/config.example.yml not found next to this script"
    install -m 0640 -o root -g "${SVC_USER}" "${SRC_DIR}/deploy/config.example.yml" "${CONF_FILE}"
    log "wrote starter config to ${CONF_FILE}"
    warn "edit ${CONF_FILE} so the stacks match this host, then re-run this script"
fi
chown root:"${SVC_USER}" "${CONF_FILE}"
chmod 0640 "${CONF_FILE}"

log "installing systemd units"
for unit in "${AGENT_BIN}.service" "${API_BIN}.service"; do
    [ -f "${SRC_DIR}/deploy/${unit}" ] || die "deploy/${unit} not found next to this script"
    # Units ship with /usr/bin so the deb and rpm are correct; this install uses BIN_DIR.
    sed "s|/usr/bin/dup|${BIN_DIR}/dup|g" "${SRC_DIR}/deploy/${unit}" > "${UNIT_DIR}/${unit}"
    chown root:root "${UNIT_DIR}/${unit}"
    chmod 0644 "${UNIT_DIR}/${unit}"
done
systemctl daemon-reload

if grep -qE '^[[:space:]]*self_signed:[[:space:]]*true' "${CONF_FILE}"; then
    log "ensuring the self-signed certificate exists"
    "${BIN_DIR}/${API_BIN}" cert --config "${CONF_FILE}" || die "could not create the TLS certificate"
fi

if ! "${BIN_DIR}/${API_BIN}" check --config "${CONF_FILE}"; then
    warn "config check failed, nothing was started"
    warn "fix ${CONF_FILE}, then re-run this script"
    exit 1
fi

if ! "${BIN_DIR}/${API_BIN}" audit --config "${CONF_FILE}"; then
    echo >&2
    warn "REFUSING TO START."
    warn "The service account can write files that decide what the root agent runs, so the"
    warn "privilege split would be decoration. Fix the paths listed above, then re-run this script."
    warn "Typically: chown -R root:root <stack dir> && chmod -R go-w <stack dir>"
    exit 1
fi

log "enabling and starting services"
systemctl enable "${AGENT_BIN}" "${API_BIN}" >/dev/null 2>&1 || true
systemctl restart "${AGENT_BIN}"
sleep 1
systemctl restart "${API_BIN}"
sleep 2

for unit in "${AGENT_BIN}" "${API_BIN}"; do
    if ! systemctl is-active --quiet "${unit}"; then
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
log "installed and running (${MODE})"
echo
echo "  dup         ${API_BIN}.service        runs as ${SVC_USER}, no docker access"
echo "  dup-agent   ${AGENT_BIN}.service  runs as root, unix socket only, no network"
echo "  listening   ${SCHEME}://${LISTEN}"
echo "  config      ${CONF_FILE}  (root:${SVC_USER} 0640)"
echo "  logs        journalctl -u ${API_BIN} -u ${AGENT_BIN} -f"
echo
if [ "${UPGRADE}" = "yes" ]; then
    echo "  secrets unchanged; read them with:"
    echo "    cat ${TOKEN_FILE}"
    echo "    cat ${GH_SECRET_FILE}"
else
    echo "  bearer token   $(cat "${TOKEN_FILE}")"
    echo "  github secret  $(cat "${GH_SECRET_FILE}")"
fi
echo
echo "  try it:"
echo "    dup list"
echo "    dup status"
echo "    dup update <stack> --dry-run"
echo
if [ "${SCHEME}" = "https" ]; then
    echo "  dup is serving TLS itself, so a reverse proxy is optional."
    echo "  If you put one in front anyway, add its address to trusted_proxies in the config,"
    echo "  otherwise allow_from will see the proxy rather than the real caller."
    echo
fi
echo "  Nginx Proxy Manager host (optional, preferred when you already run one):"
echo "    Domain          deploy.example.com"
echo "    Scheme          http"
echo "    Forward host    127.0.0.1"
echo "    Forward port    ${PORT}"
echo "    Websockets      off"
echo "    Block exploits  on"
echo "    SSL             request a cert, force SSL, HTTP/2 on"
echo "    Access List     attach one with Basic Auth (satisfy: all)"
echo
