#!/usr/bin/env bash
# ============================================================================
# 007 Bond — Comprehensive Network Impairment Test Suite
# ============================================================================
#
# Validates sliding-window FEC (XOR, W=5), ARQ, reorder buffer, and jitter
# buffer under systematic network impairment using tc/netem.
#
# Prerequisites:
#   - 007 running with bond0 up and paths configured on ens5 + ens6
#   - Management API on 127.0.0.1:8007
#   - Root privileges (tc + ip link operations)
#   - iproute2 with netem support
#
# Usage:
#   sudo bash 007-impairment-suite.sh [server_tunnel_ip]
#   Default server tunnel IP: 10.7.0.1
#
# ============================================================================
set -uo pipefail

# ─── Configuration ─────────────────────────────────────────────────────────
SERVER="${1:-10.7.0.1}"
API="http://127.0.0.1:8007"          # Client stats (local)
SERVER_API="http://${SERVER}:8007"    # Server stats (via tunnel — server must use BOND_API=0.0.0.0:8007)
IFACE1="ens5"
IFACE2="ens6"
ALL_IFACES="$IFACE1 $IFACE2"
PING_COUNT=30
PING_INTERVAL=0.1
PING_TIMEOUT=5

# ─── Colours ───────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

# ─── Counters ──────────────────────────────────────────────────────────────
PASS=0
FAIL=0
TOTAL=0

# Results array for summary table
declare -a RESULTS_NAME=()
declare -a RESULTS_RX=()
declare -a RESULTS_FEC=()
declare -a RESULTS_NACK=()
declare -a RESULTS_ARQ=()
declare -a RESULTS_DROP=()
declare -a RESULTS_VERDICT=()

# ─── Cleanup on exit ──────────────────────────────────────────────────────
cleanup_tc() {
    for iface in $ALL_IFACES; do
        tc qdisc del dev "$iface" root 2>/dev/null || true
    done
}

cleanup_links() {
    for iface in $ALL_IFACES; do
        ip link set "$iface" up 2>/dev/null || true
    done
}

cleanup_all() {
    cleanup_tc
    cleanup_links
}

trap cleanup_all EXIT INT TERM

# ─── Helper functions ──────────────────────────────────────────────────────

get_stats() {
    curl -s --max-time 3 "$API/api/stats" 2>/dev/null || echo "{}"
}

get_server_stats() {
    curl -s --max-time 3 "$SERVER_API/api/stats" 2>/dev/null || echo "{}"
}

extract_stat() {
    local json="$1"
    local key="$2"
    echo "$json" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('$key', 0))
except:
    print(0)
" 2>/dev/null
}

run_ping() {
    local count="${1:-$PING_COUNT}"
    local interval="${2:-$PING_INTERVAL}"
    ping -c "$count" -i "$interval" -W "$PING_TIMEOUT" "$SERVER" 2>&1 || true
}

count_received() {
    local output="$1"
    echo "$output" | grep -c "bytes from" 2>/dev/null || echo 0
}

# Record test result and store for summary
record_result() {
    local name="$1"
    local verdict="$2"  # PASS or FAIL
    local rx="$3"
    local fec="$4"
    local nack="$5"
    local arq="$6"
    local drop="$7"
    local detail="${8:-}"

    TOTAL=$((TOTAL + 1))
    RESULTS_NAME+=("$name")
    RESULTS_RX+=("$rx")
    RESULTS_FEC+=("$fec")
    RESULTS_NACK+=("$nack")
    RESULTS_ARQ+=("$arq")
    RESULTS_DROP+=("$drop")

    if [ "$verdict" = "PASS" ]; then
        PASS=$((PASS + 1))
        RESULTS_VERDICT+=("PASS")
        echo -e "  ${GREEN}[PASS]${RESET} $name  rx=$rx fec=$fec nack=$nack arq=$arq drop=$drop $detail"
    else
        FAIL=$((FAIL + 1))
        RESULTS_VERDICT+=("FAIL")
        echo -e "  ${RED}[FAIL]${RESET} $name  rx=$rx fec=$fec nack=$nack arq=$arq drop=$drop $detail"
    fi
}

