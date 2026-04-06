#!/usr/bin/env bash
# 007 Bond Proxy — Server Setup
# Uses kernel WireGuard for crypto + 007 proxy for FEC/multi-path
#
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/proxy-server.sh | sudo bash
set -euo pipefail

echo "[+] Cleaning up..."
killall 007-proxy 2>/dev/null || true
sleep 1
for iface in $(wg show interfaces 2>/dev/null); do ip link del "$iface" 2>/dev/null || true; done

echo "[+] Installing..."
apt-get update -qq && apt-get install -y -qq wireguard-tools > /dev/null 2>&1

mkdir -p /opt/007
cd /opt/007

# Build proxy from source (until release includes it)
if ! command -v go &>/dev/null; then
    echo "[+] Installing Go..."
    apt-get install -y -qq golang-go > /dev/null 2>&1
fi
rm -rf /opt/007/src
git clone --depth 1 https://github.com/brianwynne/007.git /opt/007/src
cd /opt/007/src
go build -o /opt/007/007-proxy ./cmd/007-proxy/
cd /opt/007

echo "[+] Generating keys..."
wg genkey | tee server.key | wg pubkey > server.pub
wg genkey | tee client.key | wg pubkey > client.pub

echo "[+] Setting up kernel WireGuard..."
ip link add wg0 type wireguard
wg set wg0 listen-port 51820 private-key ./server.key \
    peer "$(cat client.pub)" allowed-ips 10.7.0.2/32 \
    endpoint 127.0.0.1:51821
ip addr add 10.7.0.1/24 dev wg0
ip link set wg0 up
iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true

echo "[+] Starting 007 proxy..."
iptables -I INPUT -p udp --dport 51822 -j ACCEPT 2>/dev/null || true

# Detect server's primary interface for path binding
SERVER_IFACE=$(ip -4 -o addr show scope global | awk '{print $2}' | head -1)
SERVER_BIND_IP=$(ip -4 addr show "$SERVER_IFACE" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
echo "[+] Binding proxy path to $SERVER_IFACE ($SERVER_BIND_IP)"

./007-proxy \
    --wg-listen 127.0.0.1:51821 \
    --wg-forward 127.0.0.1:51820 \
    --listen-port 51822 \
    --path "$SERVER_IFACE=$SERVER_BIND_IP" \
    --api 127.0.0.1:8007 \
    > /tmp/007-proxy.log 2>&1 &
sleep 1

SERVER_IP=$(hostname -I | awk '{print $1}')
echo ""
echo "========================================================"
echo "  SERVER RUNNING"
echo "  WireGuard: wg0 on port 51820"
echo "  Proxy: listening on 127.0.0.1:51821"
echo ""
echo "  Run on CLIENT:"
echo "  curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/proxy-client.sh | sudo bash -s -- $SERVER_IP $(cat server.pub) $(cat client.key)"
echo "========================================================"
echo ""
wg show wg0
echo ""
echo "Ping from server to verify kernel WG:"
ping -c 1 -W 2 10.7.0.2 2>&1 || echo "(client not connected yet)"
