# System Overview

This project implements a real-time bonded transport system for broadcast RTP audio over the public internet, with SIP-based signalling.

The system:

- uses multiple concurrent interfaces (Ethernet, Wi-Fi, 4G/5G)
- applies bonding, path selection, and scheduling
- implements packet recovery via FEC, Reed-Solomon, and ARQ
- prioritises low-latency, continuous playout over perfect delivery
- must operate reliably under loss, jitter, reordering, and path instability

This is a real-time media system, not a generic network service.

All design decisions must respect latency, timing, and continuity constraints.

## Core Design Priorities (in order)

1. Continuous audio playout
2. Bounded and predictable latency
3. Graceful degradation under network impairment
4. Efficient packet recovery within latency constraints
5. Deterministic behaviour under failure conditions
6. Full observability of network and recovery behaviour

Perfect packet recovery is not the primary goal if it increases latency beyond acceptable limits.

## Architecture Overview

### Layering

```
transport/     RTP handling, packet ingest/egress, framing, sequencing
bonding/       path management, packet scheduling, duplication strategies
recovery/      FEC / RS encoding/decoding, ARQ request/response logic
session/       session lifecycle, stream state
control/       SIP interaction, signalling integration
network/       interface abstraction (Ethernet, Wi-Fi, cellular), socket handling
observability/ metrics, logs, health
config/
```

Business logic must not leak into transport or recovery layers.

## Real-Time Media Constraints

This system operates under strict real-time requirements.

### Rules

- all operations must respect a bounded playout deadline
- packets arriving after their usefulness must be discarded
- latency must never grow unbounded due to recovery logic
- resequencing buffers must be finite and explicitly sized
- jitter must be absorbed within defined limits only
- packet duplication and reordering are normal conditions
- degraded operation is expected and must be handled explicitly

Claude must not introduce logic that:

- buffers indefinitely
- retries without deadlines
- increases latency unpredictably

## RTP Handling Rules

RTP behaviour must remain standards-compliant.

- preserve: sequence numbers, timestamps, SSRC, payload type
- handle: sequence wraparound, duplicate packets from multiple paths, out-of-order delivery
- maintain: packet pacing characteristics, timing integrity
- enforce: MTU-safe packet sizing, no fragmentation assumptions
- Duplicate RTP packets must be detected via sequence number and deduplicated deterministically

## SIP / Control Plane Rules

SIP signalling is separate from media transport.

- media must continue independently of SIP timing
- SIP session state must not tightly couple to path state
- media degradation must not automatically terminate sessions
- re-INVITE or session updates must be explicit and controlled
- NAT traversal behaviour must be clearly defined
- control-plane failures must not cascade into media instability

## Bonding and Path Management

The system operates over multiple network interfaces.

### Path Metrics (must be continuously tracked)

For each path: RTT, jitter, packet loss rate, burst loss, reorder rate, available throughput (if measurable), interface state (up/down), recent stability history

### Path States

Each path must have an explicit state: healthy, degraded, unstable, failed, recovering

### Path Rules

- path selection must be policy-driven
- avoid oscillation during flapping
- path reinstatement requires sustained stability
- distinguish: brownout (high loss/jitter) vs blackout (no connectivity)
- system must function with reduced path sets

### Scheduling

Packet scheduling must: be deterministic, be observable, support load distribution and redundancy (duplicate send if required), not rely on assumptions of equal path quality

## Recovery Mechanisms (FEC / RS / ARQ)

Recovery is bounded by latency constraints.

### General Rules

- recovery must complete within playout deadline
- unrecoverable packets must be declared explicitly
- recovery must not create cascading delays

### FEC / Reed-Solomon

- block size must balance latency and recovery capability
- encoding/decoding must be efficient and bounded
- block boundaries must be well-defined and testable

### ARQ

- only request retransmission if it can arrive in time
- retransmission windows must be bounded
- ARQ must be rate-limited
- ARQ must not create storms under heavy loss

### Interaction Rules

- FEC and ARQ must not conflict or duplicate effort unnecessarily
- system must avoid over-recovery (wasting bandwidth)
- recovery strategy must adapt to random loss, burst loss, and path asymmetry

## Degraded Mode Behaviour

Degraded operation is normal and must be explicit.

System states: healthy, degraded (recoverable), severely degraded, unrecoverable loss, resync required

Rules: system must continue operating under partial failure, transitions must be observable, behaviour must be deterministic, no hidden fallback logic

## Observability Requirements

### Required Metrics

Per path: RTT, jitter, loss rate, burst loss, reorder depth

Global: packet receive rate, packet drop rate, duplicate packet count, ARQ request rate, ARQ success rate, FEC recovery success rate, unrecovered packets, playout underruns, effective latency, active path count, path switch events

### Logging

Logs must include: session ID, stream ID, path/interface, sequence numbers (when relevant), recovery actions, state transitions

## Network Behaviour Assumptions

Do not assume: stable latency, ordered delivery, symmetric paths, reliable connectivity, fixed IP addresses, stable NAT bindings

Always design for: jitter spikes, path flaps, packet duplication, reordering, intermittent total loss

## Concurrency Rules

- no unbounded goroutines
- all routines must have lifecycle ownership
- cancellation via context must be honoured
- shared state must be explicitly synchronised
- avoid race-prone designs in hot paths

## Testing Requirements

Testing must simulate real network conditions.

Required scenarios: random loss, burst loss, heavy reordering, duplicate packets, path flapping, Wi-Fi roaming, cellular instability, asymmetric latency across paths, NAT rebinding, sequence wraparound, FEC boundary failure, ARQ overload scenarios, SIP session maintained during media degradation

Tests must verify: bounded latency, recovery effectiveness, graceful degradation, correct state transitions

## Security Considerations

- authenticate endpoints
- validate all incoming packets
- protect against packet injection, replay attacks, malformed payloads
- rate-limit control and recovery messages
- secure signalling (TLS/mTLS where applicable)

## Configuration Rules

All of the following must be configurable: latency budget, playout buffer size, FEC parameters, ARQ window and limits, path scoring thresholds, retry/backoff policies

No hardcoded operational constants.

## Anti-Patterns (Strictly Forbidden)

- unbounded buffering
- retry without deadline
- coupling SIP state to media recovery
- hidden goroutines
- uncontrolled ARQ floods
- assuming ordered packet delivery
- assuming a single stable network path
- silent packet drops without metrics
- recovery mechanisms without limits

## When Generating Code

Claude must: prioritise deterministic behaviour, consider latency impact of every change, explicitly handle degraded conditions, expose state via metrics/logs, justify concurrency decisions, avoid abstraction that hides critical behaviour

## When Reviewing Code

Claude must check for: latency violations, unbounded recovery behaviour, missing deadlines/timeouts, incorrect RTP handling, weak path-state logic, insufficient observability, hidden coupling between layers, unsafe concurrency

## Final Principle

This system is judged by how it behaves under failure, not how it behaves under ideal conditions.

All code must be written with impaired networks as the default scenario, not the exception.