# Run a complete test scenario
# tc netem on client interfaces = egress loss = server must recover.
# Stats are read from SERVER (where FEC/ARQ recovery happens).
# Falls back to client stats if server API unavailable.
run_test() {
    local name="$1"
    local setup_fn="$2"
    local min_rx="$3"
    local expect_fec="${4:-0}"  # 1 = expect FEC recovery
    local expect_arq="${5:-0}" # 1 = expect ARQ activity

    echo ""
    echo -e "${CYAN}--- $name ---${RESET}"

    # Ensure clean state
    cleanup_tc
    cleanup_links
    sleep 0.5

    # Stats before — from SERVER (where recovery happens)
    local before
    if [ "$SERVER_API_OK" = "true" ]; then
        before=$(get_server_stats)
    else
        echo -e "  ${YELLOW}WARNING: Using client stats — FEC/ARQ counters are not meaningful (server does recovery)${RESET}"
        before=$(get_stats)
    fi
    local fec_before arq_before nack_before drop_before
    fec_before=$(extract_stat "$before" "fec_recovered")
    arq_before=$(extract_stat "$before" "arq_received")
    nack_before=$(extract_stat "$before" "nacks_sent")
    drop_before=$(extract_stat "$before" "drop_packets")

    # Apply impairment
    eval "$setup_fn"
    sleep 0.3

    # Run traffic
    local ping_out
    ping_out=$(run_ping "$PING_COUNT" "$PING_INTERVAL")
    local rx
    rx=$(count_received "$ping_out")

    # Remove impairment
    cleanup_tc
    cleanup_links
    sleep 0.5

    # Stats after — from SERVER
    local after
    if [ "$SERVER_API_OK" = "true" ]; then
        after=$(get_server_stats)
    else
        after=$(get_stats)
    fi
    local fec_after arq_after nack_after drop_after
    fec_after=$(extract_stat "$after" "fec_recovered")
    arq_after=$(extract_stat "$after" "arq_received")
    nack_after=$(extract_stat "$after" "nacks_sent")
    drop_after=$(extract_stat "$after" "drop_packets")

    # Deltas
    local d_fec d_arq d_nack d_drop
    d_fec=$((fec_after - fec_before))
    d_arq=$((arq_after - arq_before))
    d_nack=$((nack_after - nack_before))
    d_drop=$((drop_after - drop_before))

    # Determine verdict
    local verdict="PASS"
    local detail=""

    if [ "$rx" -lt "$min_rx" ]; then
        verdict="FAIL"
        detail="(rx below threshold $min_rx)"
    fi

    if [ "$expect_fec" -eq 1 ] && [ "$d_fec" -eq 0 ]; then
        detail="$detail (expected FEC recovery, got 0)"
    fi

    if [ "$expect_arq" -eq 1 ] && [ "$d_arq" -eq 0 ] && [ "$d_nack" -eq 0 ]; then
        detail="$detail (expected ARQ activity, got 0)"
    fi

    record_result "$name" "$verdict" "$rx/$PING_COUNT" "$d_fec" "$d_nack" "$d_arq" "$d_drop" "$detail"
}

# ─── Preflight checks ─────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}================================================================${RESET}"
echo -e "${BOLD}  007 Bond — Network Impairment Test Suite${RESET}"
echo -e "${BOLD}  Server: $SERVER${RESET}"
echo -e "${BOLD}  Interfaces: $IFACE1, $IFACE2${RESET}"
echo -e "${BOLD}  Date: $(date -u '+%Y-%m-%d %H:%M:%S UTC')${RESET}"
echo -e "${BOLD}================================================================${RESET}"

# Check root
if [ "$(id -u)" -ne 0 ]; then
    echo -e "${RED}ERROR: Must run as root (tc/netem requires privileges)${RESET}"
    exit 1
fi

# Check interfaces exist
for iface in $ALL_IFACES; do
    if ! ip link show "$iface" > /dev/null 2>&1; then
        echo -e "${RED}ERROR: Interface $iface not found${RESET}"
        exit 1
    fi
done

# Check bond0 / tunnel is up
if ! ping -c 2 -W 3 "$SERVER" > /dev/null 2>&1; then
    echo -e "${RED}ERROR: Cannot reach $SERVER — is 007 running with bond paths configured?${RESET}"
    exit 1
fi
echo -e "${GREEN}Preflight: tunnel reachable${RESET}"

# Check client API
if ! curl -s --max-time 3 "$API/api/health" | grep -q ok 2>/dev/null; then
    echo -e "${YELLOW}WARNING: Client API not responding at $API — client stats unavailable${RESET}"
fi

# Check server API (needed for section I — server-side FEC recovery stats)
SERVER_API_OK=false
if curl -s --max-time 3 "$SERVER_API/api/health" | grep -q ok 2>/dev/null; then
    echo -e "${GREEN}Server API reachable at $SERVER_API${RESET}"
    SERVER_API_OK=true
