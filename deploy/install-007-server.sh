#!/usr/bin/env bash
# ============================================================================
# 007 Bond — Server Installer
# ============================================================================
#
# Production installer for the 007 Bond server. Downloads a pre-built binary
# from GitHub releases, sets up FHS directories, systemd service, firewall,
# and WireGuard key management.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash
#   sudo bash install-007-server.sh [--tag vX.Y.Z]
#
# Requirements:
#   - Ubuntu/Debian Linux (amd64 or arm64)
#   - Root privileges
#   - Internet access (GitHub)
#
# FHS Layout:
#   /opt/007/           — binary
#   /etc/007/           — config (.env, WireGuard keys)
#   /var/lib/007/       — persistent data
#   /var/log/007/       — logs
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
TUNNEL_IP="10.7.0.1/24"
LISTEN_PORT="51820"
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

# ─── Parse arguments ──────────────────────────────────────────────────────
TAG=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --tag)  TAG="$2"; shift 2 ;;
        *)      die "Unknown argument: $1" ;;
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
info "Platform: $PLATFORM"

# ─── Detect install vs upgrade ────────────────────────────────────────────
UPGRADE=false
if [[ -f "$CONFIG_DIR/.env" ]]; then
    UPGRADE=true
    info "Existing installation detected — performing upgrade"
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

# Verify it runs
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

# ─── Generate WireGuard keys (first install only) ─────────────────────────
if [[ ! -f "$CONFIG_DIR/server.key" ]]; then
    info "Generating WireGuard keys..."
    wg genkey | tee "$CONFIG_DIR/server.key" | wg pubkey > "$CONFIG_DIR/server.pub"
    chown "$SERVICE_USER":"$SERVICE_USER" "$CONFIG_DIR/server.key"
    chmod 600 "$CONFIG_DIR/server.key"
    chmod 644 "$CONFIG_DIR/server.pub"

    # Generate client keys for initial provisioning
    wg genkey | tee "$CONFIG_DIR/client.key" | wg pubkey > "$CONFIG_DIR/client.pub"
    chmod 600 "$CONFIG_DIR/client.key"
    chmod 644 "$CONFIG_DIR/client.pub"
    ok "Keys generated"
else
    ok "Existing keys preserved"
fi

# ─── Create environment file (first install only) ────────────────────────
if [[ ! -f "$CONFIG_DIR/.env" ]]; then
    info "Creating configuration..."
    API_KEY=$(head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 24)
    cat > "$CONFIG_DIR/.env" << EOF
# 007 Bond Server Configuration
# Generated: $(date -u '+%Y-%m-%d %H:%M:%S UTC')

# WireGuard
INTERFACE=$INTERFACE
TUNNEL_IP=$TUNNEL_IP
LISTEN_PORT=$LISTEN_PORT

# Bond features
# Presets: broadcast (40ms), studio (80ms), field (200ms)
BOND_PRESET=field
BOND_FEC_MODE=sliding
BOND_API=$API_ADDR
BOND_API_KEY=$API_KEY
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

# ─── Create setup script (runs before 007 starts) ────────────────────────
cat > "$INSTALL_DIR/setup-wg.sh" << 'SETUP_EOF'
#!/usr/bin/env bash
# Configure WireGuard on bond0 after 007 creates the TUN interface.
# Called by systemd ExecStartPost.
set -euo pipefail

source /etc/007/.env

# Wait for bond0 to appear
for i in $(seq 1 30); do
    ip link show "$INTERFACE" > /dev/null 2>&1 && break
    sleep 0.2
done
ip link show "$INTERFACE" > /dev/null 2>&1 || { echo "ERROR: $INTERFACE not created"; exit 1; }

# Configure WireGuard
wg set "$INTERFACE" \
    listen-port "$LISTEN_PORT" \
    private-key "$CONFIG_DIR/server.key" \
    peer "$(cat "$CONFIG_DIR/client.pub")" \
    allowed-ips 10.7.0.2/32

# Assign tunnel IP and bring up
ip addr add "$TUNNEL_IP" dev "$INTERFACE" 2>/dev/null || true
ip link set "$INTERFACE" up

# Re-apply gateway mode if enabled
if [[ "${BOND_GATEWAY:-}" == "on" ]]; then
    OUTIF=$(ip route show default | awk '{print $5}' | head -1)
    OUTIF="${OUTIF:-ens5}"
    sysctl -w net.ipv4.ip_forward=1 > /dev/null
    iptables -I FORWARD 1 -i "$INTERFACE" -o "$OUTIF" -j ACCEPT 2>/dev/null || true
    iptables -I FORWARD 2 -i "$OUTIF" -o "$INTERFACE" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
    iptables -t nat -C POSTROUTING -s 10.7.0.0/24 -o "$OUTIF" -j MASQUERADE 2>/dev/null || \
        iptables -t nat -A POSTROUTING -s 10.7.0.0/24 -o "$OUTIF" -j MASQUERADE
    echo "Gateway mode re-applied (NAT via $OUTIF)"
fi

echo "WireGuard configured on $INTERFACE"
SETUP_EOF
chmod +x "$INSTALL_DIR/setup-wg.sh"

# ─── Create systemd service ──────────────────────────────────────────────
info "Creating systemd service..."
cat > "/etc/systemd/system/${SERVICE_NAME}.service" << EOF
[Unit]
Description=007 Bond — Multi-Path Network Bonding
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

systemctl daemon-reload
ok "Systemd service created: $SERVICE_NAME"

