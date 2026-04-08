#!/usr/bin/env bash
# ============================================================================
# 007 Bond — Client Installer
# ============================================================================
#
# Production installer for 007 Bond client. Downloads a pre-built binary,
# sets up FHS directories, systemd service, auto-detects network interfaces,
# and configures multi-path bonding.
#
# Usage:
#   # Full install with server details:
#   curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
#     sudo SERVER_IP=203.0.113.1 SERVER_PUB=<base64_key> CLIENT_KEY=<base64_key> bash
#
#   # Or with arguments:
#   sudo bash install-007-client.sh --server-ip 203.0.113.1 --server-pub <key> --client-key <key>
#
#   # Upgrade (preserves existing config):
#   sudo bash install-007-client.sh --tag v1.2.0
#
# Requirements:
#   - Ubuntu/Debian or Raspberry Pi OS (amd64, arm64, armv7l)
#   - Root privileges
#   - Internet access (GitHub)
#
# FHS Layout:
#   /opt/007/           — binary, helper scripts
#   /etc/007/           — config (.env, WireGuard keys)
#   /var/lib/007/       — persistent data
#   /var/log/007/       — logs
#
# After install, the client:
#   1. Starts 007 and creates bond0 TUN
#   2. Configures WireGuard with server peer
#   3. Auto-detects all network interfaces
#   4. Adds each as a bond path (multi-path bonding)
#   5. Monitors interfaces and re-adds bond paths on change
#
# ============================================================================
set -euo pipefail

# ─── Configuration ────────────────────────────────────────────────────────
REPO="brianwynne/007"
INSTALL_DIR="/opt/007"
CONFIG_DIR="/etc/007"
DATA_DIR="/var/lib/007"
LOG_DIR="/var/log/007"
SERVICE_NAME="007-bond"
SERVICE_USER="bond007"
INTERFACE="bond0"
TUNNEL_IP="10.7.0.2/24"
SERVER_PORT="51820"
API_ADDR="0.0.0.0:8007"

# ─── Colours ──────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
err()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die()   { err "$@"; exit 1; }

# ─── Root check ───────────────────────────────────────────────────────────
[[ "$(id -u)" -eq 0 ]] || die "Must run as root"

# ─── Parse arguments / environment ────────────────────────────────────────
TAG=""
SRV_IP="${SERVER_IP:-}"
SRV_PUB="${SERVER_PUB:-}"
CLI_KEY="${CLIENT_KEY:-}"
ENROLL_URL="${ENROLL_URL:-}"
ENROLL_TOKEN="${ENROLL_TOKEN:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --tag)         TAG="$2"; shift 2 ;;
        --server-ip)   SRV_IP="$2"; shift 2 ;;
        --server-pub)  SRV_PUB="$2"; shift 2 ;;
        --client-key)  CLI_KEY="$2"; shift 2 ;;
        --server-port) SERVER_PORT="$2"; shift 2 ;;
        --enroll)      ENROLL_URL="$2"; shift 2 ;;
        --token)       ENROLL_TOKEN="$2"; shift 2 ;;
        *)             die "Unknown argument: $1" ;;
    esac
done

# ─── Detect architecture ─────────────────────────────────────────────────
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  PLATFORM="linux-amd64" ;;
    aarch64) PLATFORM="linux-arm64" ;;
    armv7l)  PLATFORM="linux-arm" ;;
    *)       die "Unsupported architecture: $ARCH" ;;
esac
info "Platform: $PLATFORM ($ARCH)"

# ─── Detect install vs upgrade ────────────────────────────────────────────
UPGRADE=false
if [[ -f "$CONFIG_DIR/.env" ]]; then
    UPGRADE=true
    info "Existing installation detected — performing upgrade"
    source "$CONFIG_DIR/.env"
    # Preserve existing server details
    SRV_IP="${SRV_IP:-${SERVER_IP_SAVED:-}}"
    SRV_PUB="${SRV_PUB:-${SERVER_PUB_SAVED:-}}"
fi

# Validate required params for fresh install
ENROLL_MODE=false
if [[ "$UPGRADE" == "false" ]]; then
    if [[ -n "$ENROLL_URL" && -n "$ENROLL_TOKEN" ]]; then
        ENROLL_MODE=true
        info "Enrollment mode: will exchange keys with $ENROLL_URL"
    elif [[ -n "$SRV_IP" && -n "$SRV_PUB" && -n "$CLI_KEY" ]]; then
        : # Manual key mode — all params provided
    else
        echo ""
        echo "Usage — either provide keys directly:"
        echo "  sudo SERVER_IP=x SERVER_PUB=x CLIENT_KEY=x bash $0"
        echo ""
        echo "Or use enrollment (recommended):"
        echo "  sudo ENROLL_URL=http://server:8017 ENROLL_TOKEN=<token> bash $0"
        echo ""
        die "Missing required parameters"
    fi