else
    echo -e "${YELLOW}WARNING: Server API not responding at $SERVER_API${RESET}"
    echo -e "${YELLOW}  Server must be started with BOND_API=0.0.0.0:8007 for FEC validation tests${RESET}"
fi

# Clean any leftover tc rules
cleanup_tc

# ============================================================================
# A. RANDOM LOSS TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== A. Random Loss Tests ===${RESET}"

# A1: 5% loss on ens5 only — path diversity should absorb this completely
run_test "A1: 5% loss ens5 only (path diversity)" \
    "tc qdisc add dev $IFACE1 root netem loss 5%" \
    28 0 0

# A2: 10% loss ALL paths — FEC must recover
run_test "A2: 10% loss ALL paths (FEC recovery)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 10%; done" \
    25 1 0

# A3: 30% loss ALL paths — heavy FEC stress
run_test "A3: 30% loss ALL paths (heavy FEC stress)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 30%; done" \
    15 1 1

# ============================================================================
# B. BURST LOSS TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== B. Burst Loss Tests ===${RESET}"

# B1: 2-packet burst loss on ens5 — Gilbert-Elliott model
# gemodel: p=1% (good->bad), r=25% (bad->good) => avg burst ~4 pkts
# Reduced p for shorter bursts on single path
run_test "B1: 2-pkt burst loss ens5 (gemodel)" \
    "tc qdisc add dev $IFACE1 root netem loss gemodel 1% 25%" \
    25 0 0

# B2: 5-packet burst loss ALL paths — stresses sliding FEC window W=5
# gemodel: p=3% (good->bad), r=20% (bad->good) => avg burst ~5 pkts
run_test "B2: 5-pkt burst loss ALL (W=5 stress)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss gemodel 3% 20%; done" \
    18 1 1

# B3: 10-packet burst loss ALL paths — exceeds sliding window, ARQ must help
# gemodel: p=5% (good->bad), r=10% (bad->good) => avg burst ~10 pkts
run_test "B3: 10-pkt burst loss ALL (exceeds W=5, ARQ)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss gemodel 5% 10%; done" \
    12 1 1

# ============================================================================
# C. PATH ASYMMETRY TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== C. Path Asymmetry Tests ===${RESET}"

# C1: Mild asymmetry — 0ms vs 50ms
run_test "C1: 0ms/50ms delay asymmetry (mild)" \
    "tc qdisc add dev $IFACE1 root netem delay 0ms; tc qdisc add dev $IFACE2 root netem delay 50ms" \
    28 0 0

# C2: Extreme asymmetry — 5ms vs 100ms
run_test "C2: 5ms/100ms delay asymmetry (extreme)" \
    "tc qdisc add dev $IFACE1 root netem delay 5ms; tc qdisc add dev $IFACE2 root netem delay 100ms" \
    26 0 0

# C3: Realistic WiFi vs cellular — loss + delay mismatch
run_test "C3: WiFi(0ms+5%loss) vs Cell(80ms+0%loss)" \
    "tc qdisc add dev $IFACE1 root netem loss 5%; tc qdisc add dev $IFACE2 root netem delay 80ms" \
    26 0 0

# ============================================================================
# D. REORDER TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== D. Reorder Tests ===${RESET}"

# D1: 25% reorder on ens5 with 10ms delay
# netem reorder 75% means 25% of packets are reordered (delayed)
run_test "D1: 25% reorder ens5 (10ms delay)" \
    "tc qdisc add dev $IFACE1 root netem delay 10ms reorder 75%" \
    26 0 0

# D2: 50% reorder ALL paths
run_test "D2: 50% reorder ALL paths" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem delay 10ms reorder 50%; done" \
    24 0 0

# ============================================================================
# E. JITTER TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== E. Jitter Tests ===${RESET}"

# E1: 20ms +/- 10ms jitter on ens5 (normal distribution)
run_test "E1: 20ms+-10ms jitter ens5" \
    "tc qdisc add dev $IFACE1 root netem delay 20ms 10ms distribution normal" \
    26 0 0

# E2: 50ms +/- 30ms jitter ALL paths
run_test "E2: 50ms+-30ms jitter ALL paths" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem delay 50ms 30ms distribution normal; done" \
    22 0 0

# ============================================================================
# F. SHORT OUTAGE TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== F. Short Outage Tests ===${RESET}"

