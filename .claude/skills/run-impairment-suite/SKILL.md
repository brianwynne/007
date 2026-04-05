---
name: run-impairment-suite
description: Run network impairment verification for bonded RTP transport.
disable-model-invocation: true
---

Use impairment-tester to locate or create the right test harness and run impairment testing for the current code.

Prioritize:
- tc/netem-based reproducible scenarios
- path flap and asymmetry tests
- FEC/ARQ edge cases
- SIP continuity during media degradation

Return:
- scenarios executed
- commands used
- pass/fail by scenario
- defects found
- follow-up fixes required