fi

# ─── Install system dependencies ─────────────────────────────────────────
info "Checking dependencies..."
for pkg in wireguard-tools curl jq; do
    if ! dpkg -s "$pkg" > /dev/null 2>&1; then
        info "Installing $pkg..."
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" > /dev/null 2>&1
    fi
done

# ─── Create service user ─────────────────────────────────────────────────
if ! id "$SERVICE_USER" > /dev/null 2>&1; then
    info "Creating service user: $SERVICE_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    ok "User created: $SERVICE_USER"
else
    ok "User exists: $SERVICE_USER"
fi

# ─── Stop existing service ───────────────────────────────────────────────
if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    info "Stopping $SERVICE_NAME..."
    systemctl stop "$SERVICE_NAME"
fi

# Clean up stale interface
ip link del "$INTERFACE" 2>/dev/null || true
rm -f "/var/run/wireguard/${INTERFACE}.sock"

# ─── Determine version to install ────────────────────────────────────────
if [[ -z "$TAG" ]]; then
    info "Fetching latest release..."
    TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | jq -r .tag_name)
    if [[ -z "$TAG" || "$TAG" == "null" ]]; then
        die "Could not determine latest release"
    fi
fi
info "Installing version: $TAG"

# ─── Download binary ─────────────────────────────────────────────────────
BINARY_URL="https://github.com/$REPO/releases/download/$TAG/007-$PLATFORM"
info "Downloading 007-$PLATFORM..."
mkdir -p "$INSTALL_DIR"
curl -fsSL "$BINARY_URL" -o "$INSTALL_DIR/007.new" || die "Download failed — check release tag: $TAG"
chmod +x "$INSTALL_DIR/007.new"

if ! "$INSTALL_DIR/007.new" --version > /dev/null 2>&1; then
    rm -f "$INSTALL_DIR/007.new"
    die "Downloaded binary is not executable or invalid"
fi
INSTALLED_VERSION=$("$INSTALL_DIR/007.new" --version 2>&1 || echo "$TAG")
mv "$INSTALL_DIR/007.new" "$INSTALL_DIR/007"
ok "Binary installed: $INSTALLED_VERSION"

# ─── Create directories ──────────────────────────────────────────────────
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" /var/run/wireguard
chown "$SERVICE_USER":"$SERVICE_USER" "$INSTALL_DIR" "$DATA_DIR" "$LOG_DIR"
chown root:"$SERVICE_USER" "$CONFIG_DIR"
chmod 750 "$CONFIG_DIR"
chown "$SERVICE_USER":"$SERVICE_USER" /var/run/wireguard

# ─── Enrollment key exchange ──────────────────────────────────────────────
if [[ "$ENROLL_MODE" == "true" && ! -f "$CONFIG_DIR/client.key" ]]; then
    info "Generating local WireGuard keypair..."
    CLI_KEY=$(wg genkey)
    CLI_PUB=$(echo "$CLI_KEY" | wg pubkey)

    info "Enrolling with server at $ENROLL_URL..."
    ENROLL_RESP=$(curl -fsSL --max-time 10 \
        -X POST "$ENROLL_URL/api/enroll" \
        -H "Content-Type: application/json" \
        -d "{\"token\": \"$ENROLL_TOKEN\", \"public_key\": \"$CLI_PUB\"}" \
        2>/dev/null) || die "Enrollment failed — check ENROLL_URL and token"

    # Parse response
    ENROLL_STATUS=$(echo "$ENROLL_RESP" | jq -r '.status // empty' 2>/dev/null)
    if [[ "$ENROLL_STATUS" != "enrolled" ]]; then
        ENROLL_ERR=$(echo "$ENROLL_RESP" | jq -r '.error // "unknown error"' 2>/dev/null)
        die "Enrollment rejected: $ENROLL_ERR"
    fi

    SRV_PUB=$(echo "$ENROLL_RESP" | jq -r '.server_pub')
    TUNNEL_IP=$(echo "$ENROLL_RESP" | jq -r '.tunnel_ip')/24
    ENDPOINT=$(echo "$ENROLL_RESP" | jq -r '.endpoint')
    SRV_IP=$(echo "$ENDPOINT" | cut -d: -f1)
    SERVER_PORT=$(echo "$ENDPOINT" | cut -d: -f2)

    ok "Enrolled — tunnel IP: ${TUNNEL_IP%/*}, server: $ENDPOINT"
