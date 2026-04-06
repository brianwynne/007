#!/usr/bin/env bash
# 007 Bond — Add bond paths to an already-running 007 client
# Detects all network interfaces and adds them as bond paths
#
# Usage: sudo bash 007-add-bond-paths.sh <server_ip>
set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Usage: sudo bash $0 <server_ip>"
    exit 1
fi

SERVER_IP="$1"

if [ "$(id -u)" -ne 0 ]; then
    echo "Must run as root"
    exit 1
fi

if ! wg show bond0 > /dev/null 2>&1; then
    echo "[X] bond0 not running. Start 007 first."
    exit 1
fi

SERVER_PUB_HEX=$(cat /opt/007/server.pub | base64 -d | xxd -p -c 32)

UAPI_CMD="set=1\npublic_key=${SERVER_PUB_HEX}\n"
COUNT=0
echo "[+] Detecting interfaces..."
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "bond0" ] && continue
    LOCAL_IP=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [ -n "$LOCAL_IP" ]; then
        echo "  Bond path: $iface ($LOCAL_IP) → $SERVER_IP:51820"
        UAPI_CMD="${UAPI_CMD}bond_endpoint=${SERVER_IP}:51820@${LOCAL_IP}\n"
        COUNT=$((COUNT + 1))
    fi
done

if [ "$COUNT" -eq 0 ]; then
    echo "[X] No interfaces found"
    exit 1
fi

echo "[+] Configuring $COUNT bond paths via UAPI..."
printf "${UAPI_CMD}\n" | nc -U /var/run/wireguard/bond0.sock -w 1
echo "[+] Bond paths configured"

echo ""
echo "[+] Testing multi-path traffic..."
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "bond0" ] && continue
    tcpdump -i "$iface" udp port 51820 -c 3 -n -q 2>/dev/null &
done
sleep 1
ping -c 10 -i 0.2 10.7.0.1 > /dev/null 2>&1
sleep 2
wait 2>/dev/null

echo ""
echo "[+] Bond stats:"
curl -s --max-time 3 http://127.0.0.1:8007/api/stats | python3 -m json.tool 2>/dev/null || echo "(unavailable)"
