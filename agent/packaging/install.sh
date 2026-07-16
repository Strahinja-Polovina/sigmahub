#!/usr/bin/env bash
# sigmahub agent (sigmad) installer — P1-5 BYO SSH onboarding.
#
# Verifies the release artifacts with cosign (keyless, pinned to the release
# workflow's OIDC identity) before executing anything, installs Docker Engine if
# absent, drops in a root systemd unit, applies the WireGuard mesh config, and
# removes the one-time bootstrap SSH key from authorized_keys (Phase-0 invariant:
# the bootstrap key is never reused).
#
# Usage (the connect-server wizard renders this):
#   curl -fsSL https://<host>/install.sh | \
#     SIGMAHUB_ENDPOINT=https://cp.example.com \
#     SIGMAHUB_BOOTSTRAP_TOKEN=sbt_... \
#     SIGMAHUB_VERSION=v0.3.0 sudo -E bash
set -euo pipefail

: "${SIGMAHUB_ENDPOINT:?SIGMAHUB_ENDPOINT is required}"
: "${SIGMAHUB_BOOTSTRAP_TOKEN:?SIGMAHUB_BOOTSTRAP_TOKEN is required}"
: "${SIGMAHUB_VERSION:?SIGMAHUB_VERSION is required (e.g. v0.3.0)}"
SIGMAHUB_REPO="${SIGMAHUB_REPO:-Strahinja-Polovina/sigmahub}"
SIGMAHUB_DOWNLOAD_BASE="${SIGMAHUB_DOWNLOAD_BASE:-https://github.com/${SIGMAHUB_REPO}/releases/download/${SIGMAHUB_VERSION}}"
SIGMAHUB_WG_UP="${SIGMAHUB_WG_UP:-1}"

die() { echo "sigmahub-install: $*" >&2; exit 1; }
[ "$(id -u)" = "0" ] || die "must run as root"

# --- Distro gate: only the hardened-onboarding targets are supported ----------
. /etc/os-release 2>/dev/null || die "cannot read /etc/os-release"
distro="${ID:-}-${VERSION_ID:-}"
case "$distro" in
  ubuntu-22.04|ubuntu-24.04|debian-12) : ;;
  *) die "unsupported distro '${distro}'. Onboarding supports Ubuntu 22.04/24.04 and Debian 12 only." ;;
esac

arch="$(uname -m)"; case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported CPU architecture '${arch}'" ;;
esac
ver_noV="${SIGMAHUB_VERSION#v}"

need() { command -v "$1" >/dev/null 2>&1; }
ensure_tool() { need "$1" || { echo "installing $2..."; apt-get update -qq && apt-get install -y -qq "$2"; }; }

ensure_tool curl curl
ensure_tool tar tar
# cosign is required to verify the release before we run it.
if ! need cosign; then
  echo "installing cosign..."
  curl -fsSL "https://github.com/sigstore/cosign/releases/latest/download/cosign-linux-${arch}" -o /usr/local/bin/cosign
  chmod +x /usr/local/bin/cosign
fi

# --- Docker Engine (install if absent) ----------------------------------------
if ! need docker; then
  echo "installing Docker Engine..."
  curl -fsSL https://get.docker.com | sh
  systemctl enable --now docker
fi

# --- Download + verify --------------------------------------------------------
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
archive="sigmad_${ver_noV}_linux_${arch}.tar.gz"
echo "downloading ${archive}..."
curl -fsSL "${SIGMAHUB_DOWNLOAD_BASE}/${archive}"        -o "${work}/${archive}"
curl -fsSL "${SIGMAHUB_DOWNLOAD_BASE}/checksums.txt"     -o "${work}/checksums.txt"
curl -fsSL "${SIGMAHUB_DOWNLOAD_BASE}/checksums.txt.sig" -o "${work}/checksums.txt.sig"
curl -fsSL "${SIGMAHUB_DOWNLOAD_BASE}/checksums.txt.pem" -o "${work}/checksums.txt.pem"

echo "verifying release signature with cosign..."
COSIGN_EXPERIMENTAL=1 cosign verify-blob \
  --certificate "${work}/checksums.txt.pem" \
  --signature "${work}/checksums.txt.sig" \
  --certificate-identity-regexp "^https://github.com/${SIGMAHUB_REPO}/\.github/workflows/release\.yml@" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "${work}/checksums.txt" \
  || die "cosign verification failed — refusing to install"

echo "verifying archive checksum..."
( cd "${work}" && grep " ${archive}\$" checksums.txt | sha256sum -c - ) \
  || die "checksum mismatch — refusing to install"

# --- Install the binary + systemd unit ----------------------------------------
tar -xzf "${work}/${archive}" -C "${work}"
install -m 0755 "${work}/sigmad" /usr/local/bin/sigmad

install -d -m 0700 /etc/sigmad
umask 077
cat > /etc/sigmad/env <<EOF
SIGMAD_ENDPOINT=${SIGMAHUB_ENDPOINT}
SIGMAD_BOOTSTRAP_TOKEN=${SIGMAHUB_BOOTSTRAP_TOKEN}
EOF

curl -fsSL "${SIGMAHUB_DOWNLOAD_BASE}/sigmad.service" -o /etc/systemd/system/sigmad.service 2>/dev/null || \
  install -m 0644 /dev/stdin /etc/systemd/system/sigmad.service <<'UNIT'
[Unit]
Description=sigmahub agent (sigmad)
After=network-online.target docker.service
Wants=network-online.target
[Service]
Type=simple
User=root
EnvironmentFile=/etc/sigmad/env
ExecStart=/usr/local/bin/sigmad -wg-up
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
UNIT

if [ "${SIGMAHUB_WG_UP}" != "1" ]; then
  sed -i 's| -wg-up||' /etc/systemd/system/sigmad.service
fi

systemctl daemon-reload
systemctl enable --now sigmad.service

# --- Remove the one-time bootstrap SSH key ------------------------------------
# The provisioner added the CP's ed25519 public key (comment sigmahub-bootstrap)
# to authorized_keys for a single login. Now that the agent is up and talks
# outbound-only, the key must never be reused.
for ak in /root/.ssh/authorized_keys /home/*/.ssh/authorized_keys; do
  [ -f "$ak" ] || continue
  if grep -q 'sigmahub-bootstrap' "$ak"; then
    tmp="$(mktemp)"; grep -v 'sigmahub-bootstrap' "$ak" > "$tmp" || true
    cat "$tmp" > "$ak"; rm -f "$tmp"
    echo "removed bootstrap key from $ak"
  fi
done

echo "sigmahub agent installed and started. It will register and join the mesh; the dashboard will show it as Ready once a peer is reachable."
