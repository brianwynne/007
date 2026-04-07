# 007 Bond — Quick Start

## 1. Install Server

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-server.sh | sudo -E bash
```

## 2. Generate Client Token

```bash
sudo 007-bond enroll-token
```

Copy the client install command from the output.

## 3. Install Client

```bash
curl -fsSL https://raw.githubusercontent.com/brianwynne/007/main/deploy/install-007-client.sh | \
  sudo ENROLL_URL=http://<server_ip>:8017 ENROLL_TOKEN=<token> bash
```

## 4. Verify

```bash
ping 10.7.0.1              # from client
sudo 007-bond status        # check service
sudo 007-bond stats         # view FEC/ARQ stats
sudo 007-bond paths         # view per-path health
```

## 5. Set Latency Preset

```bash
sudo 007-bond preset broadcast   # 40ms  — live broadcast
sudo 007-bond preset studio      # 80ms  — studio links
sudo 007-bond preset field       # 200ms — WiFi + cellular (default)
```

Changes apply instantly to both sides — no restart needed.

## Useful Commands

```bash
sudo 007-bond status          # service, peers, preset
sudo 007-bond stats           # FEC, ARQ, jitter stats
sudo 007-bond logs            # tail logs
sudo 007-bond restart         # restart service
sudo 007-bond upgrade         # upgrade to latest
```

Full documentation: [INSTALL.md](docs/INSTALL.md)
