#!/usr/bin/env bash
# install.sh — install a Stored data node from a SIGNED release
# (WP-EDGE-3; docs/DEPLOY.md "Data nodes"). Run as root on the node.
#
#   install.sh <release-base-url> <control-plane-url> <public-host> [port]
#
#   release-base-url   where the release's files are: <url>/vaultaire-linux
#                      and <url>/vaultaire-linux.sig (e.g. a GitHub release's
#                      download URL)
#   control-plane-url  the origin the node asks (https://stored.ge)
#   public-host        this node's hostname (what clients use as endpoint)
#   port               listen port (default 8100)
#
# The binary is installed only after `release-sign verify` accepts it under
# the release verifier key (docs/RELEASES.md): the key is read from
# RELEASE_VKEY_FILE (default ./release.vkey — the key the release body and
# the trust host publish; until the owner's key ceremony there is none, and
# this script refuses). `release-sign` must be on PATH (built from the same
# tagged source: go build ./cmd/tools/release-sign). Never `curl | sh`.
#
# The token is NOT an argument: mint it with the registry (PUT
# /api/v1/admin/data-plane-hosts/<public-host>, shown once) and write it to
# /etc/vaultaire/node_token yourself (0600 root) — the script waits for it.
set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 ]]; then
	sed -n '2,25p' "$0" >&2
	exit 2
fi
base=${1%/}
control_plane=${2%/}
public_host=$3
port=${4:-8100}
vkey=${RELEASE_VKEY_FILE:-./release.vkey}

[[ $EUID -eq 0 ]] || { echo "install.sh: run as root" >&2; exit 1; }
[[ $control_plane == https://* || $control_plane == http://127.0.0.1* ]] || { echo "install.sh: the control plane is https://…" >&2; exit 1; }
[[ $port =~ ^[0-9]+$ ]] || { echo "install.sh: port $port is not a number" >&2; exit 1; }
command -v release-sign >/dev/null || { echo "install.sh: release-sign is not on PATH (go build ./cmd/tools/release-sign)" >&2; exit 1; }
[[ -s $vkey ]] || { echo "install.sh: no release verifier key at $vkey (RELEASE_VKEY_FILE) — refusing to install an unverified binary" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsSL --proto '=https,http' -o "$work/vaultaire-linux" "$base/vaultaire-linux"
curl -fsSL --proto '=https,http' -o "$work/vaultaire-linux.sig" "$base/vaultaire-linux.sig"
release-sign verify -vkey "@$vkey" "$work/vaultaire-linux" "$work/vaultaire-linux.sig"

install -d -m 0755 /opt/vaultaire/bin /etc/vaultaire
install -m 0755 "$work/vaultaire-linux" /opt/vaultaire/bin/vaultaire-node
here=$(cd "$(dirname "$0")" && pwd)
install -m 0644 "$here/vaultaire-node.service" /etc/systemd/system/vaultaire-node.service
umask 077
cat > /etc/vaultaire/node.env <<ENV
NODE_CONTROL_PLANE_URL=$control_plane
NODE_PUBLIC_HOST=$public_host
PORT=$port
ENV
chmod 0644 /etc/vaultaire/node.env

if [[ ! -s /etc/vaultaire/node_token ]]; then
	echo "install.sh: write the registry's token to /etc/vaultaire/node_token (0600 root), then:"
	echo "  systemctl daemon-reload && systemctl enable --now vaultaire-node"
else
	chmod 0600 /etc/vaultaire/node_token
	systemctl daemon-reload
	systemctl enable --now vaultaire-node
fi
echo "install.sh: the node heartbeats to $control_plane/api/v1/edge/heartbeat every 30 s;"
echo "  its registry row: GET $control_plane/api/v1/admin/data-plane-hosts/$public_host (health up within 90 s)"
