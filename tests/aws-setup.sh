#!/usr/bin/env bash
# 007 Bond — AWS Setup Script
#
# Usage:
#   Server: sudo bash aws-setup.sh server
#   Client: sudo bash aws-setup.sh client <server_private_ip> <server_pub_key> <client_private_key>
#
# The server generates keys and prints the command to run on the client.
#
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
fail()  { echo -e "${RED}[X]${NC} $*"; exit 1; }
pass()  { echo -e "${GREEN}[✓]${NC} $*"; }

BOND_DIR="/opt/007"
BINARY="$BOND_DIR/007"
VERSION="v0.1.0"

install_binary() {
    info "Installing 007 Bond $VERSION..."
    mkdir -p "$BOND_DIR"
    apt-get update -qq && apt-get install -y -qq wireguard-tools xxd netcat-openbsd > /dev/null
    ARCH=$(dpkg --print-architecture)
    case "$ARCH" in
        amd64) SUFFIX="linux-amd64" ;;
        arm64) SUFFIX="linux-arm64" ;;
        armhf) SUFFIX="linux-arm" ;;
        *) fail "Unsupported architecture: $ARCH" ;;
    esac
    curl -fsSL "https://github.com/brianwynne/007/releases/download/$VERSION/007-$SUFFIX" -o "$BINARY"
    chmod +x "$BINARY"
    info "Installed: $BINARY ($(du -h "$BINARY" | cut -f1))"
}

cleanup_old() {
    killall 007 2>/dev/null || true
    ip link del bond0 2>/dev/null || true
    sleep 1
}

setup_server() {
    install_binary
    cleanup_old

    info "Generating keys..."
    cd "$BOND_DIR"
    wg genkey | tee server.key | wg pubkey > server.pub
    wg genkey | tee client.key | wg pubkey > client.pub

    info "Starting 007 server..."
    WG_NO_VNET_HDR=1 "$BINARY" -f bond0 > /tmp/007-server.log 2>&1 &
    sleep 2

    wg set bond0 listen-port 51820 private-key ./server.key \
        peer "$(cat client.pub)" allowed-ips 10.7.0.2/32
    ip addr add 10.7.0.1/24 dev bond0
    ip link set bond0 up
    iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true

    SERVER_IP=$(hostname -I | awk '{print $1}')
    SERVER_PUB=$(cat server.pub)
    CLIENT_KEY=$(cat client.key)

    pass "Server running on $SERVER_IP:51820"
    echo ""
    echo "════════════════════════════════════════════════════════════════"
    echo "  Run this on the CLIENT instance:"
    echo ""
    echo "  curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-setup.sh | sudo bash -s client $SERVER_IP $SERVER_PUB $CLIENT_KEY"
    echo "════════════════════════════════════════════════════════════════"
    echo ""
    wg show bond0
}

