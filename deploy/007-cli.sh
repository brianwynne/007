#!/usr/bin/env bash
# ============================================================================
# 007 Bond — Management CLI
# ============================================================================
# Installed to /usr/local/bin/007-bond
#
# Usage:
#   sudo 007-bond status
#   sudo 007-bond logs
#   sudo 007-bond restart
#   sudo 007-bond stats
#   sudo 007-bond paths
#   sudo 007-bond add-client <client_pub_key>
#   sudo 007-bond upgrade [--tag vX.Y.Z]
#   sudo 007-bond version
#   sudo 007-bond uninstall
#
# ============================================================================
set -euo pipefail

CONFIG_DIR="/etc/007"
INSTALL_DIR="/opt/007"
SERVICE_NAME="007-bond"
REPO="brianwynne/007"

# ─── Colours ──────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
err()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

require_root() {
    [[ "$(id -u)" -eq 0 ]] || { err "Must run as root"; exit 1; }
}

# Load config
load_env() {
    if [[ -f "$CONFIG_DIR/.env" ]]; then
        source "$CONFIG_DIR/.env"
    fi
}

# ─── Commands ─────────────────────────────────────────────────────────────

cmd_status() {
    echo -e "${BOLD}007 Bond Status${NC}"
    echo ""

    # Service
    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        echo -e "  Service:  ${GREEN}running${NC}"
    else
        echo -e "  Service:  ${RED}stopped${NC}"
    fi

    # Version
    if [[ -x "$INSTALL_DIR/007" ]]; then
        local ver
        ver=$("$INSTALL_DIR/007" --version 2>&1 || echo "unknown")
        echo "  Version:  $ver"
    fi

    # Interface
    if ip link show bond0 > /dev/null 2>&1; then
        echo -e "  Interface: ${GREEN}bond0 up${NC}"
        local ip
        ip=$(ip -4 addr show bond0 2>/dev/null | grep inet | awk '{print $2}' | head -1)
        echo "  Tunnel IP: ${ip:-not assigned}"
    else
        echo -e "  Interface: ${RED}bond0 not found${NC}"
    fi

    # WireGuard
    if wg show bond0 > /dev/null 2>&1; then
        local peers
        peers=$(wg show bond0 peers | wc -l)
        echo "  Peers:    $peers"
        local latest
        latest=$(wg show bond0 latest-handshakes | awk '{print $2}' | sort -rn | head -1)
        if [[ -n "$latest" && "$latest" != "0" ]]; then
            local ago=$(( $(date +%s) - latest ))
            echo "  Handshake: ${ago}s ago"
        fi
    fi

    # API health
    load_env
    local api_addr="${BOND_API:-127.0.0.1:8007}"
    if curl -s --max-time 2 "http://${api_addr}/api/health" | grep -q ok 2>/dev/null; then
        echo -e "  API:      ${GREEN}healthy${NC} (http://${api_addr})"
    else
        echo -e "  API:      ${RED}unreachable${NC}"
    fi

    echo ""
}

cmd_stats() {
    load_env
    local api_addr="${BOND_API:-127.0.0.1:8007}"
    curl -s --max-time 3 "http://${api_addr}/api/stats" | python3 -m json.tool 2>/dev/null || err "Stats unavailable"
}

cmd_paths() {
    load_env
    local api_addr="${BOND_API:-127.0.0.1:8007}"
    curl -s --max-time 3 "http://${api_addr}/api/paths" | python3 -m json.tool 2>/dev/null || err "Paths unavailable"
}

cmd_logs() {
    journalctl -u "$SERVICE_NAME" -f --no-pager
}

cmd_restart() {
    require_root
    info "Restarting $SERVICE_NAME..."
    systemctl restart "$SERVICE_NAME"
    sleep 2
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        ok "Service restarted"
    else
        err "Service failed to restart"
        journalctl -u "$SERVICE_NAME" --no-pager -n 10
    fi
}

cmd_stop() {
    require_root
    systemctl stop "$SERVICE_NAME"
    ok "Service stopped"
}

cmd_start() {
    require_root
    systemctl start "$SERVICE_NAME"
    sleep 2
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        ok "Service started"
    else
        err "Service failed to start"
        journalctl -u "$SERVICE_NAME" --no-pager -n 10
    fi
}

cmd_version() {
    if [[ -x "$INSTALL_DIR/007" ]]; then
        "$INSTALL_DIR/007" --version 2>&1
    else
        err "007 not installed"
    fi
}

cmd_add_client() {
    require_root
    local client_pub="${1:-}"
    if [[ -z "$client_pub" ]]; then
        err "Usage: 007-bond add-client <client_public_key>"
        exit 1
    fi

    load_env
    local iface="${INTERFACE:-bond0}"

    wg set "$iface" peer "$client_pub" allowed-ips "10.7.0.2/32"
    ok "Client added: $client_pub"
    echo "  Allowed IPs: 10.7.0.2/32"
}

cmd_upgrade() {
    require_root
    local tag=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --tag) tag="$2"; shift 2 ;;
            *)     shift ;;
        esac
    done

    info "Downloading installer..."
    local installer
    installer=$(mktemp /tmp/007-install-XXXXXX.sh)
    curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/install-007-server.sh" -o "$installer"
    chmod +x "$installer"

    if [[ -n "$tag" ]]; then
        bash "$installer" --tag "$tag"
    else
        bash "$installer"
    fi
    rm -f "$installer"
}