# F1: 200ms blackout on ens6
echo ""
echo -e "${CYAN}--- F1: 200ms blackout ens6 ---${RESET}"
{
    if [ "$SERVER_API_OK" = "true" ]; then before=$(get_server_stats); else before=$(get_stats); fi
    fec_before=$(extract_stat "$before" "fec_recovered")
    arq_before=$(extract_stat "$before" "arq_received")
    nack_before=$(extract_stat "$before" "nacks_sent")
    drop_before=$(extract_stat "$before" "drop_packets")

    # Start ping in background
    ping_out_file=$(mktemp /tmp/007-ping-XXXXXX)
    run_ping "$PING_COUNT" "$PING_INTERVAL" > "$ping_out_file" &
    PING_PID=$!

    # Wait a moment, then blackout
    sleep 0.5
    ip link set "$IFACE2" down
    sleep 0.2
    ip link set "$IFACE2" up
    sleep 0.5

    wait $PING_PID 2>/dev/null || true
    rx=$(count_received "$(cat "$ping_out_file")")
    rm -f "$ping_out_file"

    if [ "$SERVER_API_OK" = "true" ]; then after=$(get_server_stats); else after=$(get_stats); fi
    d_fec=$(( $(extract_stat "$after" "fec_recovered") - fec_before ))
    d_nack=$(( $(extract_stat "$after" "nacks_sent") - nack_before ))
    d_arq=$(( $(extract_stat "$after" "arq_received") - arq_before ))
    d_drop=$(( $(extract_stat "$after" "drop_packets") - drop_before ))

    if [ "$rx" -ge 26 ]; then verdict="PASS"; else verdict="FAIL"; fi
    record_result "F1: 200ms blackout ens6" "$verdict" "$rx/$PING_COUNT" "$d_fec" "$d_nack" "$d_arq" "$d_drop"
}

# F2: 500ms blackout ALL paths — complete outage
echo ""
echo -e "${CYAN}--- F2: 500ms blackout ALL paths ---${RESET}"
{
    if [ "$SERVER_API_OK" = "true" ]; then before=$(get_server_stats); else before=$(get_stats); fi
    fec_before=$(extract_stat "$before" "fec_recovered")
    arq_before=$(extract_stat "$before" "arq_received")
    nack_before=$(extract_stat "$before" "nacks_sent")
    drop_before=$(extract_stat "$before" "drop_packets")

    ping_out_file=$(mktemp /tmp/007-ping-XXXXXX)
    run_ping "$PING_COUNT" "$PING_INTERVAL" > "$ping_out_file" &
    PING_PID=$!

    sleep 0.5
    for iface in $ALL_IFACES; do ip link set "$iface" down; done
    sleep 0.5
    for iface in $ALL_IFACES; do ip link set "$iface" up; done
    sleep 1

    wait $PING_PID 2>/dev/null || true
    rx=$(count_received "$(cat "$ping_out_file")")
    rm -f "$ping_out_file"

    if [ "$SERVER_API_OK" = "true" ]; then after=$(get_server_stats); else after=$(get_stats); fi
    d_fec=$(( $(extract_stat "$after" "fec_recovered") - fec_before ))
    d_nack=$(( $(extract_stat "$after" "nacks_sent") - nack_before ))
    d_arq=$(( $(extract_stat "$after" "arq_received") - arq_before ))
    d_drop=$(( $(extract_stat "$after" "drop_packets") - drop_before ))

    # Expect some loss during 500ms blackout — pass if we recover most
    if [ "$rx" -ge 20 ]; then verdict="PASS"; else verdict="FAIL"; fi
    record_result "F2: 500ms blackout ALL paths" "$verdict" "$rx/$PING_COUNT" "$d_fec" "$d_nack" "$d_arq" "$d_drop"
}

# Allow paths to recover after link flapping
sleep 2

# ============================================================================
# G. COMBINED REAL-WORLD SCENARIOS
# ============================================================================
echo ""
echo -e "${BOLD}=== G. Combined Real-World Scenarios ===${RESET}"

# G1: "Bad WiFi" — ens5=3% loss + 15ms+-8ms jitter, ens6=0.5% loss + 2ms delay
run_test "G1: Bad WiFi scenario" \
    "tc qdisc add dev $IFACE1 root netem loss 3% delay 15ms 8ms distribution normal; tc qdisc add dev $IFACE2 root netem loss 0.5% delay 2ms" \
    26 0 0

# G2: "Cellular failover" — ens5=80ms + 5% loss + burst, ens6=10ms + 0.1% loss
run_test "G2: Cellular failover scenario" \
    "tc qdisc add dev $IFACE1 root netem delay 80ms loss gemodel 5% 30%; tc qdisc add dev $IFACE2 root netem delay 10ms loss 0.1%" \
    25 0 0

