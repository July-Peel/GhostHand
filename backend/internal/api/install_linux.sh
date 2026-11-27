#!/bin/sh
set -eu

if [ "$#" -lt 2 ]; then
    echo "Usage: install.sh <server> <node_key> [fingerprint]" >&2
    exit 1
fi

SERVER="$1"
NODE_KEY="$2"
FINGERPRINT="${3:-}"

if [ "$(id -u)" -ne 0 ]; then
    echo "Please run this command as root." >&2
    exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64|aarch64|arm64) ;;
    *)
        echo "GhostHand Linux Agent requires 64-bit x86_64 or arm64 (detected: $ARCH)." >&2
        exit 1
        ;;
esac

case "$SERVER" in
    https://*|http://localhost|http://localhost:*|http://127.0.0.1|http://127.0.0.1:*|http://\[::1\]|http://\[::1\]:*) ;;
    *)
        echo "Use the HTTPS controller origin to install GhostHand." >&2
        exit 1
        ;;
esac

OS_ID="linux"
if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-linux}"
fi

ensure_downloader() {
    if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then
        return 0
    fi
    if command -v apk >/dev/null 2>&1; then
        apk add --no-cache ca-certificates wget
    elif command -v apt-get >/dev/null 2>&1; then
        apt-get update && apt-get install -y ca-certificates curl
    else
        echo "Requires curl or wget to download GhostHand." >&2
        exit 1
    fi
}

ensure_downloader

AUTH_TOKEN="$(printf '%s' "$NODE_KEY" | base64 | tr -d '\r\n')"
BASE_URL="${SERVER}/api/agent/install/linux/"

http_get() {
    URL="$1"
    OUT="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --max-redirs 0 --connect-timeout 15 --max-time 600 \
            -H "Authorization: Bearer ${AUTH_TOKEN}" \
            "$URL" -o "$OUT"
    else
        wget -q -T 120 --header="Authorization: Bearer ${AUTH_TOKEN}" \
            -O "$OUT" "$URL"
    fi
}

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

MANIFEST_FILE="${STAGE}/manifest.json"
http_get "${BASE_URL}manifest" "$MANIFEST_FILE"

MANIFEST_FLAT="$(tr -d '\r\n' < "$MANIFEST_FILE")"
EXPECTED_SIZE="$(printf '%s' "$MANIFEST_FLAT" | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"ghosthand-core"[^}]*"size"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
EXPECTED_SHA="$(printf '%s' "$MANIFEST_FLAT" | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"ghosthand-core"[^}]*"sha256"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F]\{64\}\)".*/\1/p')"

if [ -z "$EXPECTED_SIZE" ] || [ -z "$EXPECTED_SHA" ]; then
    echo "Invalid GhostHand manifest entry for ghosthand-core." >&2
    exit 1
fi

BIN_TMP="${STAGE}/ghosthand-core"
echo "Downloading ghosthand-core..."
http_get "${BASE_URL}files/ghosthand-core" "$BIN_TMP"

ACTUAL_SIZE="$(wc -c < "$BIN_TMP" | tr -d ' ')"
if [ "$ACTUAL_SIZE" != "$EXPECTED_SIZE" ]; then
    echo "Size mismatch for ghosthand-core (expected ${EXPECTED_SIZE}, got ${ACTUAL_SIZE})." >&2
    exit 1
fi

printf '%s  %s\n' "$EXPECTED_SHA" "$BIN_TMP" | sha256sum -c - >/dev/null

INSTALL_DIR="/opt/ghosthand"
CONFIG_DIR="/etc/ghosthand"
mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"
chmod 0755 "$INSTALL_DIR"
chmod 0700 "$CONFIG_DIR"

# Stop existing service before replacing the binary.
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop ghosthand-core.service 2>/dev/null || true
elif command -v rc-service >/dev/null 2>&1; then
    rc-service ghosthand-core stop 2>/dev/null || true
fi

cp "$BIN_TMP" "${INSTALL_DIR}/ghosthand-core.new"
chmod 0755 "${INSTALL_DIR}/ghosthand-core.new"
mv -f "${INSTALL_DIR}/ghosthand-core.new" "${INSTALL_DIR}/ghosthand-core"

ENV_FILE="${CONFIG_DIR}/agent.env"
umask 077
cat > "$ENV_FILE" <<EOF
GH_SERVER=${SERVER}
GH_KEY=${NODE_KEY}
GH_FINGERPRINT=${FINGERPRINT}
EOF
chmod 0600 "$ENV_FILE"

if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    echo "Detected systemd (${OS_ID}); installing ghosthand-core.service..."
    cat > /etc/systemd/system/ghosthand-core.service <<'EOF'
[Unit]
Description=GhostHand Linux Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ghosthand/agent.env
ExecStart=/opt/ghosthand/ghosthand-core --service
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 /etc/systemd/system/ghosthand-core.service
    systemctl daemon-reload
    systemctl enable ghosthand-core.service >/dev/null 2>&1 || true
    systemctl restart ghosthand-core.service
    sleep 1
    if ! systemctl is-active --quiet ghosthand-core.service; then
        echo "ghosthand-core.service failed to start. Check: journalctl -u ghosthand-core.service" >&2
        exit 1
    fi
    echo "GhostHand systemd service is running and will connect to the controller."
elif command -v rc-service >/dev/null 2>&1 && command -v rc-update >/dev/null 2>&1; then
    echo "Detected OpenRC (${OS_ID}); installing /etc/init.d/ghosthand-core..."
    cat > /etc/init.d/ghosthand-core <<'EOF'
#!/sbin/openrc-run

name="ghosthand-core"
description="GhostHand Linux Agent"
command="/opt/ghosthand/ghosthand-core"
command_args="--service"
command_background="yes"
pidfile="/run/ghosthand-core.pid"
output_log="/var/log/ghosthand-core.log"
error_log="/var/log/ghosthand-core.log"

depend() {
    need net
    after firewall
}

start_pre() {
    if [ -r /etc/ghosthand/agent.env ]; then
        while IFS='=' read -r k v; do
            case "$k" in
                ''|'#'*) continue ;;
            esac
            export "$k=$v"
        done < /etc/ghosthand/agent.env
    fi
}
EOF
    chmod 0755 /etc/init.d/ghosthand-core
    rc-update add ghosthand-core default >/dev/null 2>&1 || true
    rc-service ghosthand-core restart
    sleep 1
    if ! rc-service ghosthand-core status >/dev/null 2>&1; then
        echo "ghosthand-core OpenRC service failed to start. Check: /var/log/ghosthand-core.log" >&2
        exit 1
    fi
    echo "GhostHand OpenRC service is running and will connect to the controller."
else
    echo "Installed /opt/ghosthand/ghosthand-core and /etc/ghosthand/agent.env."
    echo "Neither systemd nor OpenRC was detected; start manually with:"
    echo "  set -a; . /etc/ghosthand/agent.env; set +a; /opt/ghosthand/ghosthand-core"
fi
