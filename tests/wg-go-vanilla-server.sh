#!/usr/bin/env bash
# Vanilla wireguard-go test — NO 007 bond code, NO modifications
# Tests if upstream wireguard-go works on this kernel
#
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/wg-go-vanilla-server.sh | sudo bash
set -euo pipefail

echo "[+] Cleaning up..."
pkill -9 -f 'wireguard-go|007' 2>/dev/null || true
sleep 2
rm -f /var/run/wireguard/*.sock
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done
ip link del wg0 2>/dev/null || true
fuser -k 51820/udp 2>/dev/null || true

echo "[+] Installing..."
apt-get update -qq && apt-get install -y -qq wireguard-tools golang-go git > /dev/null 2>&1

echo "[+] Building upstream wireguard-go..."
cd /tmp
rm -rf /tmp/wireguard-go-test
git clone --depth 1 https://git.zx2c4.com/wireguard-go /tmp/wireguard-go-test
cd /tmp/wireguard-go-test
go build -o /tmp/wg-go .

echo "[+] Generating keys..."
mkdir -p /tmp/wg-test
cd /tmp/wg-test
wg genkey | tee server.key | wg pubkey > server.pub
wg genkey | tee client.key | wg pubkey > client.pub

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
wg set wg0 listen-port 51820 private-key ./server.key \
    peer "$(cat client.pub)" allowed-ips 10.7.0.2/32
ip addr add 10.7.0.1/24 dev wg0 2>/dev/null || true
ip link set wg0 up
iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true

SERVER_IP=$(hostname -I | awk '{print $1}')

echo ""
echo "========================================================"
echo "  VANILLA wireguard-go SERVER on $SERVER_IP:51820"
echo "  Kernel: $(uname -r)"
echo "  Binary: upstream wireguard-go (no 007 modifications)"
echo ""
echo "  Run on CLIENT:"
echo "  curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/wg-go-vanilla-client.sh | sudo bash -s -- $SERVER_IP $(cat server.pub) $(cat client.key)"
echo "========================================================"
echo ""
wg show wg0
echo ""
echo "Log: tail -f /tmp/wg-go.log"
