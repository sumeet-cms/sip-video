#!/usr/bin/env bash
#
# Generate the RTP pcaps that SIPp replays:
#   media/video.pcap : H.264 (PT 97, 90kHz) - the single video we publish
#   media/audio.pcap : PCMU  (PT 0,  8kHz)  - keeps the audio leg alive
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
VIDEO_PT="${VIDEO_PT:-97}"
AUDIO_PT="${AUDIO_PT:-0}"

command -v ffmpeg >/dev/null 2>&1 || { echo "ERROR: ffmpeg not found in PATH" >&2; exit 1; }

echo ">> Generating media/video.pcap (${VIDEO_SECONDS}s H.264, PT ${VIDEO_PT}) ..."
python3 tools/udp_to_pcap.py --port 7000 --out media/video.pcap --max $((VIDEO_SECONDS + 10)) &
VPID=$!
sleep 0.7
# Constrained-baseline-ish stream (profile-level-id 42e01f), 1 keyframe/sec,
# no B-frames, packets capped near the 1200-byte MTU we advertise.
ffmpeg -hide_banner -loglevel error -re \
  -f lavfi -i "testsrc=size=640x480:rate=30" \
  -t "$VIDEO_SECONDS" \
  -c:v libx264 -profile:v baseline -level 3.1 -pix_fmt yuv420p \
  -g 30 -keyint_min 30 -x264-params "scenecut=0:bframes=0" -an \
  -payload_type "$VIDEO_PT" -ssrc 22222 \
  -f rtp "rtp://127.0.0.1:7000?pkt_size=1200&rtcpport=7001"
wait "$VPID"

echo ">> Generating media/audio.pcap (${AUDIO_SECONDS}s PCMU, PT ${AUDIO_PT}) ..."
python3 tools/udp_to_pcap.py --port 7002 --out media/audio.pcap --max $((AUDIO_SECONDS + 10)) &
APID=$!
sleep 0.7
ffmpeg -hide_banner -loglevel error -re \
  -f lavfi -i "sine=frequency=440:duration=${AUDIO_SECONDS}" \
  -ar 8000 -ac 1 -c:a pcm_mulaw \
  -payload_type "$AUDIO_PT" -ssrc 11111 \
  -f rtp "rtp://127.0.0.1:7002?pkt_size=160&rtcpport=7003"
wait "$APID"

echo ">> Done:"
ls -lh media/video.pcap media/audio.pcap
