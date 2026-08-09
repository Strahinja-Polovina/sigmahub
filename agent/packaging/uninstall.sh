#!/usr/bin/env bash
# sigmahub agent (sigmad) manual uninstall — the force-disconnect companion.
#
# The dashboard's graceful disconnect asks the agent to do all of this itself
# (the agent.uninstall op, SIGMA-204). This script is what to run when it could
# not: the host was already unreachable, or the teardown timed out. It removes
# exactly what the installer added, in the same order the agent uses.
#
# It is SAFE to run twice, and safe to run on a host that is still connected —
# it will simply disconnect it.
#
# Named volumes are KEPT unless you ask for them: they hold database data
# directories and uploaded files, which are yours and outlive this machine.
#
# Usage — the dashboard's Force disconnect dialog renders this script in full,
# to be copied onto the host and run there:
#
#   sudo bash uninstall.sh
#   sudo bash uninstall.sh --purge-volumes
#
# It used to say `curl -fsSL https://<host>/uninstall.sh | sudo bash`. No host
# has ever served that path, so the one instruction on the page was a dead end —
# followed by someone whose graceful teardown had just failed, on a machine they
# were trying to get back.
set -uo pipefail

PURGE_VOLUMES=0
for arg in "$@"; do
  case "$arg" in
    --purge-volumes) PURGE_VOLUMES=1 ;;
    *) echo "sigmahub-uninstall: unknown argument '$arg'" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = "0" ] || { echo "sigmahub-uninstall: must run as root" >&2; exit 1; }

DATA_DIR="${SIGMAD_DATA_DIR:-/root/.sigmad}"

# 0. k3s, if this host ever joined a cluster. Cluster workloads run under k3s's
#    own containerd, not Docker, so the sweep below cannot see them — a host
#    cleaned without this keeps running k3s and every pod the scheduler placed
#    on it. A host that never joined a cluster has no script here.
for k3s_uninstall in /usr/local/bin/k3s-uninstall.sh /usr/local/bin/k3s-agent-uninstall.sh; do
  if [ -x "${k3s_uninstall}" ]; then
    echo "removing k3s (${k3s_uninstall})"
    "${k3s_uninstall}" || echo "warning: ${k3s_uninstall} failed; k3s may still be installed"
  fi
done

# 1. Containers and networks. Everything sigmahub created carries the
#    sigmahub.managed label, so nothing of yours is matched.
if command -v docker >/dev/null 2>&1; then
  ids="$(docker ps -aq --filter label=sigmahub.managed=true)"
  [ -n "$ids" ] && docker rm -f $ids
  nets="$(docker network ls -q --filter label=sigmahub.managed=true)"
  [ -n "$nets" ] && docker network rm $nets
  if [ "$PURGE_VOLUMES" = "1" ]; then
    vols="$(docker volume ls -q --filter label=sigmahub.managed=true)"
    [ -n "$vols" ] && docker volume rm -f $vols
  else
    echo "keeping named volumes; re-run with --purge-volumes to delete application data"
  fi
fi

# 2. The WireGuard mesh: interface down, then the config and the private key.
if command -v wg-quick >/dev/null 2>&1 && ip link show sigma0 >/dev/null 2>&1; then
  wg-quick down "${DATA_DIR}/sigma0.conf" || true
fi
rm -f "${DATA_DIR}/sigma0.conf" "${DATA_DIR}/wg.key"

# 3. The systemd units. The live nftables ruleset is deliberately left in place
#    — dropping a host's firewall as a side effect of returning the machine
#    would be a security change you did not ask for. Only our claim to re-apply
#    it on the next boot is removed.
if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now sigmad.service >/dev/null 2>&1 || true
  systemctl disable sigmahub-nftables.service >/dev/null 2>&1 || true
fi
rm -f /etc/systemd/system/sigmad.service /etc/systemd/system/sigmahub-nftables.service
rm -f /etc/sigmad/env /etc/sigmahub/nftables.conf
rmdir /etc/sigmad /etc/sigmahub 2>/dev/null || true
command -v systemctl >/dev/null 2>&1 && systemctl daemon-reload

# 4. The agent's data directory (identity, journal, desired state) and binary.
rm -rf "${DATA_DIR}"
rm -f /usr/local/bin/sigmad

echo "sigmahub agent removed. Disconnect the server in the dashboard if you have not already."