cmd_uninstall() {
    require_root
    echo -e "${YELLOW}This will remove 007 Bond server.${NC}"
    echo "Configuration and data will be preserved in $CONFIG_DIR and /var/lib/007/"
    read -rp "Continue? [y/N] " confirm
    [[ "$confirm" =~ ^[Yy]$ ]] || { info "Cancelled"; exit 0; }

    info "Stopping and disabling service..."
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
    rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
    systemctl daemon-reload

    info "Removing binary..."
    rm -rf "$INSTALL_DIR"

    info "Removing CLI..."
    rm -f /usr/local/bin/007-bond

    info "Removing logrotate..."
    rm -f /etc/logrotate.d/007-bond

    # Clean up WireGuard interface
    ip link del bond0 2>/dev/null || true

    ok "007 Bond removed"
    echo "  Config preserved: $CONFIG_DIR/"
    echo "  Data preserved:   /var/lib/007/"
    echo "  Logs preserved:   /var/log/007/"
    echo "  To fully remove: sudo rm -rf $CONFIG_DIR /var/lib/007 /var/log/007"
}

cmd_enroll_token() {
    require_root
    local tunnel_ip="${1:-}"
    local token_dir="$CONFIG_DIR/tokens"
    mkdir -p "$token_dir"

    # Auto-assign tunnel IP if not specified
    if [[ -z "$tunnel_ip" ]]; then
        # Find next available IP in 10.7.0.0/24 range
        # .1 = server, .2+ = clients
        local max_ip=1
        # Check existing WireGuard peers
        for ip in $(wg show bond0 allowed-ips 2>/dev/null | grep -oP '10\.7\.0\.\K\d+'); do
            [[ "$ip" -gt "$max_ip" ]] && max_ip="$ip"
        done
        # Check existing tokens
        for tf in "$token_dir"/*; do
            [[ -f "$tf" ]] || continue
            local tip
            tip=$(cat "$tf" 2>/dev/null)
            local octet
            octet=$(echo "$tip" | grep -oP '10\.7\.0\.\K\d+' || true)
            [[ -n "$octet" && "$octet" -gt "$max_ip" ]] && max_ip="$octet"
        done
        tunnel_ip="10.7.0.$((max_ip + 1))"
    fi

    # Generate token
    local token
    token=$(head -c 16 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)

    # Save token with allocated IP
    echo "$tunnel_ip" > "$token_dir/$token"
    chmod 600 "$token_dir/$token"
    chown bond007:bond007 "$token_dir/$token" 2>/dev/null || true

    local server_ip
    server_ip=$(hostname -I | awk '{print $1}')

    echo ""
    echo -e "${BOLD}Enrollment token generated${NC}"
    echo ""
    echo -e "  Token:     ${GREEN}$token${NC}"
    echo -e "  Tunnel IP: $tunnel_ip"
    echo -e "  Expires:   on first use (one-time)"
    echo ""
    echo -e "  ${BOLD}Client install command:${NC}"
    echo ""
    echo "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/deploy/install-007-client.sh | \\"
    echo "    sudo ENROLL_URL=http://${server_ip}:8017 ENROLL_TOKEN=$token bash"
    echo ""
}

cmd_list_tokens() {
    local token_dir="$CONFIG_DIR/tokens"
    if [[ ! -d "$token_dir" ]] || [[ -z "$(ls -A "$token_dir" 2>/dev/null)" ]]; then
        info "No pending enrollment tokens"
        return
    fi
    echo -e "${BOLD}Pending enrollment tokens:${NC}"
    for tf in "$token_dir"/*; do
        [[ -f "$tf" ]] || continue
        local token
        token=$(basename "$tf")
        local ip
        ip=$(cat "$tf")
        echo "  $token → $ip"
    done
}

cmd_revoke_token() {
    require_root
    local token="${1:-}"
    [[ -n "$token" ]] || { err "Usage: 007-bond revoke-token <token>"; exit 1; }
    local token_file="$CONFIG_DIR/tokens/$token"
    if [[ -f "$token_file" ]]; then
        rm -f "$token_file"
        ok "Token revoked: $token"
    else
        err "Token not found: $token"
    fi
}

cmd_help() {
    echo "Usage: 007-bond <command> [args]"
    echo ""
    echo "Commands:"
    echo "  status              Show service status, interface, peers"
    echo "  stats               Show bond statistics (FEC, ARQ, paths)"
    echo "  paths               Show per-path health (RTT, loss, jitter)"
    echo "  logs                Tail service logs"
    echo "  start               Start the service"
    echo "  stop                Stop the service"
    echo "  restart             Restart the service"
    echo "  add-client <key>    Add a WireGuard client peer"
    echo "  enroll-token [ip]   Generate one-time enrollment token for a client"
    echo "  list-tokens         Show pending enrollment tokens"
    echo "  revoke-token <tok>  Revoke an unused enrollment token"
    echo "  upgrade [--tag v]   Upgrade to latest or specific version"
    echo "  version             Show installed version"
    echo "  uninstall           Remove 007 Bond (preserves config/data)"
    echo "  help                Show this help"
}

# ─── Dispatch ─────────────────────────────────────────────────────────────
case "${1:-help}" in
    status)       cmd_status ;;
    stats)        cmd_stats ;;
    paths)        cmd_paths ;;
    logs)         cmd_logs ;;
    start)        cmd_start ;;
    stop)         cmd_stop ;;
    restart)      cmd_restart ;;
    add-client)   shift; cmd_add_client "$@" ;;
    enroll-token) shift; cmd_enroll_token "$@" ;;
    list-tokens)  cmd_list_tokens ;;
    revoke-token) shift; cmd_revoke_token "$@" ;;
    upgrade)      shift; cmd_upgrade "$@" ;;
    version)      cmd_version ;;
    uninstall)    cmd_uninstall ;;
    help|--help|-h) cmd_help ;;
    *)          err "Unknown command: $1"; cmd_help; exit 1 ;;
esac
