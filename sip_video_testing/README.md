# SIP video call test (SIPp)

Test a **video** SIP call into a meeting on `ap1sip-dev.daakia.co.in`:

1. Place a video INVITE to `1111@ap1sip-dev.daakia.co.in` (audio PCMU + H.264 video, `sendrecv`).
2. On answer, **publish a single video stream** and keep the audio leg alive.
3. After **10 seconds**, send DTMF **`560545#`** (RFC2833) to join the meeting.
4. Stay in the meeting and **record the mixed video stream we receive**.
5. Hang up (BYE).

SIPp drives the signaling and sends our published video; because SIPp cannot
write inbound RTP to disk, the **received** (mixed) video is captured with
`tcpdump` and reconstructed into an `.mp4` by a small bundled Python tool.

```
sip video testing/
├── video_call.xml                 # main scenario (IP-authenticated trunk)
├── video_call_auth.xml            # same flow, with SIP digest auth (401/407)
├── video_call_cisco_dx80.xml      # Cisco DX-80 / Tandberg dual-PT H264 scenario
├── run_test.sh                    # one-shot: generate media → capture → call → record → verify
├── media/                         # generated RTP pcaps (created on first run)
│   ├── video.pcap                 #   H.264 PT 97  @ 90kHz (generic / video_call.xml)
│   ├── video_pt126.pcap           #   H.264 PT 126 @ 90kHz (Cisco DX-80 scenario)
│   └── audio.pcap                 #   PCMU  PT 0   @ 8kHz
└── tools/
    ├── generate_media.sh          # build the pcaps with ffmpeg (no root)
    ├── udp_to_pcap.py             # capture ffmpeg's RTP into a pcap (no root)
    └── extract_received_video.py  # depacketize received H.264 → .h264/.mp4
```

## Prerequisites

- **SIPp built with PCAP-play support** (needed for `play_pcap_video` and
  `play_dtmf`). The Homebrew bottle may not include it, so building from source
  is the reliable path:

  ```bash
  brew install pcap   # or: libpcap is already on macOS
  git clone https://github.com/SIPp/sipp.git && cd sipp
  cmake . -DUSE_PCAP=1
  make
  sudo make install   # or run ./sipp from the build dir
  ```

  Use SIPp **3.7.3 or newer** — earlier 3.7.x dropped the `-mp` /
  `[auto_media_port]` keywords this scenario relies on (they were restored in
  3.7.3).

- **ffmpeg** — to generate the published video and to mux the recorded video.
- **python3** — for the bundled capture/depacketize tools (standard library only).
- **tcpdump** — to capture the call so the received video can be recorded
  (needs `sudo`).

## Quick start

```bash
cd "sip video testing"
./run_test.sh
```

This will, in order:

1. generate `media/video.pcap` + `media/audio.pcap` (first run only),
2. start `tcpdump` on your capture interface (prompts for `sudo`),
3. place the call with SIPp,
4. stop the capture and write the recorded video to `received_video.mp4`
   (and the raw `received_video.h264`).

### Common overrides (environment variables)

| Variable      | Default                    | Meaning                                   |
|---------------|----------------------------|-------------------------------------------|
| `REMOTE_HOST` | `ap1sip-dev.daakia.co.in`  | SIP server host                           |
| `REMOTE_PORT` | `5060`                     | SIP server port                           |
| `SERVICE`     | `1111`                     | Number we dial                            |
| `TRANSPORT`   | `u1`                       | `u1`=UDP, `t1`=TCP, `l1`=TLS              |
| `SCENARIO`    | `video_call.xml`           | use `video_call_auth.xml` for digest auth |
| `LOCAL_IP`    | auto (from `CAP_IFACE`)    | local IP advertised in SIP/SDP            |
| `CAP_IFACE`   | `en0`                      | interface for `tcpdump` + IP detection    |
| `MEDIA_PORT`  | `6000`                     | RTP base; audio=6000, video=6002          |
| `AUTH_USER` / `AUTH_PASS` | empty          | digest credentials (auth scenario)        |

Examples:

```bash
# TCP transport, explicit local IP
LOCAL_IP=192.168.1.50 TRANSPORT=t1 ./run_test.sh

# server requires digest auth
SCENARIO=video_call_auth.xml AUTH_USER=myuser AUTH_PASS=secret ./run_test.sh

# capture on Wi-Fi interface en1
CAP_IFACE=en1 ./run_test.sh

# Cisco DX-80 / Tandberg dual-PT H264 scenario (see section below)
SCENARIO=video_call_cisco_dx80.xml ./run_test.sh
```

