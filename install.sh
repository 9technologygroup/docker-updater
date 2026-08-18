#!/usr/bin/env bash
set -euo pipefail

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
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run this as root (it creates a service account and installs systemd units)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v docker >/dev/null 2>&1 || die "docker is required"
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

resolve_binary() {
  local name="$1"
  for candidate in \
    "${SRC_DIR}/dist/${name}-linux-${ARCH}" \
    "${SRC_DIR}/${name}-linux-${ARCH}" \
    "${SRC_DIR}/${name}"; do
    if [[ -f "${candidate}" ]]; then
      echo "${candidate}"
      return 0
    fi
  done
  return 1
}

build_from_source() {
  command -v go >/dev/null 2>&1 || return 1
  [[ -f "${SRC_DIR}/go.mod" ]] || return 1
  log "no prebuilt binaries found, building from source" >&2
  BUILD_DIR="$(mktemp -d)"
  chmod 0700 "${BUILD_DIR}"
  ( cd "${SRC_DIR}" && \
    CGO_ENABLED=0 go build -trimpath -o "${BUILD_DIR}/${API_BIN}" ./cmd/dup && \
    CGO_ENABLED=0 go build -trimpath -o "${BUILD_DIR}/${AGENT_BIN}" ./cmd/dup-agent ) >&2 || return 1
  return 0
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
  local url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
  local body
  body="$(mktemp)"
  fetch "${url}" "${body}" >/dev/null 2>&1 || { rm -f "${body}"; return 1; }
  sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "${body}" | head -1
  rm -f "${body}"
}

download_release() {
  command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || return 1

  local version="${DUP_VERSION:-}"
  if [[ -z "${version}" ]]; then
    version="$(latest_version)" || return 1
  fi
  [[ -n "${version}" ]] || return 1

  local tarball="dup-linux-${ARCH}.tar.gz"
  local base="https://github.com/${GITHUB_REPO}/releases/download/${version}"

  DOWNLOAD_DIR="$(mktemp -d)"
  chmod 0700 "${DOWNLOAD_DIR}"

  log "downloading dup ${version} (${ARCH})" >&2
  fetch "${base}/${tarball}" "${DOWNLOAD_DIR}/${tarball}" || return 1

  if fetch "${base}/checksums.txt" "${DOWNLOAD_DIR}/checksums.txt" 2>/dev/null; then
    if command -v sha256sum >/dev/null 2>&1; then
      ( cd "${DOWNLOAD_DIR}" && sha256sum -c checksums.txt --ignore-missing >/dev/null 2>&1 ) \
        || { warn "checksum verification FAILED for ${tarball}"; return 1; }
      log "checksum verified" >&2
    else
      warn "sha256sum not available, skipping checksum verification"
    fi
  else
    warn "could not fetch checksums.txt, skipping verification"
  fi

  tar -xzf "${DOWNLOAD_DIR}/${tarball}" -C "${DOWNLOAD_DIR}" || return 1
  SRC_DIR="${DOWNLOAD_DIR}"
  return 0
}

API_SRC="$(resolve_binary "${API_BIN}" || true)"
AGENT_SRC="$(resolve_binary "${AGENT_BIN}" || true)"

if [[ -z "${API_SRC}" || -z "${AGENT_SRC}" ]]; then
  if download_release; then
    API_SRC="$(resolve_binary "${API_BIN}" || true)"
    AGENT_SRC="$(resolve_binary "${AGENT_BIN}" || true)"
  fi
fi

if [[ -z "${API_SRC}" || -z "${AGENT_SRC}" ]]; then
  if build_from_source; then
    API_SRC="${BUILD_DIR}/${API_BIN}"
    AGENT_SRC="${BUILD_DIR}/${AGENT_BIN}"
  else
    die "could not download or build the binaries; check network access, or run 'make build-linux' and re-run this script from the repo"
  fi
fi

cleanup() { [[ -n "${DOWNLOAD_DIR:-}" ]] && rm -rf "${DOWNLOAD_DIR}"; }
trap cleanup EXIT

gen_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

UPGRADE=no
[[ -f "${BIN_DIR}/${API_BIN}" ]] && UPGRADE=yes

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
  local path="$1" label="$2"
  if [[ -f "${path}" ]]; then
    log "keeping existing ${label}"
  else
    ( umask 077 && gen_secret > "${path}" )
    log "generated ${label}"
  fi
  chown root:"${SVC_USER}" "${path}"
  chmod 0640 "${path}"
}

install_secret "${TOKEN_FILE}" "bearer token"
install_secret "${GH_SECRET_FILE}" "github webhook secret"

if [[ -f "${CONF_FILE}" ]]; then
  log "keeping existing ${CONF_FILE}"
else
  [[ -f "${SRC_DIR}/deploy/config.example.yml" ]] || die "deploy/config.example.yml not found next to this script"
  install -m 0640 -o root -g "${SVC_USER}" "${SRC_DIR}/deploy/config.example.yml" "${CONF_FILE}"
  log "wrote starter config to ${CONF_FILE}"
  warn "edit ${CONF_FILE} so the stacks match this host, then re-run this script"
fi
chown root:"${SVC_USER}" "${CONF_FILE}"
chmod 0640 "${CONF_FILE}"

log "installing systemd units"
for unit in "${AGENT_BIN}.service" "${API_BIN}.service"; do
  [[ -f "${SRC_DIR}/deploy/${unit}" ]] || die "deploy/${unit} not found next to this script"
  # Units ship with /usr/bin so the deb/rpm are correct; this install uses BIN_DIR.
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
[[ -n "${LISTEN}" ]] || LISTEN="127.0.0.1:7788"
PORT="${LISTEN##*:}"

MODE="fresh install"
[[ "${UPGRADE}" == "yes" ]] && MODE="upgrade"

echo
log "installed and running (${MODE})"
echo
echo "  dup         ${API_BIN}.service        runs as ${SVC_USER}, no docker access"
echo "  dup-agent   ${AGENT_BIN}.service  runs as root, unix socket only, no network"
echo "  listening   ${LISTEN}"
echo "  config      ${CONF_FILE}  (root:${SVC_USER} 0640)"
echo "  logs        journalctl -u ${API_BIN} -u ${AGENT_BIN} -f"
echo
if [[ "${UPGRADE}" == "yes" ]]; then
  echo "  secrets unchanged; read them with:"
  echo "    cat ${TOKEN_FILE}"
  echo "    cat ${GH_SECRET_FILE}"
else
  echo "  bearer token   $(cat "${TOKEN_FILE}")"
  echo "  github secret  $(cat "${GH_SECRET_FILE}")"
fi
echo
SCHEME=http
grep -qE '^[[:space:]]*(self_signed:[[:space:]]*true|cert_file:)' "${CONF_FILE}" && SCHEME=https
echo "  scheme      ${SCHEME}"
echo
echo "  try it:"
echo "    dup list"
echo "    dup status"
echo "    dup update <stack> --dry-run"
echo
if [[ "${SCHEME}" == "https" ]]; then
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
