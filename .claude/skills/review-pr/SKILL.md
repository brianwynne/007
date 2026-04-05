---
name: review-pr
description: Review a branch or PR-sized change for transport safety.
disable-model-invocation: true
---

Review the current diff as if it were a production PR for a real-time bonded RTP audio transport.

Do:
1. identify changed files
2. run verify-transport
3. run run-impairment-suite if transport, scheduler, recovery, RTP, or SIP files changed
4. produce a release-risk summary
5. state whether the change is safe for lab only, pilot, or production