## Cisco DX-80 / Tandberg blank-tile scenario

`video_call_cisco_dx80.xml` reproduces the exact SDP structure a **Cisco DX-80
(CE firmware)** sends, which triggered a blank video tile bug:

**The bug** — The Cisco INVITE offers two H.264 payload types:

| PT  | `packetization-mode` | Capacity params |
|-----|----------------------|-----------------|
| 97  | `0` (single-NAL)     | `max-br=2500;max-mbps=245000;…` |
| 126 | `1` (non-interleaved, preferred) | `max-br=2500;max-mbps=122400;…` |

Without the fix, the server would pick PT 97 (first H264 found), discard all
capacity params, and answer with just `profile-level-id` + `packetization-mode`.
Cisco interprets the missing capacity params as "no constraints" and either
doesn't send video or sends something the decoder can't handle → **blank tile**.

**The fix** (in `pkg/sip/video_sdp.go`):
1. `selectH264` now prefers a PT with an *explicit* `packetization-mode=1` in
   its fmtp over one that merely defaults to it.
2. The capacity params (`max-br`, `max-mbps`, `max-fs`, `max-dpb`, `max-smbps`)
   are parsed from the selected PT's fmtp and stored in `videoMediaConf.H264FmtpExtra`.
3. `addVideoAnswer` echoes them back verbatim so Cisco receives a complete answer.

**Expected server answer with the fix:**
```
m=video <port> RTP/AVP 126
a=rtpmap:126 H264/90000
a=fmtp:126 profile-level-id=428014;packetization-mode=1;max-br=2500;max-mbps=122400;max-fs=8160;max-dpb=16320;max-smbps=122400
a=rtcp-fb:126 nack pli
a=sendrecv
```

**Running the scenario:**
```bash
SCENARIO=video_call_cisco_dx80.xml ./run_test.sh
```

`run_test.sh` will automatically:
1. Generate `media/video_pt126.pcap` (PT 126 video, played back after answer).
2. Run SIPp with `-trace_msg` to capture all SIP messages.
3. After the call, **verify the 200 OK answer** contains PT 126 and all five
   capacity params, printing `[PASS]` / `[FAIL]` for each:

```
== verifying Cisco DX-80 answer SDP ==
  [PASS] server selected PT 126 (packetization-mode=1)
  [PASS] answer contains max-br=
  [PASS] answer contains max-mbps=
  [PASS] answer contains max-fs=
  [PASS] answer contains max-dpb=
  [PASS] answer contains max-smbps=

  RESULT: PASS — server answer is Cisco-compatible (blank tile fix verified)
```

The scenario uses the same SIP headers and audio SDP as `video_call.xml` so
it hits the same auth/dispatch path.  Only the `m=video` line differs: it
offers PT 97 (mode=0) and PT 126 (mode=1) with capacity params — the minimum
required to reproduce and verify the blank-tile fix.

> **Note on extra Cisco m-lines:** A real Cisco INVITE also contains BFCP,
> slides-video, H224, and UDP/IX m-lines.  These are omitted from the test
> scenario because `m=application 0 UDP/BFCP *` (with `*` as the format token)
> can confuse pion/sdp's parser and cause the server to silently drop the INVITE
> before responding.  The dual-PT H264 offer is the only part needed to verify
> the fix.

## Running SIPp manually

If you prefer to drive SIPp yourself (e.g. for live stats with the interactive
UI), generate the media once and then:

```bash
tools/generate_media.sh

sipp ap1sip-dev.daakia.co.in:5060 \
  -sf video_call.xml \
  -s 1111 \
  -t u1 \
  -i <your-local-ip> -mi <your-local-ip> \
  -mp 6000 \
  -m 1 -l 1 -trace_err -trace_msg
```

To also record the received video, run `tcpdump` capturing
`udp portrange 6000-6003` during the call, then:

```bash
tools/extract_received_video.py --pcap capture.pcap --port 6002 --out received_video
```

## How it maps to the request

