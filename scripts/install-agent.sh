#!/usr/bin/env bash
# Install or upgrade surfswarm-agent as a boot-time service on macOS (launchd)
# or Linux (systemd). Wraps the agent's own install command: finds the right
# binary, makes it executable, clears the macOS quarantine flag, installs,
# and shows the result.
#
#   sudo ./install-agent.sh --server ws://HOST:8080/agent --token TOKEN [--name NAME] [--binary PATH]
#
# Without --binary it looks next to the script for surfswarm-agent or
# surfswarm-agent-<os>-<arch>, then in dist/ for a repo checkout.
set -euo pipefail

usage() {
  sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
}

SERVER=""
TOKEN=""
NAME=""
BINARY=""
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$SERVER" ]; then
  echo "--server is required, for example --server ws://192.168.1.10:8080/agent" >&2
  exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "this needs root: run it with sudo" >&2
  exit 2
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  armv7l|armv6l) arch=arm ;;
esac

if [ -z "$BINARY" ]; then
  here="$(cd "$(dirname "$0")" && pwd)"
  for candidate in \
    "$here/surfswarm-agent" \
    "$here/surfswarm-agent-$os-$arch" \
    "$here/dist/surfswarm-agent-$os-$arch" \
    "$here/../dist/surfswarm-agent-$os-$arch"; do
    if [ -f "$candidate" ]; then
      BINARY="$candidate"
      break
    fi
  done
fi
if [ -z "$BINARY" ] || [ ! -f "$BINARY" ]; then
  echo "agent binary not found for $os/$arch; pass --binary PATH" >&2
  exit 2
fi

chmod +x "$BINARY"
if [ "$os" = "darwin" ]; then
  xattr -d com.apple.quarantine "$BINARY" 2>/dev/null || true
fi

echo "installing surfswarm-agent $("$BINARY" -version) from $BINARY"
args=(install --server "$SERVER")
if [ -n "$TOKEN" ]; then args+=(--token "$TOKEN"); fi
if [ -n "$NAME" ]; then args+=(--name "$NAME"); fi
"$BINARY" "${args[@]}"

sleep 2
echo
/usr/local/bin/surfswarm-agent status
echo "installed version: $(/usr/local/bin/surfswarm-agent -version)"
if [ "$os" = "darwin" ]; then
  echo "log: /var/log/surfswarm-agent.err.log"
  tail -n 5 /var/log/surfswarm-agent.err.log 2>/dev/null || true
else
  echo "log: journalctl -u surfswarm-agent -f"
fi