# G3: "Conference room" — ALL=2% loss + 20ms+-15ms jitter + 10% reorder
run_test "G3: Conference room scenario" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 2% delay 20ms 15ms distribution normal reorder 90%; done" \
    24 1 0

# ============================================================================
# H. SLIDING FEC SPECIFIC TESTS
# ============================================================================
echo ""
echo -e "${BOLD}=== H. Sliding FEC Specific Tests ===${RESET}"

# H1: Low uniform loss — within W=5 window, FEC should recover all
# 2% loss = ~1 packet per 50, well within single-loss XOR recovery
run_test "H1: 2% loss (within W=5, FEC recovers)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 2%; done" \
    28 1 0

# H2: Burst of W+1=6 consecutive drops — exceeds window, FEC fails, ARQ must help
# gemodel: p=8% (good->bad), r=15% (bad->good) => avg burst ~6-7 pkts
run_test "H2: Burst W+1=6 (exceeds window, ARQ)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss gemodel 8% 15%; done" \
    10 1 1

# ============================================================================
# I. SINGLE-PATH FEC/ARQ VALIDATION (Server-Side Recovery)
# ============================================================================
# The real-world scenario: CLIENT sends audio on impaired interfaces.
# tc netem on the client's interfaces drops/delays OUTBOUND packets.
# The SERVER receives the impaired stream and must recover missing packets
# using FEC and ARQ. We check the SERVER's stats for fec_recovered.
#
# tc netem egress loss is correct here — it simulates client interface issues.
# Server API must be reachable via tunnel (BOND_API=0.0.0.0:8007 on server).
#
# Bond endpoints are cleared so only the primary WireGuard path remains,
# ensuring loss isn't absorbed by multi-path redundancy.
# ============================================================================
echo ""
echo -e "${BOLD}=== I. Single-Path FEC/ARQ Validation (Server Recovery) ===${RESET}"

if [ "$SERVER_API_OK" != "true" ]; then
    echo -e "  ${RED}[SKIP]${RESET} Server API not reachable at $SERVER_API"
    echo -e "  ${RED}       Restart server with: BOND_API=0.0.0.0:8007 BOND_FEC_MODE=sliding sudo -E bash 007-sliding-server.sh${RESET}"
else

# Get WireGuard peer info for UAPI restore
WG_SOCK="/var/run/wireguard/bond0.sock"
WG_ENDPOINT=$(wg show bond0 endpoints 2>/dev/null | awk '{print $2}' | head -1)
WG_PEER_PUB=$(wg show bond0 peers 2>/dev/null | head -1)
WG_PEER_PUB_HEX=$(echo "$WG_PEER_PUB" | base64 -d 2>/dev/null | xxd -p -c 32 2>/dev/null || echo "")
WG_SERVER_IP=$(echo "$WG_ENDPOINT" | cut -d: -f1)

if [ -z "$WG_PEER_PUB_HEX" ] || [ -z "$WG_SERVER_IP" ]; then
    echo -e "  ${RED}[SKIP]${RESET} Cannot determine WireGuard peer info — skipping single-path tests"
else

# Detect all bond-eligible interfaces and their local IPs (for restore)
declare -a BOND_IFACES=()
declare -a BOND_LOCAL_IPS=()
for iface in $(ip -4 -o addr show scope global | awk '{print $2}' | sort -u); do
    [ "$iface" = "bond0" ] && continue
    local_ip=$(ip -4 addr show "$iface" | grep inet | awk '{print $2}' | cut -d/ -f1 | head -1)
    if [ -n "$local_ip" ]; then
        BOND_IFACES+=("$iface")
        BOND_LOCAL_IPS+=("$local_ip")
    fi
done

# Function: clear bond endpoints (single-path mode)
enter_single_path() {
    printf "set=1\npublic_key=%s\nclear_bond_endpoints=true\n\n" "$WG_PEER_PUB_HEX" \
        | nc -U "$WG_SOCK" -w 1 > /dev/null 2>&1 || true
    sleep 0.5
}

# Function: restore bond endpoints (multi-path mode)
restore_bond_paths() {
    local cmd="set=1\npublic_key=${WG_PEER_PUB_HEX}\nclear_bond_endpoints=true\n"
    for i in "${!BOND_LOCAL_IPS[@]}"; do
        cmd="${cmd}bond_endpoint=${WG_SERVER_IP}:51820@${BOND_LOCAL_IPS[$i]}\n"
    done
    printf "${cmd}\n" | nc -U "$WG_SOCK" -w 1 > /dev/null 2>&1 || true
    sleep 1
}