fi

# ─── Save WireGuard keys (first install only) ────────────────────────────
if [[ ! -f "$CONFIG_DIR/client.key" && -n "$CLI_KEY" ]]; then
    info "Saving WireGuard keys..."
    echo "$CLI_KEY" > "$CONFIG_DIR/client.key"
    chown "$SERVICE_USER":"$SERVICE_USER" "$CONFIG_DIR/client.key"
    chmod 600 "$CONFIG_DIR/client.key"
    ok "Client private key saved"
fi

if [[ ! -f "$CONFIG_DIR/server.pub" && -n "$SRV_PUB" ]]; then
    echo "$SRV_PUB" > "$CONFIG_DIR/server.pub"
    chmod 644 "$CONFIG_DIR/server.pub"
    ok "Server public key saved"
fi

# ─── Create environment file (first install only) ────────────────────────
if [[ ! -f "$CONFIG_DIR/.env" ]]; then
    info "Creating configuration..."
    cat > "$CONFIG_DIR/.env" << EOF
# 007 Bond Client Configuration
# Generated: $(date -u '+%Y-%m-%d %H:%M:%S UTC')

# Server
SERVER_IP_SAVED=$SRV_IP
SERVER_PUB_SAVED=$SRV_PUB
SERVER_PORT=$SERVER_PORT

# WireGuard
INTERFACE=$INTERFACE
TUNNEL_IP=$TUNNEL_IP

# Bond features
# Presets: broadcast (40ms), studio (80ms), field (200ms)
BOND_PRESET=field
BOND_FEC_MODE=sliding
BOND_API=$API_ADDR
LOG_LEVEL=error

# Paths (do not change)
INSTALL_DIR=$INSTALL_DIR
CONFIG_DIR=$CONFIG_DIR
DATA_DIR=$DATA_DIR
LOG_DIR=$LOG_DIR
EOF
    chown root:"$SERVICE_USER" "$CONFIG_DIR/.env"
    chmod 640 "$CONFIG_DIR/.env"
    ok "Configuration created: $CONFIG_DIR/.env"
else
    ok "Existing configuration preserved"
fi

# ─── Create WireGuard setup script ───────────────────────────────────────
cat > "$INSTALL_DIR/setup-wg.sh" << 'SETUP_EOF'
#!/usr/bin/env bash
# Configure WireGuard and bond paths after 007 creates the TUN interface.
set -euo pipefail

source /etc/007/.env

# Wait for interface
for i in $(seq 1 30); do
    ip link show "$INTERFACE" > /dev/null 2>&1 && break
    sleep 0.2
done
ip link show "$INTERFACE" > /dev/null 2>&1 || { echo "ERROR: $INTERFACE not created"; exit 1; }

# Configure WireGuard
# If gateway mode: allow all traffic through tunnel (server NATs it)
# Otherwise: only tunnel network
ALLOWED_IPS="10.7.0.0/24"
if [[ "${BOND_GATEWAY:-}" == "on" ]]; then
    ALLOWED_IPS="0.0.0.0/0"
fi

wg set "$INTERFACE" \
    private-key "$CONFIG_DIR/client.key" \
    peer "$(cat "$CONFIG_DIR/server.pub")" \
    endpoint "${SERVER_IP_SAVED}:${SERVER_PORT}" \
    allowed-ips "$ALLOWED_IPS" \
    persistent-keepalive 25

# Assign tunnel IP and bring up
ip addr add "$TUNNEL_IP" dev "$INTERFACE" 2>/dev/null || true
ip link set "$INTERFACE" up

echo "WireGuard configured on $INTERFACE → ${SERVER_IP_SAVED}:${SERVER_PORT}"

# Add bond paths for all available interfaces
/opt/007/add-bond-paths.sh || true
SETUP_EOF
chmod +x "$INSTALL_DIR/setup-wg.sh"

# ─── Create bond path discovery script ────────────────────────────────────
cat > "$INSTALL_DIR/add-bond-paths.sh" << 'PATHS_EOF'
#!/usr/bin/env bash
# Detect all network interfaces and add them as bond paths.
# Called by setup-wg.sh on startup and by the path monitor timer.
set -euo pipefail

source /etc/007/.env

