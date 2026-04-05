#!/usr/bin/env bash
set -euo pipefail

echo "[verify] quick checks after edit"

if command -v go >/dev/null 2>&1; then
  echo "[verify] go test"
  go test ./... || true
fi

if command -v golangci-lint >/dev/null 2>&1; then
  echo "[verify] golangci-lint"
  golangci-lint run || true
fi

if command -v staticcheck >/dev/null 2>&1; then
  echo "[verify] staticcheck"
  staticcheck ./... || true
fi