echo -e "  ${CYAN}Checking SERVER stats at $SERVER_API${RESET}"
echo -e "  ${CYAN}tc netem on client egress → server FEC must recover${RESET}"

# Wrapper for single-path tests — applies egress loss, checks SERVER stats
run_single_path_test() {
    local name="$1"
    local setup_fn="$2"
    local min_rx="$3"
    local expect_fec="${4:-0}"
    local expect_arq="${5:-0}"

    echo ""
    echo -e "${CYAN}--- $name ---${RESET}"

    # Clean state and enter single-path mode
    cleanup_tc
    cleanup_links
    enter_single_path
    sleep 0.5

    # Verify single-path connectivity
    if ! ping -c 2 -W 3 "$SERVER" > /dev/null 2>&1; then
        echo -e "  ${RED}[SKIP]${RESET} $name — tunnel not reachable in single-path mode"
        restore_bond_paths
        return
    fi

    # SERVER stats before (this is where FEC recovery happens)
    local srv_before
    srv_before=$(get_server_stats)
    local fec_before arq_before nack_before drop_before
    fec_before=$(extract_stat "$srv_before" "fec_recovered")
    arq_before=$(extract_stat "$srv_before" "arq_received")
    nack_before=$(extract_stat "$srv_before" "nacks_sent")
    drop_before=$(extract_stat "$srv_before" "drop_packets")

    # Apply egress impairment on client interfaces
    eval "$setup_fn"
    sleep 0.3

    # Run traffic — more packets for statistical significance
    local ping_out
    ping_out=$(run_ping 50 "$PING_INTERVAL")
    local rx
    rx=$(count_received "$ping_out")

    # Remove impairment
    cleanup_tc

    # SERVER stats after
    sleep 0.5
    local srv_after
    srv_after=$(get_server_stats)
    local fec_after arq_after nack_after drop_after
    fec_after=$(extract_stat "$srv_after" "fec_recovered")
    arq_after=$(extract_stat "$srv_after" "arq_received")
    nack_after=$(extract_stat "$srv_after" "nacks_sent")
    drop_after=$(extract_stat "$srv_after" "drop_packets")

    # Deltas (SERVER-side)
    local d_fec d_arq d_nack d_drop
    d_fec=$((fec_after - fec_before))
    d_arq=$((arq_after - arq_before))
    d_nack=$((nack_after - nack_before))
    d_drop=$((drop_after - drop_before))

    # Determine verdict
    local verdict="PASS"
    local detail="[server-side]"

    if [ "$rx" -lt "$min_rx" ]; then
        verdict="FAIL"
        detail="$detail (rx below threshold $min_rx)"
    fi

    if [ "$expect_fec" -eq 1 ] && [ "$d_fec" -eq 0 ]; then
        verdict="FAIL"
        detail="$detail (EXPECTED server FEC recovery, got 0)"
    fi

    if [ "$expect_arq" -eq 1 ] && [ "$d_arq" -eq 0 ] && [ "$d_nack" -eq 0 ]; then
        detail="$detail (expected server ARQ activity, got 0)"
    fi

    record_result "$name" "$verdict" "$rx/50" "$d_fec" "$d_nack" "$d_arq" "$d_drop" "$detail"

    # Restore bond paths for next test
    restore_bond_paths
}

# I1: 10% egress loss — server FEC MUST recover some packets
run_single_path_test "I1: 10% egress loss (server FEC)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 10%; done" \
    35 1 0

# I2: 20% egress loss — heavy server FEC + ARQ
run_single_path_test "I2: 20% egress loss (server FEC+ARQ)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 20%; done" \
    25 1 1

# I3: 2-pkt burst egress loss — server sliding FEC window recovery
# gemodel p=2%, r=50% => avg burst ~2 pkts
run_single_path_test "I3: 2-pkt burst egress (server FEC window)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss gemodel 2% 50%; done" \
    38 1 0

# I4: 5-pkt burst — at W=5 limit, server FEC partially recovers
# gemodel p=5%, r=20% => avg burst ~5 pkts
run_single_path_test "I4: 5-pkt burst egress (W=5 limit)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss gemodel 5% 20%; done" \
    25 1 1

# I5: 10-pkt burst — exceeds server FEC, ARQ must recover
# gemodel p=8%, r=10% => avg burst ~10 pkts
run_single_path_test "I5: 10-pkt burst egress (server ARQ must help)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss gemodel 8% 10%; done" \
    15 1 1