| Requirement                              | How it's done |
|------------------------------------------|---------------|
| Video SIP call to `1111@…daakia.co.in`   | INVITE with `m=audio` (PCMU) **and** `m=video` H.264 `sendrecv` |
| On answer, after 10 s, send `560545#`    | `<pause milliseconds="10000"/>` then `<exec play_dtmf="560545#"/>` |
| Join the meeting                         | the DTMF (`560545#`) is the meeting/PIN entry |
| Publish a single video                   | `<exec play_pcap_video="media/video.pcap"/>` (one H.264 stream) |
| Receive a mixed video stream             | `m=video` is `sendrecv`; server sends the composite back to port 6002 |
| Record the received video                | `tcpdump` capture → `extract_received_video.py` → `received_video.mp4` |

## Codec / SDP notes

The SDP is tuned to interoperate with the LiveKit-based SIP server:

- audio: `PCMU/8000` (PT 0)
- DTMF: `telephone-event/8000` at **PT 96** — SIPp's `play_dtmf` hard-codes
  RTP payload type 96, so the audio `m`-line advertises 96 for telephone-event.
- video: `H264/90000` (PT 97), `profile-level-id=42e01f` (constrained baseline
  3.1), `packetization-mode=1`, with `a=rtcp-fb:97 nack pli`.

The payload types in the generated pcaps **must** match the SDP — they are set
by `tools/generate_media.sh` (`VIDEO_PT=97`, `AUDIO_PT=0`). SIPp replays the
RTP verbatim and does not rewrite payload types.

## Tuning

- **Recording length:** the scenario stays up for `10s + 4s + 45s ≈ 59s`.
  Change the final `<pause milliseconds="45000"/>` in the `.xml`. Keep it
  shorter than the published video length (`VIDEO_SECONDS`, default 65 s) so we
  keep publishing for the whole call, or regenerate a longer `video.pcap`:
  `VIDEO_SECONDS=120 tools/generate_media.sh`.
- **Use your own published video** instead of the synthetic test pattern: drop
  in any `media/video.pcap` containing H.264 RTP at payload type 97, or adapt
  the `ffmpeg` line in `tools/generate_media.sh` (e.g. `-i myclip.mp4`).
- **DTMF tone length:** `play_dtmf="560545#,500"` sets 500 ms tones.

## Verified live test results

Run against `1111@ap1sip-dev.daakia.co.in` (SIPp 3.7.7-TLS-PCAP):

- Signaling works: `INVITE → 100 → 180 → 200 OK → ACK`, **no digest auth** required
  (use `video_call.xml`).
- The server **negotiates video**: its answer is `m=video <port> RTP/AVP 97`,
  `a=rtpmap:97 H264/90000`, `sendrecv` — i.e. it enables its grid compositor.
- We **publish video** correctly: a sustained H.264 stream leaves our `6002`
  (~1600+ packets per call). The DTMF `560545#` join is accepted (an audio
  prompt is heard right after the digits).
- **Symmetric RTP** is in effect: return media comes back to the exact port we
  send from, so video must be sent from `6002` (it is).

Two important findings baked into the scenario:

1. **SIPp has a single pcap-play engine per call.** Running `play_pcap_video`,
   `play_pcap_audio` and `play_dtmf` *concurrently* corrupts the video (only a
   couple of packets escape). They are therefore sequenced in `video_call.xml`:
   audio keepalive → DTMF join → **then** publish video.
2. **You only receive a mixed video stream if another participant is publishing
   video in the room.** The server composites *remote participants'* tracks; in
   an empty meeting the compositor has nothing to mix and sends no video (the
   received audio was silent after join, confirming an empty room). To capture a
   real recording, make sure at least one other participant is publishing video
   in meeting `560545` while the test runs.

`run_test.sh` produces `received_video.mp4` when video is returned, and always
saves the raw capture (`capture_*.pcap`); you can also pull the received audio
for sanity with the snippet in this folder's history.

## Limitations

- `play_dtmf` / `play_pcap_*` require a PCAP-enabled SIPp build; otherwise SIPp
  errors with *"requires pcap support! Please recompile SIPp"*.
- The bundled depacketizer handles the common H.264 RTP packetizations
  (single-NAL, STAP-A, FU-A). Exotic modes (STAP-B / MTAP / FU-B) are skipped.
- `tcpdump` needs `sudo`; the media-generation tools do not.
- One call at a time (`-m 1 -l 1`); `[auto_media_port]` would step ports for
  concurrent calls if you raise these.
```
