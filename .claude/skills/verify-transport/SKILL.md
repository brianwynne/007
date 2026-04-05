---
name: verify-transport
description: Run the full transport verification workflow using the specialist agents.
disable-model-invocation: true
---

Run a full verification pass for the current changes.

Steps:
1. Ask architecture-reviewer to review the changed code.
2. Ask rtp-verifier to review RTP/media-plane correctness.
3. Ask sip-verifier to review signalling/control-plane correctness.
4. Ask recovery-verifier to review FEC, Reed-Solomon, and ARQ logic.
5. Ask observability-reviewer to review logs and metrics.
6. Ask security-reviewer to review abuse resistance.
7. Summarize findings under:
   - Critical
   - Important
   - Nice to improve
   - Missing tests
   - Go/no-go
