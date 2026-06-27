# LiveKit SIP Service Architecture

## Scope and Purpose

This repository implements a SIP-to-LiveKit bridge service. It accepts inbound SIP calls, places outbound SIP calls, and relays media (audio and optional video) between SIP endpoints and LiveKit rooms. It also handles authentication, dispatch/routing, transfer signaling, call state updates, metrics, and deployment packaging.

Primary source of operational behavior:

- `cmd/livekit-sip/main.go`
- `pkg/sip`
- `pkg/service`
- `pkg/config`

## Runtime Topology

At runtime, one `livekit-sip` process coordinates four planes:

1. **Control plane**
   - Receives SIP participant creation/management requests via PSRPC.
   - Queries auth/dispatch services for inbound call decisions.
2. **Signaling plane**
   - SIP server (UAS) for inbound INVITE/BYE/ACK/NOTIFY/REFER.
   - SIP client (UAC) for outbound INVITE, auth challenge handling, ACK/BYE/CANCEL.
3. **Media plane**
   - RTP/SRTP sockets, codec negotiation/transcoding, DTMF, timeout/jitter behavior.
   - Optional video SDP and RTP H264 path.
4. **Room plane**
   - LiveKit room joins, participant publication/subscription, room disconnect handling.

Key files:

- `pkg/sip/service.go`
- `pkg/sip/server.go`
- `pkg/sip/client.go`
- `pkg/sip/inbound.go`
- `pkg/sip/outbound.go`
- `pkg/sip/media_port.go`
- `pkg/sip/room.go`

## Entrypoints and Process Lifecycle

### Executables

- **Production binary:** `cmd/livekit-sip/main.go`
- **Test utilities:** `test/client/main.go`, `test/lktest-sip-outbound/main.go`

### Boot sequence

1. Parse config (`--config`, `--config-body`, env fallbacks).
2. Initialize logging, optional tracing, and metrics endpoints.
3. Connect Redis and PSRPC bus.
4. Build SIP service dependencies and register RPC handlers.
5. Start SIP listeners (UDP/TCP/TLS as configured) and health/debug servers.

Relevant files:

- `cmd/livekit-sip/main.go`
- `pkg/config/config.go`
- `pkg/service/service.go`
- `pkg/service/psrpc.go`
- `pkg/sip/service.go`

## Directory Map and Responsibilities

- `cmd/`
  - Process entrypoint and wiring for the SIP service.
- `pkg/config/`
  - Typed configuration schema, defaults, validation, parsing, logger setup.
- `pkg/service/`
  - Shared host service concerns (health, metrics, pprof, RPC registration).
- `pkg/sip/`
  - Core SIP domain orchestration: inbound/outbound flows, media, room bridge, protocol helpers, analytics.
- `pkg/media/`
  - Reusable codec and media primitives; video compositor implementations.
- `pkg/stats/`
  - Prometheus metrics and termination classification.
- `pkg/errors/`
  - Canonical error wrapping and SIP status mapping.
- `pkg/siptest/`, `pkg/audiotest/`
  - Test support libraries.
- `res/`
  - Embedded audio assets.
- `test/`
  - Integration/cloud suites and local E2E harnesses.
- `build/`
  - Docker image definitions.
- `.github/workflows/`
  - CI/CD pipelines (test/build/docker release).

## Core Domain Model

### SIP service contracts

- `Handler` in `pkg/sip/server.go` defines how signaling integrates with auth, dispatch, and transfer topics.
- Core decision types include auth info and dispatch outcomes (`AuthInfo`, `CallDispatch`, `DispatchResult`).

### Identity and addressing

- SIP URI/addressing and transport helpers live in `pkg/sip/types.go`.
- Protocol-level utility methods for contacts/route selection are in `pkg/sip/protocol.go`.

### Session abstractions

- Call/session state and room coupling are represented across:
  - `pkg/sip/room.go`
  - `pkg/sip/participant.go`
  - `pkg/sip/analytics.go`

### Media abstractions

- `MediaPort` is the key abstraction for RTP/SRTP media connection lifecycle in `pkg/sip/media_port.go`.
- Codec negotiation and preferences are centralized in `pkg/sip/media_codecs.go`.

## Inbound Call Flow (UAS)

1. SIP server receives INVITE in `pkg/sip/server.go`.
2. `processInvite` in `pkg/sip/inbound.go` parses source transport/headers and builds call context.
3. Auth and dispatch decisions are fetched via configured handlers.
4. If accepted:
   - Allocate/prepare media (`runMediaConn` path).
   - Send SIP response with negotiated SDP.
   - Join LiveKit room and wire participant media.
5. Keep call alive while monitoring:
   - BYE/ACK/NOTIFY handling.
   - Transfer updates and call-state transitions.
6. On termination:
   - Update metrics and call state.
   - Tear down media sockets and room participant resources.

Primary files:

- `pkg/sip/server.go`
- `pkg/sip/inbound.go`
- `pkg/sip/media.go`
- `pkg/sip/room.go`
- `pkg/sip/analytics.go`

## Outbound Call Flow (UAC)

1. RPC request creates outbound SIP participant in `pkg/sip/client.go`.
2. `newCall` in `pkg/sip/outbound.go` constructs outbound call state and media ports.
3. Service can connect room state early, then dial SIP endpoint.
4. Outbound signaling path:
   - Build SDP offer.
   - Send INVITE.
   - Handle digest challenges.
   - Parse SDP answer and send ACK.