# ─── Install enrollment service ──────────────────────────────────────────
info "Installing enrollment service..."
curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/enroll-server.sh" -o "$INSTALL_DIR/enroll-server.sh" 2>/dev/null \
    || cp "$(dirname "$0")/enroll-server.sh" "$INSTALL_DIR/enroll-server.sh" 2>/dev/null \
    || warn "Could not install enrollment service"

if [[ -f "$INSTALL_DIR/enroll-server.sh" ]]; then
    chmod +x "$INSTALL_DIR/enroll-server.sh"
    chown "$SERVICE_USER":"$SERVICE_USER" "$INSTALL_DIR/enroll-server.sh"
    mkdir -p "$CONFIG_DIR/tokens"
    chown "$SERVICE_USER":"$SERVICE_USER" "$CONFIG_DIR/tokens"

    cat > "/etc/systemd/system/${SERVICE_NAME}-enroll.service" << EOF
[Unit]
Description=007 Bond — Client Enrollment Service
After=${SERVICE_NAME}.service
Requires=${SERVICE_NAME}.service

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
EnvironmentFile=$CONFIG_DIR/.env
Environment=ENROLL_PORT=8017
ExecStart=$INSTALL_DIR/enroll-server.sh

AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN

Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=007-enroll

NoNewPrivileges=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=$CONFIG_DIR/tokens /var/run/wireguard

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}-enroll" > /dev/null 2>&1
    systemctl start "${SERVICE_NAME}-enroll" 2>/dev/null || true
    ok "Enrollment service on port 8017"
fi

# ─── Firewall ────────────────────────────────────────────────────────────
if command -v ufw > /dev/null 2>&1; then
    info "Configuring firewall..."
    ufw allow ssh > /dev/null 2>&1 || true
    ufw allow "$LISTEN_PORT/udp" comment "007 WireGuard" > /dev/null 2>&1 || true

    # Allow API from tunnel only
    ufw allow from 10.7.0.0/24 to any port 8007 proto tcp comment "007 API (tunnel only)" > /dev/null 2>&1 || true
    # Enrollment port — open to all (token-authenticated, one-time use)
    ufw allow 8017/tcp comment "007 enrollment" > /dev/null 2>&1 || true

    if ! ufw status | grep -q "Status: active"; then
        ufw --force enable > /dev/null 2>&1
    fi
    ok "Firewall configured (UDP $LISTEN_PORT, TCP 8007 tunnel-only)"
fi

# ─── Install CLI ─────────────────────────────────────────────────────────
info "Installing management CLI..."
curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/007-cli.sh" -o /usr/local/bin/007-bond 2>/dev/null \
    || cp "$(dirname "$0")/007-cli.sh" /usr/local/bin/007-bond 2>/dev/null \
    || warn "Could not install CLI"
if [[ -f /usr/local/bin/007-bond ]]; then
    chmod +x /usr/local/bin/007-bond
    ok "CLI installed: 007-bond (run '007-bond help')"
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
echo "           007 Bond — Multi-Path Bonding Server"
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
echo "  Client Enrollment:"
echo "    sudo 007-bond enroll-token      Generate one-time client token"
echo "    sudo 007-bond list-tokens       Show pending tokens"
echo "    sudo 007-bond revoke-token <t>  Revoke a token"
echo ""
echo "  Maintenance:"
echo "    sudo 007-bond add-client <key>  Add WireGuard peer manually"
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
info "Enabling and starting $SERVICE_NAME..."
systemctl enable "$SERVICE_NAME" > /dev/null 2>&1
systemctl start "$SERVICE_NAME"

# Wait for startup
sleep 3

if systemctl is-active --quiet "$SERVICE_NAME"; then
    ok "Service running"
else
    err "Service failed to start"
    journalctl -u "$SERVICE_NAME" --no-pager -n 20
    exit 1
fi

# Verify WireGuard
if wg show "$INTERFACE" > /dev/null 2>&1; then
    ok "WireGuard interface $INTERFACE active"
else
    warn "WireGuard not yet configured (waiting for first client)"
fi

# ─── Print summary ───────────────────────────────────────────────────────
SERVER_IP=$(hostname -I | awk '{print $1}')
SERVER_PUB=$(cat "$CONFIG_DIR/server.pub")
CLIENT_KEY=$(cat "$CONFIG_DIR/client.key")

echo ""
echo -e "${BOLD}================================================================${NC}"
echo -e "${BOLD}  007 Bond Server${NC}"
echo -e "${BOLD}================================================================${NC}"
echo ""
echo -e "  Version:    $INSTALLED_VERSION"
echo -e "  Interface:  $INTERFACE"
echo -e "  Tunnel IP:  ${TUNNEL_IP%/*}"
echo -e "  Listen:     $SERVER_IP:$LISTEN_PORT"
echo -e "  API:        http://${API_ADDR}/api/stats"
echo -e "  Service:    systemctl status $SERVICE_NAME"
echo -e "  Logs:       journalctl -u $SERVICE_NAME -f"
echo ""
echo -e "  ${BOLD}Client connection command:${NC}"
echo ""
echo -e "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/deploy/install-007-client.sh | \\"
echo -e "    sudo SERVER_IP=$SERVER_IP SERVER_PUB=$SERVER_PUB CLIENT_KEY=$CLIENT_KEY bash"
echo ""
echo -e "${BOLD}================================================================${NC}"

if [[ "$UPGRADE" == "true" ]]; then
    ok "Upgrade complete"
else
    ok "Installation complete"
fi
