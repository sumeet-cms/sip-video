#!/usr/bin/env bash
#
# Run the video SIP test end-to-end:
#   1. generate the RTP pcaps (once),
#   2. start tcpdump to capture the call (so we can record the received video),
#   3. place the call with SIPp (publish video + DTMF join),
#   4. stop the capture and reconstruct the received video into an .mp4.
#
# Override any setting via environment variables, e.g.:
#   LOCAL_IP=192.168.1.50 TRANSPORT=t1 ./run_test.sh
#
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

# ---- configuration -------------------------------------------------------
REMOTE_HOST="${REMOTE_HOST:-ap1sip-dev.daakia.co.in}"
REMOTE_PORT="${REMOTE_PORT:-5060}"
SERVICE="${SERVICE:-1111}"                 # the number we dial
TRANSPORT="${TRANSPORT:-u1}"               # u1=UDP, t1=TCP, l1=TLS
SCENARIO="${SCENARIO:-video_call.xml}"     # or video_call_auth.xml
MEDIA_PORT="${MEDIA_PORT:-6000}"           # -mp base; audio=6000, video=6002
CAP_IFACE="${CAP_IFACE:-en0}"             # interface to capture on
OUT_BASENAME="${OUT_BASENAME:-received_video}"
# Optional digest credentials (only used by video_call_auth.xml):
AUTH_USER="${AUTH_USER:-}"
AUTH_PASS="${AUTH_PASS:-}"

VIDEO_PORT=$((MEDIA_PORT + 2))
STAMP="$(date +%Y%m%d_%H%M%S)"
CAPTURE="capture_${STAMP}.pcap"

# Best-effort local IP detection (macOS first, then Linux).
LOCAL_IP="${LOCAL_IP:-$(ipconfig getifaddr "$CAP_IFACE" 2>/dev/null || true)}"
if [ -z "$LOCAL_IP" ]; then
  LOCAL_IP="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
fi
if [ -z "$LOCAL_IP" ]; then
  echo "ERROR: could not auto-detect LOCAL_IP. Set it: LOCAL_IP=x.x.x.x $0" >&2
  exit 1
fi

command -v sipp >/dev/null 2>&1 || { echo "ERROR: sipp not found in PATH (see README)." >&2; exit 1; }

echo "== config =="
echo "  target     : ${SERVICE}@${REMOTE_HOST}:${REMOTE_PORT} (${TRANSPORT})"
echo "  scenario   : ${SCENARIO}"
echo "  local IP   : ${LOCAL_IP}"
echo "  media base : ${MEDIA_PORT} (audio ${MEDIA_PORT} / video ${VIDEO_PORT})"
echo "  capture    : ${CAPTURE} on ${CAP_IFACE}"
echo

# ---- 1. media ------------------------------------------------------------
if [ ! -f media/video.pcap ] || [ ! -f media/audio.pcap ]; then
  echo "== generating media pcaps =="
  tools/generate_media.sh
  echo
fi

# ---- 2. capture ----------------------------------------------------------
echo "== starting capture (tcpdump needs sudo) =="
sudo tcpdump -i "$CAP_IFACE" -s 0 -U -w "$CAPTURE" \
  "udp and portrange ${MEDIA_PORT}-$((MEDIA_PORT + 3))" &
TCPDUMP_PID=$!
# tcpdump runs under sudo; remember to tear it down even on error.
cleanup() { sudo kill "$TCPDUMP_PID" 2>/dev/null || true; }
trap cleanup EXIT
sleep 1.5

# ---- 3. place the call ---------------------------------------------------
echo "== placing call with SIPp =="
SIPP_AUTH=()
if [ -n "$AUTH_USER" ]; then SIPP_AUTH+=(-au "$AUTH_USER"); fi
if [ -n "$AUTH_PASS" ]; then SIPP_AUTH+=(-ap "$AUTH_PASS"); fi

# NOTE: SIPp's play_pcap_*/play_dtmf send RTP via a raw socket, which requires
# root. We therefore run sipp under sudo as well.
set +e
sudo sipp "${REMOTE_HOST}:${REMOTE_PORT}" \
  -sf "$SCENARIO" \
  -s "$SERVICE" \
  -t "$TRANSPORT" \
  -i "$LOCAL_IP" \
  -mi "$LOCAL_IP" \
  -mp "$MEDIA_PORT" \
  -m 1 -l 1 \
  -trace_err -trace_msg \
  ${SIPP_AUTH[@]+"${SIPP_AUTH[@]}"} \
  ${SIPP_EXTRA:-}
SIPP_RC=$?
set -e
echo "SIPp exited with code ${SIPP_RC}"

# ---- 4. stop capture + record video -------------------------------------
sleep 1
cleanup
trap - EXIT
wait "$TCPDUMP_PID" 2>/dev/null || true

echo
echo "== reconstructing received video =="
tools/extract_received_video.py --pcap "$CAPTURE" --port "$VIDEO_PORT" \
  --direction in --out "$OUT_BASENAME" || true

echo
echo "== done =="
echo "  signaling/media capture : ${CAPTURE}"
echo "  recorded received video : ${OUT_BASENAME}.mp4 (and ${OUT_BASENAME}.h264)"