IFACE="${INTERFACE:-bond0}"
WG_SOCK="/var/run/wireguard/${IFACE}.sock"

if [[ ! -S "$WG_SOCK" ]]; then
    echo "UAPI socket not found: $WG_SOCK"
    exit 1
fi

# Get peer public key in hex for UAPI
SERVER_PUB_HEX=$(cat "$CONFIG_DIR/server.pub" | base64 -d | xxd -p -c 32)

# Build UAPI command: clear existing bond paths, then add all detected
UAPI_CMD="set=1\npublic_key=${SERVER_PUB_HEX}\nclear_bond_endpoints=true\n"

COUNT=0
SKIPPED=0
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    # Skip the tunnel interface itself
    [[ "$iface" == "$IFACE" ]] && continue

    LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [[ -z "$LOCAL_IP" ]]; then
        continue
    fi

    # Verify this interface can route to the server
    if ! ip route get "$SERVER_IP_SAVED" from "$LOCAL_IP" > /dev/null 2>&1; then
        echo "  Skip:      $iface ($LOCAL_IP) — no route to ${SERVER_IP_SAVED}"
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    echo "  Bond path: $iface ($LOCAL_IP) → ${SERVER_IP_SAVED}:${SERVER_PORT}"
    UAPI_CMD="${UAPI_CMD}bond_endpoint=${SERVER_IP_SAVED}:${SERVER_PORT}@${LOCAL_IP}\n"
    COUNT=$((COUNT + 1))
done

if [[ "$COUNT" -eq 0 ]]; then
    echo "WARNING: No network interfaces found for bonding"
    exit 0
fi

printf "${UAPI_CMD}\n" | nc -U "$WG_SOCK" -w 1 > /dev/null 2>&1 || true
echo "Configured $COUNT bond path(s)${SKIPPED:+, skipped $SKIPPED (no route)}"
PATHS_EOF
chmod +x "$INSTALL_DIR/add-bond-paths.sh"

# ─── Create systemd service ──────────────────────────────────────────────
info "Creating systemd service..."
cat > "/etc/systemd/system/${SERVICE_NAME}.service" << EOF
[Unit]
Description=007 Bond — Multi-Path Network Bonding Client
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
EnvironmentFile=$CONFIG_DIR/.env
ExecStartPre=+/bin/sh -c 'ip link del ${INTERFACE} 2>/dev/null; rm -f /var/run/wireguard/${INTERFACE}.sock; true'
# Go runtime tuning — reduce GC frequency, cap memory
Environment=GOGC=200
Environment=GOMEMLIMIT=64MiB
ExecStart=$INSTALL_DIR/007 -f \${INTERFACE}
ExecStartPost=+$INSTALL_DIR/setup-wg.sh
ExecStopPost=+/bin/sh -c 'ip link del ${INTERFACE} 2>/dev/null; rm -f /var/run/wireguard/${INTERFACE}.sock; true'

# Restart policy
Restart=on-failure
RestartSec=5
TimeoutStopSec=10

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=007-bond

# Capabilities — 007 needs network admin for TUN/WireGuard, not full root
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE

# Security hardening
NoNewPrivileges=true
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=$DATA_DIR $LOG_DIR /var/run/wireguard

[Install]
WantedBy=multi-user.target
EOF

# ─── Create path monitor timer ───────────────────────────────────────────
# Periodically re-scans interfaces and updates bond paths.
# Handles WiFi reconnects, cellular dongle plug/unplug, DHCP renewals.
cat > "/etc/systemd/system/${SERVICE_NAME}-paths.service" << EOF
[Unit]
Description=007 Bond — Refresh bond paths
After=${SERVICE_NAME}.service
Requires=${SERVICE_NAME}.service

[Service]
Type=oneshot
User=$SERVICE_USER
Group=$SERVICE_USER
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
ExecStart=$INSTALL_DIR/add-bond-paths.sh
StandardOutput=journal
SyslogIdentifier=007-bond-paths
EOF

cat > "/etc/systemd/system/${SERVICE_NAME}-paths.timer" << EOF
[Unit]
Description=007 Bond — Periodic bond path refresh

[Timer]
OnBootSec=30s
OnUnitActiveSec=30s
AccuracySec=5s

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
ok "Systemd service and path monitor created"

# ─── Install CLI ─────────────────────────────────────────────────────────
info "Installing management CLI..."
curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/007-cli.sh" -o /usr/local/bin/007-bond 2>/dev/null \
    || cp "$(dirname "$0")/007-cli.sh" /usr/local/bin/007-bond 2>/dev/null \
    || warn "Could not install CLI"
