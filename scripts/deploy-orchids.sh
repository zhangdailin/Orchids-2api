#!/usr/bin/env bash
# Deploy a released orchids-server build to this host.
#
# Usage:
#   deploy-orchids.sh --artifact ./orchids-server-linux-amd64 \
#                     --checksum ./orchids-server-linux-amd64.sha256 \
#                     [--build-info ./orchids-server-linux-amd64.build-info.txt] \
#                     [--install-dir /opt/orchids-2api] [--service orchids-2api]
#
# The script refuses to install a binary whose checksum does not match, keeps the
# previous binary as a timestamped backup, restarts the service, and rolls back
# automatically when the service does not come up. Deploying a build by hand is
# exactly how a Windows binary once reached a Linux host; the checksum plus the
# built-info manifest make that mistake impossible to miss.
#
# Every deploy also records what is actually installed in
# orchids-server.deploy-info.txt. A release installs the pipeline's build-info
# file; a manual install records its own checksum and time, so the version a
# report quotes can always be traced to the bytes on disk instead of to whatever
# build-info file was left behind by an earlier release.

set -euo pipefail

ARTIFACT=""
CHECKSUM=""
BUILD_INFO=""
INSTALL_DIR="/opt/orchids-2api"
SERVICE="orchids-2api"
BINARY_NAME="orchids-server"
HEALTH_PATH="/admin"
HEALTH_TIMEOUT=20

while [ $# -gt 0 ]; do
  case "$1" in
    --artifact) ARTIFACT="$2"; shift 2 ;;
    --checksum) CHECKSUM="$2"; shift 2 ;;
    --build-info) BUILD_INFO="$2"; shift 2 ;;
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --service) SERVICE="$2"; shift 2 ;;
    --binary-name) BINARY_NAME="$2"; shift 2 ;;
    --health-path) HEALTH_PATH="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$ARTIFACT" ] || [ ! -f "$ARTIFACT" ]; then
  echo "usage: $0 --artifact <binary> [--checksum <sha256 file>]" >&2
  exit 2
fi

# 1. Refuse anything that is not a Linux/amd64 ELF. This is the guard that the
#    "uploaded server.exe" incident needs.
file_type="$(file -b "$ARTIFACT" || true)"
case "$file_type" in
  *ELF*64-bit*x86-64*) : ;;
  *) echo "refusing to deploy: $ARTIFACT is not a linux/amd64 ELF ($file_type)" >&2; exit 3 ;;
esac

# 2. Verify the published checksum when one is supplied.
if [ -n "$CHECKSUM" ]; then
  if [ ! -f "$CHECKSUM" ]; then
    echo "checksum file not found: $CHECKSUM" >&2
    exit 4
  fi
  expected="$(awk '{print $1}' "$CHECKSUM" | head -1)"
  actual="$(sha256sum "$ARTIFACT" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then
    echo "checksum mismatch: expected $expected, got $actual" >&2
    exit 5
  fi
  echo "checksum ok: $actual"
fi

# 3. Install atomically, keeping the previous binary.
cd "$INSTALL_DIR"
stamp="$(date -u +%Y%m%d-%H%M%S)"
backup="${BINARY_NAME}.backup-${stamp}"
cp -f "$BINARY_NAME" "$backup" 2>/dev/null || true
install -m 0755 "$ARTIFACT" "${BINARY_NAME}.incoming"
mv -f "${BINARY_NAME}.incoming" "$BINARY_NAME"
installed_sha="$(sha256sum "$BINARY_NAME" | awk '{print $1}')"
echo "installed ${installed_sha} (previous kept as $backup)"

# 3b. Record what is installed. The pipeline's build-info is authoritative when it
#     is supplied; otherwise the deploy-info file is the only honest source, and
#     it always names the checksum that is on disk right now.
if [ -n "$BUILD_INFO" ] && [ -f "$BUILD_INFO" ]; then
  cp -f "$BUILD_INFO" "${BINARY_NAME}.build-info.txt"
fi
{
  echo "sha256=${installed_sha}"
  echo "deployed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "artifact=${ARTIFACT}"
  if [ -n "$BUILD_INFO" ] && [ -f "$BUILD_INFO" ]; then
    echo "build_info=installed from $(basename "$BUILD_INFO")"
  else
    echo "build_info=not supplied (manual build; build-info file left untouched)"
  fi
} > "${BINARY_NAME}.deploy-info.txt"

# 4. Restart and verify; roll back on failure.
systemctl restart "$SERVICE"
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
healthy=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  if systemctl is-active --quiet "$SERVICE"; then
    # 200 (health page) and 302 (redirect to login) both prove the HTTP server
    # is answering; anything else keeps waiting until the deadline.
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:3002${HEALTH_PATH}" || echo 000)"
    case "$code" in
      200|301|302) healthy=1; break ;;
    esac
  fi
  sleep 1
done

if [ "$healthy" -eq 1 ]; then
  echo "deployed and healthy"
  echo "  build-info:  $(cat "${BINARY_NAME}.build-info.txt" 2>/dev/null | tr '\n' ' ')"
  echo "  deploy-info: $(cat "${BINARY_NAME}.deploy-info.txt" 2>/dev/null | tr '\n' ' ')"
  exit 0
fi

echo "service did not become healthy; rolling back to $backup" >&2
cp -f "$backup" "$BINARY_NAME"
systemctl restart "$SERVICE"
sleep 5
systemctl is-active "$SERVICE" || true
exit 6