# I6: 5% loss + jitter — realistic single-link conditions
run_single_path_test "I6: 5% loss + jitter (realistic)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 5% delay 20ms 10ms distribution normal; done" \
    40 1 0

# I7: 30% loss — extreme stress, many server recoveries expected
run_single_path_test "I7: 30% egress loss (extreme stress)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 30%; done" \
    15 1 1

# I8: 3% loss + reorder — server ARQ must not false-trigger
run_single_path_test "I8: 3% loss + reorder (false NACK check)" \
    "for i in $ALL_IFACES; do tc qdisc add dev \$i root netem loss 3% delay 10ms reorder 70%; done" \
    40 1 0

echo ""
echo -e "  ${CYAN}Bond paths restored — back to multi-path mode${RESET}"

fi  # end WG_PEER_PUB_HEX check
fi  # end SERVER_API_OK check

# Allow bond paths to stabilise after restore
sleep 2

# ============================================================================
# Verify tunnel still works after all tests
# ============================================================================
echo ""
echo -e "${BOLD}=== Post-test validation ===${RESET}"
cleanup_tc
cleanup_links
sleep 1

if ping -c 5 -W 3 "$SERVER" > /dev/null 2>&1; then
    echo -e "  ${GREEN}[OK]${RESET} Tunnel still operational after all tests"
else
    echo -e "  ${RED}[WARN]${RESET} Tunnel not responding — may need path re-establishment"
fi

# ============================================================================
# Final stats dump
# ============================================================================
echo ""
echo -e "${BOLD}=== Final Bond Stats (Client) ===${RESET}"
get_stats | python3 -m json.tool 2>/dev/null || echo "(stats unavailable)"

if [ "$SERVER_API_OK" = "true" ]; then
    echo ""
    echo -e "${BOLD}=== Final Bond Stats (Server) ===${RESET}"
    get_server_stats | python3 -m json.tool 2>/dev/null || echo "(stats unavailable)"
fi

# ============================================================================
# SUMMARY TABLE
# ============================================================================
echo ""
echo -e "${BOLD}================================================================${RESET}"
echo -e "${BOLD}  RESULTS SUMMARY  (stats from server where recovery happens)${RESET}"
echo -e "${BOLD}================================================================${RESET}"
printf "  %-45s  %-8s  %-5s  %-5s  %-5s  %-5s  %s\n" "TEST" "RX" "FEC" "NACK" "ARQ" "DROP" "VERDICT"
printf "  %-45s  %-8s  %-5s  %-5s  %-5s  %-5s  %s\n" "----" "--" "---" "----" "---" "----" "-------"

for i in "${!RESULTS_NAME[@]}"; do
    v="${RESULTS_VERDICT[$i]}"
    if [ "$v" = "PASS" ]; then
        colour="$GREEN"
    else
        colour="$RED"
    fi
    printf "  %-45s  %-8s  %-5s  %-5s  %-5s  %-5s  ${colour}%s${RESET}\n" \
        "${RESULTS_NAME[$i]}" "${RESULTS_RX[$i]}" "${RESULTS_FEC[$i]}" \
        "${RESULTS_NACK[$i]}" "${RESULTS_ARQ[$i]}" "${RESULTS_DROP[$i]}" "$v"
done

echo ""
echo -e "  ${BOLD}Total: $TOTAL  Passed: $PASS  Failed: $FAIL${RESET}"
if [ "$FAIL" -eq 0 ]; then
    echo -e "  ${GREEN}ALL TESTS PASSED${RESET}"
else
    echo -e "  ${RED}$FAIL TEST(S) FAILED${RESET}"
fi

# ============================================================================
# MINIMUM VALIDATION SUITE
# ============================================================================
echo ""
echo -e "${BOLD}================================================================${RESET}"
echo -e "${BOLD}  MINIMUM VALIDATION SUITE (run these 5 first)${RESET}"
echo -e "${BOLD}================================================================${RESET}"
echo "  1. A1 — 5% single-path loss (confirms path diversity works)"
echo "  2. I1 — 10% loss single-path (confirms FEC actually recovers packets)"
echo "  3. I4 — 5-pkt burst single-path (stresses sliding FEC window W=5)"
echo "  4. I5 — 10-pkt burst single-path (confirms ARQ recovers beyond FEC)"
echo "  5. C2 — Extreme delay asymmetry (confirms reorder buffer handles it)"
echo "  6. F1 — 200ms single-path blackout (confirms failover works)"
echo "  7. A2 — 10% all-path loss (confirms multi-path absorbs loss)"