setup_client() {
    local SERVER_IP="$1"
    local SERVER_PUB="$2"
    local CLIENT_KEY="$3"

    install_binary
    cleanup_old

    cd "$BOND_DIR"
    echo "$CLIENT_KEY" > client.key
    echo "$SERVER_PUB" > server.pub
    wg pubkey < client.key > client.pub

    info "Starting 007 client..."
    WG_NO_VNET_HDR=1 "$BINARY" -f bond0 > /tmp/007-client.log 2>&1 &
    sleep 2

    wg set bond0 private-key ./client.key \
        peer "$SERVER_PUB" endpoint "$SERVER_IP:51820" \
        allowed-ips 10.7.0.0/24 persistent-keepalive 25
    ip addr add 10.7.0.2/24 dev bond0
    ip link set bond0 up

    # Detect interfaces and add bond paths
    info "Detecting network interfaces..."
    SERVER_PUB_HEX=$(echo -n "$SERVER_PUB" | base64 -d | xxd -p -c 32)
    BOND_CMD="set=1\npublic_key=$SERVER_PUB_HEX\n"
    PATH_COUNT=0

    for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
        if [ "$iface" = "bond0" ]; then continue; fi
        LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
        if [ -n "$LOCAL_IP" ]; then
            BOND_CMD="${BOND_CMD}bond_endpoint=${SERVER_IP}:51820@${LOCAL_IP}\n"
            info "  Bond path: $iface ($LOCAL_IP)"
            PATH_COUNT=$((PATH_COUNT + 1))
        fi
    done

    if [ "$PATH_COUNT" -gt 0 ]; then
        printf "${BOND_CMD}\n" | nc -U /var/run/wireguard/bond0.sock -w 1 > /tmp/007-uapi.txt 2>&1 || true
        if grep -q "errno=0" /tmp/007-uapi.txt 2>/dev/null; then
            pass "$PATH_COUNT bond path(s) configured"
        else
            warn "Bond endpoint config: $(cat /tmp/007-uapi.txt 2>/dev/null)"
        fi
    fi

    info "Waiting for handshake..."
    sleep 3

    # ─── Tests ───────────────────────────────────────────────────
    echo ""
    info "═══ TEST 1: Ping through tunnel ═══"
    if ping -c 5 -W 2 10.7.0.1 > /tmp/007-ping.txt 2>&1; then
        pass "Tunnel works!"
        grep "rtt" /tmp/007-ping.txt
    else
        fail "Ping failed"
        cat /tmp/007-ping.txt
        echo ""
        wg show bond0
        echo ""
        warn "Log: /tmp/007-client.log"
        exit 1
    fi

    if [ "$PATH_COUNT" -gt 1 ]; then
        echo ""
        info "═══ TEST 2: Multi-path verification ═══"
        for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
            if [ "$iface" = "bond0" ]; then continue; fi
            COUNT=$(timeout 5 tcpdump -i "$iface" udp port 51820 -c 3 -q 2>/dev/null | grep -c UDP || echo 0)
            if [ "$COUNT" -gt 0 ]; then
                pass "  $iface: $COUNT packets"
            else
                warn "  $iface: no traffic"
            fi
        done

        echo ""
        info "═══ TEST 3: Path failover ═══"
        SECOND_IFACE=$(ip -4 -o addr show scope global | awk '{print $2}' | sort -u | grep -v bond0 | tail -1)
        if [ -n "$SECOND_IFACE" ]; then
            ping -c 10 -i 0.5 10.7.0.1 > /tmp/007-failover.txt 2>&1 &
            PING_PID=$!
            sleep 2
            info "  Disabling $SECOND_IFACE..."
            ip link set "$SECOND_IFACE" down
            sleep 3
            info "  Re-enabling $SECOND_IFACE..."
            ip link set "$SECOND_IFACE" up
            wait $PING_PID 2>/dev/null || true
            RX=$(grep -c "bytes from" /tmp/007-failover.txt 2>/dev/null || echo 0)
            pass "  Failover: $RX/10 pings received during interface toggle"
        fi
    fi

    echo ""
    info "═══ TEST 4: Bond stats ═══"
    STATS=$(curl -s --max-time 3 http://127.0.0.1:8007/api/stats 2>/dev/null || echo "unavailable")
    echo "$STATS" | python3 -m json.tool 2>/dev/null || echo "$STATS"

    echo ""
    info "═══ DONE ═══"
    wg show bond0
    echo ""
    info "Logs: /tmp/007-client.log"
    info "Stats: curl -s http://127.0.0.1:8007/api/stats | python3 -m json.tool"
}

# ─── Main ────────────────────────────────────────────────────────
if [ "$(id -u)" -ne 0 ]; then
    fail "Must run as root: sudo bash $0 ..."
fi

case "${1:-}" in
    server)
        setup_server
        ;;
    client)
        if [ $# -lt 4 ]; then
            fail "Usage: sudo bash $0 client <server_ip> <server_pub_key> <client_private_key>"
        fi
        setup_client "$2" "$3" "$4"
        ;;
    *)
        echo "Usage:"
        echo "  Server: sudo bash $0 server"
        echo "  Client: sudo bash $0 client <server_ip> <server_pub> <client_key>"
        exit 1
        ;;
esac