if [[ -f /usr/local/bin/007-bond ]]; then
    chmod +x /usr/local/bin/007-bond
    ok "CLI installed: 007-bond"
fi

# ─── Log rotation ────────────────────────────────────────────────────────
cat > /etc/logrotate.d/007-bond << EOF
$LOG_DIR/*.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
EOF

# ─── Login banner (MOTD) ─────────────────────────────────────────────────
MOTD_FILE="/etc/update-motd.d/10-007-bond"
if [[ ! -f "$MOTD_FILE" ]]; then
    info "Creating login welcome screen..."
    cat > "$MOTD_FILE" << 'MOTDEOF'
#!/bin/bash
echo ""
echo "  ======================================================"
echo "           007 Bond — Multi-Path Bonding Client"
echo "  ======================================================"
echo ""
echo "  Service:"
echo "    sudo 007-bond status            Service & tunnel status"
echo "    sudo 007-bond start             Start 007"
echo "    sudo 007-bond stop              Stop 007"
echo "    sudo 007-bond restart           Restart 007"
echo "    sudo 007-bond logs              Tail service logs"
echo ""
echo "  Monitoring:"
echo "    sudo 007-bond stats             FEC, ARQ, jitter stats"
echo "    sudo 007-bond paths             Per-path health (RTT, loss)"
echo "    sudo 007-bond version           Show installed version"
echo ""
echo "  Maintenance:"
echo "    sudo 007-bond upgrade           Upgrade to latest release"
echo "    sudo 007-bond uninstall         Remove 007 Bond"
echo ""
echo "  Config: /etc/007/.env"
echo "  Logs:   journalctl -u 007-bond -f"
echo ""
MOTDEOF
    chmod 755 "$MOTD_FILE"
    ok "Login welcome screen created"
fi

# ─── Enable and start ────────────────────────────────────────────────────
info "Enabling and starting services..."
systemctl enable "$SERVICE_NAME" > /dev/null 2>&1
systemctl enable "${SERVICE_NAME}-paths.timer" > /dev/null 2>&1
systemctl start "$SERVICE_NAME"

# Wait for startup and WireGuard config
sleep 4

systemctl start "${SERVICE_NAME}-paths.timer"

if systemctl is-active --quiet "$SERVICE_NAME"; then
    ok "Service running"
else
    err "Service failed to start"
    journalctl -u "$SERVICE_NAME" --no-pager -n 20
    exit 1
fi

# ─── Verify connectivity ─────────────────────────────────────────────────
echo ""
info "Testing tunnel connectivity..."
sleep 2

TUNNEL_PEER="${TUNNEL_IP%.*}.1"
if ping -c 3 -W 3 "$TUNNEL_PEER" > /dev/null 2>&1; then
    ok "Tunnel to $TUNNEL_PEER is working"
else
    warn "Tunnel not yet reachable — server may need to accept this client"
fi

# Show bond paths
echo ""
info "Bond paths:"
BOND_COUNT=0
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [[ "$iface" == "$INTERFACE" ]] && continue
    LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [[ -n "$LOCAL_IP" ]]; then
        echo -e "  ${GREEN}●${NC} $iface ($LOCAL_IP)"
        BOND_COUNT=$((BOND_COUNT + 1))
    fi
done

# ─── Print summary ───────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}================================================================${NC}"
echo -e "${BOLD}  007 Bond Client${NC}"
echo -e "${BOLD}================================================================${NC}"
echo ""
echo -e "  Version:    $INSTALLED_VERSION"
echo -e "  Interface:  $INTERFACE"
echo -e "  Tunnel IP:  ${TUNNEL_IP%/*}"
echo -e "  Server:     ${SRV_IP:-$(grep SERVER_IP_SAVED $CONFIG_DIR/.env | cut -d= -f2)}:${SERVER_PORT}"
echo -e "  Bond paths: $BOND_COUNT interface(s)"
echo -e "  Service:    systemctl status $SERVICE_NAME"
echo -e "  Logs:       journalctl -u $SERVICE_NAME -f"
echo -e "  Stats:      007-bond stats"
echo -e "  Paths:      007-bond paths"
echo ""
echo -e "  Path monitor runs every 30s (handles WiFi/cellular changes)"
echo ""
echo -e "${BOLD}================================================================${NC}"

if [[ "$UPGRADE" == "true" ]]; then
    ok "Upgrade complete"
else
    ok "Installation complete"
fi
