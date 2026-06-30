#!/usr/bin/env bash
#
# Run the video SIP test end-to-end:
#   1. generate the RTP pcaps (once),
#   2. start tcpdump to capture the call (so we can record the received video),
#   3. place the call with SIPp (publish video + DTMF join),
#   4. stop the capture and reconstruct the received video into an .mp4.
#   5. for the Cisco DX-80 scenario: verify the server echoed H.264 capacity
#      params (max-mbps, max-fs, etc.) in the 200 OK answer.
#
# Override any setting via environment variables, e.g.:
#   LOCAL_IP=192.168.1.50 TRANSPORT=t1 ./run_test.sh
#   SCENARIO=video_call_cisco_dx80.xml ./run_test.sh
#
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

# ---- configuration -------------------------------------------------------
REMOTE_HOST="${REMOTE_HOST:-ap1sip-dev.daakia.co.in}"
REMOTE_PORT="${REMOTE_PORT:-5060}"
SERVICE="${SERVICE:-1111}"                 # the number we dial
TRANSPORT="${TRANSPORT:-u1}"               # u1=UDP, t1=TCP, l1=TLS
SCENARIO="${SCENARIO:-video_call.xml}"     # or video_call_auth.xml / video_call_cisco_dx80.xml
MEDIA_PORT="${MEDIA_PORT:-6000}"           # -mp base; audio=6000, video=6002
CAP_IFACE="${CAP_IFACE:-en0}"             # interface to capture on
OUT_BASENAME="${OUT_BASENAME:-received_video}"
# Optional digest credentials (only used by video_call_auth.xml):
AUTH_USER="${AUTH_USER:-}"
AUTH_PASS="${AUTH_PASS:-}"

VIDEO_PORT=$((MEDIA_PORT + 2))
STAMP="$(date +%Y%m%d_%H%M%S)"
CAPTURE="capture_${STAMP}.pcap"
SCENARIO_STEM="${SCENARIO%.xml}"           # e.g. video_call_cisco_dx80

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
NEED_MEDIA=0
[ ! -f media/video.pcap ]      && NEED_MEDIA=1
[ ! -f media/audio.pcap ]      && NEED_MEDIA=1
# The Cisco DX-80 scenario plays back video at PT 126.
if [ "$SCENARIO" = "video_call_cisco_dx80.xml" ] && [ ! -f media/video_pt126.pcap ]; then
  NEED_MEDIA=1
fi
if [ "$NEED_MEDIA" = "1" ]; then
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
# -trace_msg writes all SIP messages to ${SCENARIO_STEM}_<pid>_messages.log,
# which we parse below to verify the server's 200 OK answer.
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

# ---- 5. verify 200 OK answer (Cisco DX-80 scenario) ---------------------
# For the Cisco DX-80 scenario we check that the server's 200 OK answer
# includes the H.264 capacity params that were offered by "Cisco".
# Without these params the Cisco endpoint shows a blank video tile.
if [ "$SCENARIO" = "video_call_cisco_dx80.xml" ]; then
  echo
  echo "== verifying Cisco DX-80 answer SDP =="

  # Find the most-recently-written SIPp messages log for this scenario stem.
  MSG_LOG="$(ls -t ${SCENARIO_STEM}_*_messages.log 2>/dev/null | head -1 || true)"

  if [ -z "$MSG_LOG" ]; then
    echo "  WARN: no SIPp message log found (${SCENARIO_STEM}_*_messages.log) — skipping answer check"
  else
    echo "  message log : ${MSG_LOG}"
    PASS=1

    check_param() {
      local param="$1"
      # Search only inside 200 OK responses for the param.
      if grep -A 50 "^SIP/2.0 200" "$MSG_LOG" | grep -q "$param"; then
        echo "  [PASS] answer contains $param"
      else
        echo "  [FAIL] answer is MISSING $param  ← server did not echo capacity param"
        PASS=0
      fi
    }

    # PT 126 must be selected (not PT 97).
    if grep -A 50 "^SIP/2.0 200" "$MSG_LOG" | grep -q "a=rtpmap:126 H264"; then
      echo "  [PASS] server selected PT 126 (packetization-mode=1)"
    else
      echo "  [FAIL] server did NOT select PT 126 — check selectH264() in video_sdp.go"
      PASS=0
    fi

    check_param "max-br="
    check_param "max-mbps="
    check_param "max-fs="
    check_param "max-dpb="
    check_param "max-smbps="

    if [ "$PASS" = "1" ]; then
      echo
      echo "  RESULT: PASS — server answer is Cisco-compatible (blank tile fix verified)"
    else
      echo
      echo "  RESULT: FAIL — see [FAIL] lines above"
    fi
  fi
fi

echo
echo "== done =="
echo "  signaling/media capture : ${CAPTURE}"
echo "  recorded received video : ${OUT_BASENAME}.mp4 (and ${OUT_BASENAME}.h264)"
