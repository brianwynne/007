#!/usr/bin/env bash
# ============================================================================
# 007 Bond — Enrollment Server
# ============================================================================
#
# Lightweight HTTP enrollment service for automated key exchange.
# Runs alongside 007 and handles client registration.
#
# Flow:
#   1. Admin generates token:  sudo 007-bond enroll-token
#   2. Token given to field engineer
#   3. Client runs:  install-007-client.sh --enroll http://server:8007 --token <token>
#   4. Client generates keypair, POSTs public key + token to /api/enroll
#   5. Server validates token, adds WireGuard peer, returns server pub + tunnel IP
#   6. Tunnel comes up — no private keys cross the network
#
# Tokens are stored in /etc/007/tokens/ (one file per token, deleted on use)
# Each token file contains the allocated tunnel IP for that client.
#
# This script is called by the 007-bond systemd service as a sidecar.
# It uses Python's http.server (available on all targets) to avoid
# adding dependencies.
#
# ============================================================================
set -euo pipefail

CONFIG_DIR="/etc/007"
TOKEN_DIR="$CONFIG_DIR/tokens"
INTERFACE="bond0"
LISTEN_PORT="${ENROLL_PORT:-8007}"

source "$CONFIG_DIR/.env" 2>/dev/null || true

mkdir -p "$TOKEN_DIR"

# Python enrollment handler — runs inside the 007-bond API process
# We write a small Python script that the installer drops in place.
# The Go API doesn't need modification — enrollment is a sidecar.

exec python3 - "$TOKEN_DIR" "$CONFIG_DIR" "$INTERFACE" << 'PYEOF'
import http.server
import json
import os
import subprocess
import sys
import socketserver

TOKEN_DIR = sys.argv[1]
CONFIG_DIR = sys.argv[2]
INTERFACE = sys.argv[3]
LISTEN_PORT = int(os.environ.get("ENROLL_PORT", "8017"))

class EnrollHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        pass  # suppress default logging

    def do_POST(self):
        if self.path != "/api/enroll":
            self.send_error(404)
            return

        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length))
        except Exception:
            self.send_error(400, "Invalid JSON")
            return

        token = body.get("token", "").strip()
        client_pub = body.get("public_key", "").strip()

        if not token or not client_pub:
            self._respond(400, {"error": "token and public_key required"})
            return

        # Validate token
        token_file = os.path.join(TOKEN_DIR, token)
        if not os.path.isfile(token_file):
            self._respond(403, {"error": "invalid or expired token"})
            return

        # Read allocated tunnel IP from token file
        with open(token_file) as f:
            tunnel_ip = f.read().strip()

        # Read server public key
        server_pub_file = os.path.join(CONFIG_DIR, "server.pub")
        if not os.path.isfile(server_pub_file):
            self._respond(500, {"error": "server key not found"})
            return
        with open(server_pub_file) as f:
            server_pub = f.read().strip()

        # Add client as WireGuard peer
        try:
            subprocess.run(
                ["wg", "set", INTERFACE, "peer", client_pub,
                 "allowed-ips", f"{tunnel_ip}/32"],
                check=True, capture_output=True, timeout=10
            )
        except subprocess.CalledProcessError as e:
            self._respond(500, {"error": f"wg set failed: {e.stderr.decode()}"})
            return
        except FileNotFoundError:
            self._respond(500, {"error": "wg command not found"})
            return

        # Persist peer for server restarts
        peers_dir = os.path.join(CONFIG_DIR, "peers")
        os.makedirs(peers_dir, exist_ok=True)
        peer_file = os.path.join(peers_dir, tunnel_ip.replace(".", "_"))
        with open(peer_file, "w") as f:
            f.write(f"{client_pub}\n{tunnel_ip}\n")
        os.chmod(peer_file, 0o600)

        # Consume token (one-time use)
        os.remove(token_file)

        # Get server endpoint — use the IP the client connected to
        # (from the HTTP request), not hostname -I which returns the
        # private IP on cloud instances behind NAT.
        ip = self.headers.get("Host", "").split(":")[0]
        if not ip or ip in ("localhost", "127.0.0.1", "0.0.0.0"):
            try:
                ip = subprocess.run(
                    ["hostname", "-I"], capture_output=True, text=True, timeout=5
                ).stdout.split()[0]
            except Exception:
                ip = "unknown"

        listen_port = os.environ.get("LISTEN_PORT", "51820")

        self._respond(200, {
            "server_pub": server_pub,
            "tunnel_ip": tunnel_ip,
            "endpoint": f"{ip}:{listen_port}",
            "status": "enrolled"
        })

        print(f"[ENROLL] Client enrolled: {tunnel_ip} (peer {client_pub[:20]}...)")

    def do_GET(self):
        if self.path == "/api/enroll/health":
            self._respond(200, {"status": "ok"})
        else:
            self.send_error(404)

    def _respond(self, code, data):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())

with socketserver.TCPServer(("0.0.0.0", LISTEN_PORT), EnrollHandler) as httpd:
    print(f"[ENROLL] Listening on 0.0.0.0:{LISTEN_PORT}")
    httpd.serve_forever()
PYEOF
