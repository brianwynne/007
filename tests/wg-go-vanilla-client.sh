#!/usr/bin/env bash
# Vanilla wireguard-go test — NO 007 bond code, NO modifications
# Tests if upstream wireguard-go works on this kernel
#
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/wg-go-vanilla-client.sh | sudo bash -s -- <server_ip> <server_pub> <client_key>
set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: sudo bash $0 <server_ip> <server_pub_key> <client_private_key>"
    exit 1
fi

SERVER_IP="$1"
SERVER_PUB="$2"
CLIENT_KEY="$3"

echo "[+] Cleaning up..."
pkill -9 -f 'wireguard-go|007' 2>/dev/null || true
sleep 2
rm -f /var/run/wireguard/*.sock
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done
ip link del wg0 2>/dev/null || true

echo "[+] Installing..."
apt-get update -qq && apt-get install -y -qq wireguard-tools golang-go git > /dev/null 2>&1

echo "[+] Building upstream wireguard-go..."
cd /tmp
rm -rf /tmp/wireguard-go-test
git clone --depth 1 https://git.zx2c4.com/wireguard-go /tmp/wireguard-go-test
cd /tmp/wireguard-go-test
go build -o /tmp/wg-go .

echo "[+] Setting up keys..."
mkdir -p /tmp/wg-test
cd /tmp/wg-test
echo "$CLIENT_KEY" > client.key
echo "$SERVER_PUB" > server.pub

echo "[+] Starting vanilla wireguard-go..."
WG_PROCESS_FOREGROUND=1 /tmp/wg-go wg0 > /tmp/wg-go.log 2>&1 &
echo $! > /tmp/wg-go.pid
sleep 2

if ! ip link show wg0 > /dev/null 2>&1; then
    echo "[X] wg0 not created. Log:"
    cat /tmp/wg-go.log
    exit 1
fi

echo "[+] Configuring..."
wg set wg0 private-key ./client.key \
    peer "$SERVER_PUB" endpoint "$SERVER_IP:51820" \
    allowed-ips 10.7.0.0/24 persistent-keepalive 25
ip addr add 10.7.0.2/24 dev wg0 2>/dev/null || true
ip link set wg0 up

echo "[+] Waiting for handshake..."
sleep 5

echo ""
echo "=== WireGuard Status ==="
wg show wg0
echo ""

echo "=== Ping test ==="
if ping -c 5 -W 2 10.7.0.1; then
    echo ""
    echo "[OK] VANILLA wireguard-go WORKS on kernel $(uname -r)"
    echo "[OK] The TUN fd issue is specific to our 007 modifications"
else
    echo ""
    echo "[FAIL] Even VANILLA wireguard-go fails on kernel $(uname -r)"
    echo "[FAIL] This is an upstream wireguard-go issue, not 007"
    echo ""
    echo "--- Log ---"
    tail -20 /tmp/wg-go.log
    echo ""
    echo "--- Transfer ---"
    wg show wg0 | grep transfer
fi

echo ""
echo "=== Cleanup ==="
kill $(cat /tmp/wg-go.pid 2>/dev/null) 2>/dev/null || true
ip link del wg0 2>/dev/null || true
echo "[+] Done"