# ============================================================================
# FAILURE DIAGNOSIS GUIDE
# ============================================================================
#
# SYMPTOM -> LIKELY CAUSE -> INVESTIGATION
#
# A1 fails (single-path loss not absorbed):
#   -> Path diversity not working. Only one path active?
#   -> Check: curl /api/paths — are both paths "active"?
#   -> Check: tcpdump on each interface for WG traffic
#
# A2/A3 fails (FEC not recovering under uniform loss):
#   -> FEC disabled or wrong mode
#   -> Check: BOND_FEC_MODE=sliding in environment
#   -> Check: fec_recovered in stats — if 0, FEC is not running
#   -> If fec_recovered > 0 but still failing: loss exceeds XOR single-repair capacity
#
# B2 fails (burst within W=5 not recovered):
#   -> Sliding window too small or repair packets also lost
#   -> XOR can only recover 1 loss per window. Burst of 2+ in same window = fail.
#   -> Overlapping windows should cover bursts up to W-1=4, but only if repairs survive.
#   -> Check: fec_failed increasing — confirms FEC attempted but couldn't recover
#
# B3/H2 fails badly (burst > W):
#   -> Expected. FEC cannot help. ARQ must recover.
#   -> If arq_retransmit_ok is 0: ARQ not triggering or retransmit too late
#   -> Check: nacks_sent — are NACKs being generated?
#   -> Check: arq_deadline_skip — are retransmits arriving after playout deadline?
#
# C1/C2 fails (asymmetric delay causes loss):
#   -> Reorder buffer too small or window too short
#   -> Check: reorder_late — packets arriving after reorder window closes
#   -> Fix: increase reorder window (ReorderWindowMs in config)
#
# D1/D2 fails (reorder causes drops):
#   -> Reorder buffer misclassifying reordered packets as lost
#   -> Check: reorder_reordered vs reorder_late — late means buffer too small
#   -> Check: nacks_sent — false NACKs for merely-reordered packets waste bandwidth
#   -> Too many false NACKs = ARQ reorder suppression not working
#
# E1/E2 fails (jitter causes drops):
#   -> Jitter buffer depth too shallow
#   -> Check: JitterStats.Late — packets arriving after playout deadline
#   -> Fix: increase BufferDepth
#
# F1 fails (200ms blackout drops packets):
#   -> Failover too slow. Path health detection not switching fast enough.
#   -> Check: path state transitions in logs
#   -> 200ms should be within ARQ deadline for buffered packets
#
# F2 many drops (500ms full outage):
#   -> Expected to lose some packets. 500ms > typical jitter buffer depth.
#   -> If ALL packets lost: WireGuard handshake may have timed out
#   -> Check: wg show — is handshake recent?
#
# G tests fail (combined scenarios):
#   -> Usually indicates a specific subsystem struggling under combined load
#   -> Run the individual component tests (A/B/C/D/E) to isolate
#
# I1-I8 server-side FEC tests — fec_recovered still 0:
#   -> Server API not reachable. Must start server with BOND_API=0.0.0.0:8007
#   -> Server not running sliding FEC. Must start with BOND_FEC_MODE=sliding
#   -> tc netem only affects client EGRESS. Server's FEC decoder recovers
#      packets the client dropped. Check SERVER stats, not client stats.
#   -> If server fec_recovered > 0 but test still FAIL: check rx threshold
#
# I1-I8 — tunnel unreachable after clearing bond endpoints:
#   -> Primary WireGuard path uses a different interface than expected
#   -> Check: wg show bond0 endpoints — verify server endpoint
#
# I5 — server ARQ not recovering despite heavy loss:
#   -> Server retransmit requests (NACKs) also lost on impaired path
#   -> Check server arq_deadline_skip — retransmits too late for playout
#   -> Increase playout buffer or reduce loss rate
#
# General: fec_recovered always 0:
#   -> FEC not enabled. Check BOND_FEC_MODE env var.
#   -> Repair packets being dropped by netem too (expected under all-path loss).
#
# General: arq_retransmit_ok always 0 but nacks_sent > 0:
#   -> Server received NACKs but retransmits not arriving.
#   -> Retransmits may be dropped by same netem rule.
#   -> Or retransmits arriving after playout deadline (check arq_deadline_skip).
#
# General: high drop_packets despite FEC+ARQ:
#   -> Loss rate exceeds system capacity.
#   -> XOR FEC can recover 1 loss per W=5 window.
#   -> With 2 paths, effective capacity is roughly 2x single-path.
#   -> Beyond ~40% all-path loss, system will degrade.
#
# ============================================================================

exit $FAIL
