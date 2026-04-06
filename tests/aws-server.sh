#!/usr/bin/env bash
# 007 Bond — AWS Server Setup
# Usage: curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-server.sh | sudo bash
set -euo pipefail

BOND_DIR="/opt/007"
VERSION="v0.1.0"

echo "[+] Cleaning up any running instances..."
killall 007 007-bond 2>/dev/null || true
sleep 1
rm -f /var/run/wireguard/*.sock
ip link del bond0 2>/dev/null || true
rm -f "$BOND_DIR/007"

echo "[+] Installing 007 Bond server..."
mkdir -p "$BOND_DIR"
apt-get update -qq && apt-get install -y -qq wireguard-tools xxd netcat-openbsd > /dev/null 2>&1

ARCH=$(dpkg --print-architecture)
case "$ARCH" in
    amd64) SUFFIX="linux-amd64" ;;
    arm64) SUFFIX="linux-arm64" ;;
    *) echo "[X] Unsupported: $ARCH"; exit 1 ;;
esac
curl -fsSL "https://github.com/brianwynne/007/releases/download/$VERSION/007-$SUFFIX" -o "$BOND_DIR/007"
chmod +x "$BOND_DIR/007"

echo "[+] Cleaning up old instances..."
killall 007 2>/dev/null || true
sleep 1
rm -f /var/run/wireguard/bond0.sock
ip link del bond0 2>/dev/null || true

echo "[+] Generating keys..."
cd "$BOND_DIR"
wg genkey | tee server.key | wg pubkey > server.pub
wg genkey | tee client.key | wg pubkey > client.pub

echo "[+] Starting 007..."
WG_NO_VNET_HDR=1 WG_TUN_BLOCKING=1 "$BOND_DIR/007" -f bond0 > /tmp/007-server.log 2>&1 &
sleep 2

echo "[+] Configuring WireGuard..."
wg set bond0 listen-port 51820 private-key ./server.key \
    peer "$(cat client.pub)" allowed-ips 10.7.0.2/32
ip addr add 10.7.0.1/24 dev bond0 2>/dev/null || true
ip link set bond0 up
iptables -I INPUT -p udp --dport 51820 -j ACCEPT 2>/dev/null || true

SERVER_IP=$(hostname -I | awk '{print $1}')

echo ""
echo "========================================================"
echo "  SERVER RUNNING on $SERVER_IP:51820"
echo ""
echo "  Run this on the CLIENT:"
echo ""
echo "  curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/tests/aws-client.sh | sudo bash -s -- $SERVER_IP $(cat server.pub) $(cat client.key)"
echo "========================================================"
echo ""
wg show bond0
