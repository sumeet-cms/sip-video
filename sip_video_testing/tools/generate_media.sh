#!/usr/bin/env bash
#
# Generate the RTP pcaps that SIPp replays:
#   media/video.pcap      : H.264 (PT 97,  90kHz) - generic scenario (video_call.xml)
#   media/video_pt126.pcap: H.264 (PT 126, 90kHz) - Cisco DX-80 scenario (video_call_cisco_dx80.xml)
#   media/audio.pcap      : PCMU  (PT 0,   8kHz)  - keeps the audio leg alive
#
# Uses ffmpeg to emit real RTP and a tiny pure-python listener to capture it
# into a pcap (no tcpdump / root needed). Payload types here MUST match the
# SDP in the scenario .xml files.
#
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"
mkdir -p media

VIDEO_SECONDS="${VIDEO_SECONDS:-65}"
AUDIO_SECONDS="${AUDIO_SECONDS:-9}"
AUDIO_PT="${AUDIO_PT:-0}"

command -v ffmpeg >/dev/null 2>&1 || { echo "ERROR: ffmpeg not found in PATH" >&2; exit 1; }

# ---- shared ffmpeg encoder options for H.264 ---------------------------------
# Constrained-baseline stream (profile-level-id 428014 ≈ Main level 2.0),
# 1 keyframe/sec, no B-frames, packets capped near the 1200-byte MTU.
h264_opts() {
  echo "-c:v libx264 -profile:v baseline -level 3.1 -pix_fmt yuv420p \
        -g 30 -keyint_min 30 -x264-params scenecut=0:bframes=0 -an"
}

# ---- media/video.pcap (PT 97 — generic / video_call.xml) --------------------
if [ "${REGEN_VIDEO:-0}" = "1" ] || [ ! -f media/video.pcap ]; then
  echo ">> Generating media/video.pcap (${VIDEO_SECONDS}s H.264, PT 97) ..."
  python3 tools/udp_to_pcap.py --port 7000 --out media/video.pcap \
    --max $((VIDEO_SECONDS + 10)) &
  VPID=$!
  sleep 0.7
  # shellcheck disable=SC2046
  ffmpeg -hide_banner -loglevel error -re \
    -f lavfi -i "testsrc=size=640x480:rate=30" \
    -t "$VIDEO_SECONDS" \
    $(h264_opts) \
    -payload_type 97 -ssrc 22222 \
    -f rtp "rtp://127.0.0.1:7000?pkt_size=1200&rtcpport=7001"
  wait "$VPID"
else
  echo ">> media/video.pcap already exists (set REGEN_VIDEO=1 to regenerate)"
fi

# ---- media/video_pt126.pcap (PT 126 — Cisco DX-80 / video_call_cisco_dx80.xml) --
# Same synthetic content, but uses RTP payload type 126, matching the
# packetization-mode=1 PT that the server selects from a Cisco DX-80 INVITE.
if [ "${REGEN_VIDEO:-0}" = "1" ] || [ ! -f media/video_pt126.pcap ]; then
  echo ">> Generating media/video_pt126.pcap (${VIDEO_SECONDS}s H.264, PT 126) ..."
  python3 tools/udp_to_pcap.py --port 7004 --out media/video_pt126.pcap \
    --max $((VIDEO_SECONDS + 10)) &
  V126PID=$!
  sleep 0.7
  # shellcheck disable=SC2046
  ffmpeg -hide_banner -loglevel error -re \
    -f lavfi -i "testsrc=size=640x480:rate=30" \
    -t "$VIDEO_SECONDS" \
    $(h264_opts) \
    -payload_type 126 -ssrc 33333 \
    -f rtp "rtp://127.0.0.1:7004?pkt_size=1200&rtcpport=7005"
  wait "$V126PID"
else
  echo ">> media/video_pt126.pcap already exists (set REGEN_VIDEO=1 to regenerate)"
fi

# ---- media/audio.pcap (PT 0, PCMU) ------------------------------------------
if [ "${REGEN_AUDIO:-0}" = "1" ] || [ ! -f media/audio.pcap ]; then
  echo ">> Generating media/audio.pcap (${AUDIO_SECONDS}s PCMU, PT ${AUDIO_PT}) ..."
  python3 tools/udp_to_pcap.py --port 7002 --out media/audio.pcap \
    --max $((AUDIO_SECONDS + 10)) &
  APID=$!
  sleep 0.7
  ffmpeg -hide_banner -loglevel error -re \
    -f lavfi -i "sine=frequency=440:duration=${AUDIO_SECONDS}" \
    -ar 8000 -ac 1 -c:a pcm_mulaw \
    -payload_type "$AUDIO_PT" -ssrc 11111 \
    -f rtp "rtp://127.0.0.1:7002?pkt_size=160&rtcpport=7003"
  wait "$APID"
else
  echo ">> media/audio.pcap already exists (set REGEN_AUDIO=1 to regenerate)"
fi

echo ">> Done:"
ls -lh media/video.pcap media/video_pt126.pcap media/audio.pcap