5. Once established:
   - Start media relay between SIP RTP and LiveKit.
   - Publish periodic state/metrics.
6. On close:
   - Send BYE/CANCEL as appropriate.
   - Clean room/media resources and persist final call outcome.

Primary files:

- `pkg/sip/client.go`
- `pkg/sip/outbound.go`
- `pkg/sip/outbound_utilities_test.go`
- `pkg/sip/analytics.go`

## Media Plane Design

### Audio

- RTP read/write loops, timeout and jitter behavior, SRTP integration, and DTMF handling are centered in `pkg/sip/media_port.go`.
- Codec selection and SDP mappings are in `pkg/sip/media_codecs.go`.
- Opus helpers and supporting media code are in `pkg/media/opus` and `pkg/media/rtpconn`.

### Video (optional/feature-dependent)

- Video SDP offer/answer helpers: `pkg/sip/video_sdp.go`.
- SIP video RTP packetization path: `pkg/sip/video.go`.
- Room video wiring/compositing path: `pkg/sip/room_video.go`.
- Backend implementation split:
  - `pkg/media/video/gst.go` (GStreamer-backed implementation when enabled)
  - `pkg/media/video/stub.go` (stub implementation for builds/environments without GStreamer)
  - Shared contracts in `pkg/media/video/video.go`

### Port and network behavior

- Dynamic media port allocation and address selection are handled by:
  - `pkg/sip/media_port.go`
  - `pkg/sip/media_port_test.go`
  - `pkg/sip/config.go`

## Session and State Lifecycle

- Live call lifecycle is modeled with ongoing status updates and transfer events in:
  - `pkg/sip/analytics.go`
  - `pkg/sip/participant.go`
  - `pkg/sip/inbound.go`
  - `pkg/sip/outbound.go`
- Disconnect reasons and call termination categories feed metrics and reporting in `pkg/stats/termination.go`.

## Transport and Security

- SIP server transport listeners are configured in `pkg/sip/server.go`.
- TLS setup and validation logic is in `pkg/sip/tls.go`.
- Contact URI and route-related behavior is in `pkg/sip/protocol.go`.
- IP/public address resolution and service network configuration are in `pkg/sip/config.go`.

## Integrations

- **LiveKit**
  - Room, participant, and media track interactions in `pkg/sip/room.go`.
  - SIP RPC/state integration via `pkg/service/psrpc.go` and `pkg/sip/client.go`.
- **Redis/PSRPC**
  - Bootstrapped in `cmd/livekit-sip/main.go`; used for control-plane RPC.
- **SIP Providers**
  - Provider-specific header handling/mapping in `pkg/sip/participant.go`.
- **Observability**
  - Metrics: `pkg/stats/monitor.go`
  - Tracing setup/helpers: `cmd/livekit-sip/main.go`, `pkg/sip/otel.go`

## Build, Deploy, and Operations

- Docker image build:
  - `build/sip/Dockerfile`
  - `docker-compose.yaml` for local multi-service runs.
- Build/test helpers:
  - `magefile.go`
- CI/CD:
  - `.github/workflows/test.yaml`
  - `.github/workflows/build.yaml`
  - `.github/workflows/docker.yaml`

## Testing Strategy

### Unit tests

Most SIP logic has package-level unit tests in `pkg/sip`, including signaling, media timeouts, TLS parsing, routing behavior, and SDP handling.

Notable files:

- `pkg/sip/signaling_test.go`
- `pkg/sip/service_test.go`
- `pkg/sip/media_port_test.go`
- `pkg/sip/tls_test.go`
- `pkg/sip/video_sdp_test.go`

### Integration tests

End-to-end and environment-aware tests are in:

- `test/integration/`
- `test/cloud/`
- `test/lktest/`

These validate Redis/LiveKit/SIP interactions and full call behavior under more realistic runtime conditions.

## Feature Flags and Build Variants

- Video backend behavior is build/runtime dependent:
  - GStreamer-enabled path (`pkg/media/video/gst.go`)
  - Non-GStreamer stub fallback (`pkg/media/video/stub.go`)
- Runtime feature controls and networking knobs are declared in `pkg/config/config.go` and consumed in SIP/media setup paths.

## Complexity Hotspots and Coupling Notes

1. `pkg/sip/inbound.go` and `pkg/sip/outbound.go` are large orchestrators spanning signaling, media, room state, and cleanup.
2. `pkg/sip/media_port.go` contains dense media logic (timers, RTP/SRTP, DTMF, codec behavior), making it critical for reliability and regressions.
3. Control-plane contracts are tightly coupled to SIP lifecycle behavior (`pkg/service/psrpc.go`, `pkg/sip/client.go`, `pkg/sip/server.go`).
4. Video support introduces environment/build variability (`gst` vs stub paths), requiring test coverage in both variants.
5. Call-state consistency depends on alignment across analytics, participant attributes, and transfer/topic handling.

## Recommended Reading Order for New Contributors

1. `README.md`
2. `cmd/livekit-sip/main.go`
3. `pkg/config/config.go`
4. `pkg/sip/service.go`
5. `pkg/sip/server.go` and `pkg/sip/client.go`
6. `pkg/sip/inbound.go` and `pkg/sip/outbound.go`
7. `pkg/sip/media_port.go` and `pkg/sip/room.go`
8. `test/integration` and key `pkg/sip/*_test.go` files
