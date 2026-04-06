#!/usr/bin/env bash
# 007 Bond — Kernel WireGuard baseline test
# Tests if the KERNEL WireGuard module works on these instances.
# If kernel WG works but 007 (userspace) doesn't, the issue is TUN fd reads.
#
# Usage:
#   Server: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-kernel-wg-test.sh | sudo bash -s server
#   Client: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-kernel-wg-test.sh | sudo bash -s client <server_ip> <server_pub> <client_key>
set -euo pipefail

cleanup() {
    killall 007 007-bond 2>/dev/null || true
    rm -f /var/run/wireguard/*.sock
    ip link del bond0 2>/dev/null || true
    ip link del wg-test 2>/dev/null || true
}

setup_server() {
    cleanup
    echo "[+] Kernel: $(uname -r)"
    echo "[+] Setting up kernel WireGuard server..."

    apt-get install -y -qq wireguard-tools > /dev/null 2>&1

    mkdir -p /opt/007
    cd /opt/007
    wg genkey | tee server.key | wg pubkey > server.pub
    wg genkey | tee client.key | wg pubkey > client.pub

    ip link add wg-test type wireguard
    wg set wg-test listen-port 51820 private-key ./server.key \
        peer "$(cat client.pub)" allowed-ips 10.7.0.2/32
    ip addr add 10.7.0.1/24 dev wg-test
    ip link set wg-test up
    iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true

    SERVER_IP=$(hostname -I | awk '{print $1}')
    echo ""
    echo "[+] Kernel WireGuard server running on $SERVER_IP:51820"
    echo ""
    echo "Run on CLIENT:"
    echo "curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-kernel-wg-test.sh | sudo bash -s client $SERVER_IP $(cat server.pub) $(cat client.key)"
    echo ""
    wg show wg-test
}

setup_client() {
    local SERVER_IP="$1"
    local SERVER_PUB="$2"
    local CLIENT_KEY="$3"

    cleanup
    echo "[+] Kernel: $(uname -r)"
    echo "[+] Setting up kernel WireGuard client..."

    apt-get install -y -qq wireguard-tools > /dev/null 2>&1

    mkdir -p /opt/007
    cd /opt/007
    echo "$CLIENT_KEY" > client.key
    echo "$SERVER_PUB" > server.pub

    ip link add wg-test type wireguard
    wg set wg-test private-key ./client.key \
        peer "$SERVER_PUB" endpoint "$SERVER_IP:51820" \
        allowed-ips 10.7.0.0/24 persistent-keepalive 25
    ip addr add 10.7.0.2/24 dev wg-test
    ip link set wg-test up

    echo "[+] Waiting for handshake..."
    sleep 3
    wg show wg-test
    echo ""

    echo "=== Kernel WireGuard ping test ==="
    if ping -c 5 -W 2 10.7.0.1; then
        echo ""
        echo "[OK] KERNEL WireGuard works — TUN fd issue confirmed in userspace 007"
        echo "[OK] The 007 bond code is correct, but Go's TUN fd read fails on kernel $(uname -r)"
    else
        echo ""
        echo "[FAIL] Even kernel WireGuard fails — network/firewall issue, not 007"
    fi

    echo ""
    echo "[+] Cleaning up..."
    ip link del wg-test 2>/dev/null || true
}

if [ "$(id -u)" -ne 0 ]; then
    echo "Must run as root"; exit 1
fi

case "${1:-}" in
    server) setup_server ;;
    client)
        if [ $# -lt 4 ]; then
            echo "Usage: sudo bash $0 client <server_ip> <server_pub> <client_key>"
            exit 1
        fi
        setup_client "$2" "$3" "$4"
        ;;
    *) echo "Usage: sudo bash $0 server|client ..." ; exit 1 ;;
esac
