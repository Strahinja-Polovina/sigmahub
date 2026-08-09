#!/usr/bin/env bash
# sigmahub agent (sigmad) installer — P1-5 BYO SSH onboarding.
#
# Verifies the release artifacts with cosign (keyless, pinned to the release
# workflow's OIDC identity) before executing anything, installs Docker Engine if
# absent, drops in a root systemd unit, applies the WireGuard mesh config, and
# removes the one-time bootstrap SSH key from authorized_keys (Phase-0 invariant:
# the bootstrap key is never reused).
#
# Usage — this is verbatim the shape the connect-server wizard renders
# (web/src/server/actions/servers.ts, installCommand):
#
#   curl -fsSL https://cp.example.com/install.sh | \
#     SIGMAHUB_ENDPOINT=https://cp.example.com \
#     SIGMAHUB_BOOTSTRAP_TOKEN=sbt_... \
#     SIGMAHUB_VERSION=v0.3.0 \
#     SIGMAHUB_DOWNLOAD_BASE=https://cp.example.com/dl/v0.3.0 sudo -E bash
#
# Every URL an operator pastes is the control plane's — the script itself and,
# through SIGMAHUB_DOWNLOAD_BASE, each release asset it goes on to fetch. The
# control plane proxies those assets with a server-side GitHub credential, which
# is what makes a PRIVATE release repository onboardable at all: unauthenticated
# curls at github.com 404 on all five assets, and the only workaround an operator
# has is to make the repository public.
#
# The proxy moves nothing about the trust model, and that is the first thing to
# check when reading this file. Authenticity comes from the cosign verification
# below — checksums.txt is verified against the release workflow's keyless OIDC
# identity, and the archive and the systemd unit are verified against that
# checksums.txt — never from who served the bytes. A signed artifact handed over
# by a proxy is still the signed artifact, and a control plane that tampered with
# one would fail verification here exactly as GitHub would.
set -euo pipefail

: "${SIGMAHUB_ENDPOINT:?SIGMAHUB_ENDPOINT is required}"
: "${SIGMAHUB_BOOTSTRAP_TOKEN:?SIGMAHUB_BOOTSTRAP_TOKEN is required}"
: "${SIGMAHUB_VERSION:?SIGMAHUB_VERSION is required (e.g. v0.3.0)}"
# SIGMAHUB_REPO is the cosign TRUST ANCHOR: the certificate-identity regexp
# below is built from it, so it decides whose release workflow may have signed
# what this host is about to run as root. It is deliberately not something the
# rendered install command sets — a repository named by the same command that
# names the download server would let one input choose both the bytes and the
# identity they are checked against, which is no check at all.
SIGMAHUB_REPO="${SIGMAHUB_REPO:-Strahinja-Polovina/sigmahub}"
# Where the assets come from, which the control plane overrides so the host
# talks to one host it can already reach. The github.com default is what a
# hand-run install against a PUBLIC release still needs, and it is the fallback
# a typo in the wizard's variable name would silently land on — hence the test
# in web/src/server/actions/install-command.test.ts that pins the two names
# together.
SIGMAHUB_DOWNLOAD_BASE="${SIGMAHUB_DOWNLOAD_BASE:-https://github.com/${SIGMAHUB_REPO}/releases/download/${SIGMAHUB_VERSION}}"
SIGMAHUB_WG_UP="${SIGMAHUB_WG_UP:-1}"

die() { echo "sigmahub-install: $*" >&2; exit 1; }
[ "$(id -u)" = "0" ] || die "must run as root"

# Strip the one-time bootstrap key on EXIT — success OR abort. Even if a later
# step fails, the CP-held bootstrap key must never linger in authorized_keys
# (Phase-0 invariant: never reused). Wired as an EXIT trap below so it always
# runs, not as a happy-path tail that a `set -e` abort would skip.
remove_bootstrap_key() {
  for ak in /root/.ssh/authorized_keys /home/*/.ssh/authorized_keys; do
    [ -f "$ak" ] || continue
    if grep -q 'sigmahub-bootstrap' "$ak" 2>/dev/null; then
      tmp="$(mktemp)"; grep -v 'sigmahub-bootstrap' "$ak" > "$tmp" || true
      cat "$tmp" > "$ak"; rm -f "$tmp"
      echo "removed bootstrap key from $ak"
    fi
  done
}
# Register the EXIT trap NOW, before any die-able step, so an abort in the
# distro/arch gate, an apt install, the cosign download, or the Docker install
# still strips the CP-held one-time bootstrap key (SIGMA-158). The workdir
# cleanup is added to the trap once $work exists.
trap remove_bootstrap_key EXIT

# --- The two vocabularies this script shares with the Go code ----------------
#
# SUPPORTED_DISTROS is the shell copy of the onboarding distro catalog in
# cp/internal/store/server_catalog.go (supportedDistroLabels) — the same list
# the registration compatibility gate enrolls hosts against. SUPPORTED_ARCHES is
# the shell copy of selfupdate.SupportedArches, the architectures .goreleaser
# actually builds sigmad for.
#
# They are copies because a shell script cannot import Go, and because the two
# Go copies live in modules (cp/ and agent/) that cannot import each other. The
# copy is fine; the DRIFT is the defect, and two tests read this file off disk to
# make drift a build failure: agent/packaging/install_script_test.go pins
# SUPPORTED_ARCHES to the agent's list and to the release that publishes it, and
# cp/internal/store/installer_vocabulary_test.go pins SUPPORTED_DISTROS to the
# catalog. Each fails on the edit that causes the bug rather than on the
# onboarding that reveals it — an architecture missing here is a host the release
# builds for and this script turns away, and a distro missing there is a host
# this script happily installs onto and the control plane then parks as
# `incompatible` with the agent already running.
#
# Each list is written ONCE and both the gate and its rejection message are
# rendered from it, so the message cannot go stale on its own the way the
# hand-typed "Ubuntu 22.04/24.04 and Debian 12" sentence it replaced could.
SUPPORTED_DISTROS="ubuntu-22.04 ubuntu-24.04 debian-12"
SUPPORTED_ARCHES="amd64 arm64"

# Membership in a space-separated list, without arrays or grep: this gate runs
# before ensure_tool has installed anything.
in_list() { case " $2 " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# --- Distro gate: only the hardened-onboarding targets are supported ----------
. /etc/os-release 2>/dev/null || die "cannot read /etc/os-release"
distro="${ID:-}-${VERSION_ID:-}"
in_list "${distro}" "${SUPPORTED_DISTROS}" \
  || die "unsupported distro '${distro}'. Onboarding supports: ${SUPPORTED_DISTROS}. Reinstall this host on one of those, or connect a different machine."

# uname -m reports the KERNEL's name for the machine, which is not the release's
# name for it: x86_64 is amd64 and aarch64 is arm64. Normalize first, then check
# membership, so SUPPORTED_ARCHES stays exactly the release's vocabulary and this
# mapping stays what it is — a fact about Linux, not a second architecture list.
arch="$(uname -m)"
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
esac
in_list "${arch}" "${SUPPORTED_ARCHES}" \
  || die "unsupported CPU architecture '$(uname -m)'. sigmad is published for: ${SUPPORTED_ARCHES}."
ver_noV="${SIGMAHUB_VERSION#v}"

need() { command -v "$1" >/dev/null 2>&1; }
ensure_tool() { need "$1" || { echo "installing $2..."; apt-get update -qq && apt-get install -y -qq "$2"; }; }

ensure_tool curl curl
ensure_tool tar tar
# The firewall backend (nftables) and the CIS auditd control are not on the
# stock base images; the host.cis / host.nftables DSD ops need them present.
need nft || { echo "installing nftables..."; apt-get update -qq && apt-get install -y -qq nftables; }
# wg-quick/wg back the WireGuard mesh. Without them mesh.Apply logs "wg-quick
# not found; config rendered but not applied" and the sigma0 interface never
# exists — so intra-fleet traffic, mesh-bound databases and the firewall's
# `iifname "sigma0" accept` rule all silently do nothing (SIGMA-179).
need wg-quick || { echo "installing wireguard-tools..."; apt-get update -qq && apt-get install -y -qq wireguard-tools; }
apt-get install -y -qq auditd >/dev/null 2>&1 || echo "warning: could not install auditd; the CIS auditd control will score as unmet"
# restic executes the P1-11 backup/verify ops (client-side encrypted backups).
ensure_tool restic restic
# --- Third-party tooling ------------------------------------------------------
#
# The three downloads that follow — nixpacks, sigstore's cosign, Docker Engine —
# are other people's software, not this repository's release assets, so they
# keep their own upstream URLs and deliberately do NOT follow
# SIGMAHUB_DOWNLOAD_BASE. Putting them behind the control plane would make it a
# mirror for binaries it neither builds nor signs, which is a separate decision
# from serving our own release and buys nothing for the private-repository case
# this base exists for: these three are public downloads that never 404 on
# account of our repository's visibility. A host firewalled off from them could
# not have installed Docker before this change either.
#
# nixpacks builds a repo that carries no Dockerfile — the wizard's answer to
# "this repository does not say how to build itself". Without it the auto-build
# method the dashboard offers fails on the host with "executable file not
# found", which is the dead end that path exists to remove. Not fatal: a fleet
# that only ever builds Dockerfiles is unaffected, and the agent says plainly
# what is missing rather than reporting a broken repository.
if ! need nixpacks; then
  echo "installing nixpacks..."
  curl -fsSL https://nixpacks.com/install.sh | bash \
    || echo "warning: could not install nixpacks; repositories with no Dockerfile cannot be auto-built on this host"
fi
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
#
# Every sigmahub release asset this script fetches goes through
# SIGMAHUB_DOWNLOAD_BASE: the four here and sigmad.service further down. That is
# the complete set, and a fifth one written with a hard-coded github.com URL is
# the private-repository 404 coming back for a single asset — a host that gets
# most of the way through an install and stops, with the one-time bootstrap key
# already spent, instead of being refused cleanly. install_script_test.go reads
# this file and fails on the commit that adds one.
work="$(mktemp -d)"; trap 'rm -rf "$work"; remove_bootstrap_key' EXIT
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

# The systemd unit is a signed release artifact (goreleaser checksum.extra_files
# lists sigmad.service in the cosign-verified checksums.txt), and it runs as root,
# so verify it against that checksum before installing — otherwise a tampered
# release asset yields a root ExecStart despite the signed-binary machinery
# (SIGMA-155). Fall back to the embedded unit ONLY when the asset is genuinely
# absent (curl fails), never on a checksum mismatch.
if curl -fsSL "${SIGMAHUB_DOWNLOAD_BASE}/sigmad.service" -o "${work}/sigmad.service" 2>/dev/null; then
  unit_line="$(grep " sigmad.service\$" "${work}/checksums.txt" || true)"
  [ -n "${unit_line}" ] || die "sigmad.service is not in the signed checksums.txt — refusing to install an unverifiable unit"
  printf '%s\n' "${unit_line}" | ( cd "${work}" && sha256sum -c - ) \
    || die "sigmad.service checksum mismatch — refusing to install a tampered unit"
  install -m 0644 "${work}/sigmad.service" /etc/systemd/system/sigmad.service
else
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
fi

if [ "${SIGMAHUB_WG_UP}" != "1" ]; then
  sed -i 's| -wg-up||' /etc/systemd/system/sigmad.service
fi

# Persist the firewall across reboots: a oneshot unit loads the ruleset the
# agent writes on its first host.nftables op (ConditionPathExists guards the
# pre-first-apply boot). Without this the DSD apply is version-gated and would
# never re-run `nft -f` after a reboot, leaving the host firewall failing open.
install -m 0644 /dev/stdin /etc/systemd/system/sigmahub-nftables.service <<'NFTUNIT'
[Unit]
Description=Load sigmahub nftables ruleset
Before=network-pre.target
Wants=network-pre.target
ConditionPathExists=/etc/sigmahub/nftables.conf
[Service]
Type=oneshot
ExecStart=/usr/sbin/nft -f /etc/sigmahub/nftables.conf
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
NFTUNIT

systemctl daemon-reload
systemctl enable sigmahub-nftables.service
systemctl enable --now sigmad.service

# The one-time bootstrap key is stripped by the EXIT trap (remove_bootstrap_key),
# so it is removed even if any step above aborted.
echo "sigmahub agent installed and started. It will register and join the mesh; the dashboard will show it as Ready once a peer is reachable."
