#!/usr/bin/env python3
"""Extract and record the RECEIVED H.264 video from a captured pcap.

SIPp cannot write inbound RTP to disk, so run_test.sh captures the call with
tcpdump and this script reconstructs the mixed/composited video the server
sent back to us:

  1. read the pcap (classic libpcap; DLT EN10MB / NULL / RAW),
  2. keep UDP packets on the local video RTP port,
  3. de-packetize H.264 (RFC 6184: single NAL, STAP-A, FU-A) in RTP-sequence
     order into an Annex-B .h264 elementary stream,
  4. if ffmpeg is available, mux it into a playable .mp4.

Pure standard library (plus optional ffmpeg for the final mux).
"""
import argparse
import os
import shutil
import struct
import subprocess
import sys

STARTCODE = b"\x00\x00\x00\x01"
RTCP_PT_RANGE = range(72, 77)  # 200-204 with marker bit -> 72..76 in pt field


def read_pcap(path):
    """Yield raw link-layer frames from a classic pcap file."""
    with open(path, "rb") as f:
        gh = f.read(24)
        if len(gh) < 24:
            raise ValueError("file too short to be a pcap")
        magic = gh[:4]
        if magic in (b"\xa1\xb2\xc3\xd4", b"\xa1\xb2\x3c\x4d"):
            endian = ">"
        elif magic in (b"\xd4\xc3\xb2\xa1", b"\x4d\x3c\xb2\xa1"):
            endian = "<"
        elif magic == b"\x0a\x0d\x0d\x0a":
            raise ValueError("this is a pcapng file; re-capture with classic pcap "
                             "(tcpdump -w writes classic pcap by default)")
        else:
            raise ValueError(f"unrecognised pcap magic {magic!r}")
        linktype = struct.unpack(endian + "I", gh[20:24])[0]
        while True:
            ph = f.read(16)
            if len(ph) < 16:
                break
            _, _, incl, _ = struct.unpack(endian + "IIII", ph)
            data = f.read(incl)
            if len(data) < incl:
                break
            yield linktype, data


def l3_from_link(linktype, frame):
    """Return the IPv4 payload (skipping the link layer), or None."""
    if linktype == 1:  # EN10MB
        if len(frame) < 14:
            return None
        eth_type = struct.unpack("!H", frame[12:14])[0]
        if eth_type == 0x0800:
            return frame[14:]
        if eth_type == 0x8100 and len(frame) >= 18:  # 802.1Q VLAN
            inner = struct.unpack("!H", frame[16:18])[0]
            return frame[18:] if inner == 0x0800 else None
        return None
    if linktype == 0:  # NULL/loopback: 4-byte address family
        if len(frame) < 4:
            return None
        fam = struct.unpack("=I", frame[:4])[0]
        return frame[4:] if fam in (2,) else None
    if linktype in (101, 12, 14):  # RAW IP
        return frame
    if linktype == 113:  # Linux SLL
        return frame[16:] if len(frame) > 16 else None
    return None


def parse_udp(ip_pkt):
    """Return (src_port, dst_port, payload) for a UDP/IPv4 packet, else None."""
    if len(ip_pkt) < 20:
        return None
    if (ip_pkt[0] >> 4) != 4:
        return None
    ihl = (ip_pkt[0] & 0x0F) * 4
    if ip_pkt[9] != 17:  # not UDP
        return None
    udp = ip_pkt[ihl:]
    if len(udp) < 8:
        return None
    src_port, dst_port, ulen, _ = struct.unpack("!HHHH", udp[:8])
    payload = udp[8:ulen] if 8 <= ulen <= len(udp) else udp[8:]
    return src_port, dst_port, payload


def parse_rtp(payload):
    """Return (seq, timestamp, pt, body) for an RTP packet, else None."""
    if len(payload) < 12:
        return None
    b0 = payload[0]
    if (b0 >> 6) != 2:  # version must be 2
        return None
    cc = b0 & 0x0F
    has_ext = (b0 >> 4) & 0x01
    pt = payload[1] & 0x7F
    if pt in RTCP_PT_RANGE:  # RTCP, not media
        return None
    seq, ts = struct.unpack("!HI", payload[2:8])
    offset = 12 + cc * 4
    if has_ext:
        if len(payload) < offset + 4:
            return None
        ext_len = struct.unpack("!H", payload[offset + 2:offset + 4])[0]
        offset += 4 + ext_len * 4
    if offset > len(payload):
        return None
    return seq, ts, pt, payload[offset:]


def depacketize_h264(packets):
    """packets: list of (ext_seq, body). Return Annex-B bytes.

    ext_seq is the unwrapped (monotonically increasing) RTP sequence number.
    When a sequence-number gap is detected while a FU-A assembly is in
    progress the incomplete NAL is *discarded* rather than emitted — emitting
    a NAL with missing interior bytes produces a syntactically invalid H.264
    bitstream that causes decoders to fail for all subsequent frames.
    """
    out = bytearray()
    fu_buf = bytearray()
    fu_active = False
    prev_seq = None
    gaps_reset = 0
    for ext_seq, body in packets:
        # Detect packet loss: any gap in the sequence number means one or more
        # RTP packets were lost.  If a FU-A reassembly is in progress those
        # missing packets are almost certainly FU-A middle/end fragments, so
        # the NAL unit is incomplete — discard it to keep the bitstream clean.
        if prev_seq is not None and ext_seq != prev_seq + 1:
            if fu_active:
                fu_active = False
                fu_buf = bytearray()
                gaps_reset += 1
        prev_seq = ext_seq

        if not body:
            continue
        nal_type = body[0] & 0x1F
        if 1 <= nal_type <= 23:
            out += STARTCODE + body
        elif nal_type == 24:  # STAP-A: aggregation of several NALs
            i = 1
            while i + 2 <= len(body):
                size = struct.unpack("!H", body[i:i + 2])[0]
                i += 2
                if size == 0 or i + size > len(body):
                    break
                out += STARTCODE + body[i:i + size]
                i += size
        elif nal_type == 28:  # FU-A: fragmented NAL
            if len(body) < 2:
                continue
            fu_header = body[1]
            start = fu_header & 0x80
            end = fu_header & 0x40
            frag_type = fu_header & 0x1F
            if start:
                nal_hdr = (body[0] & 0xE0) | frag_type
                fu_buf = bytearray([nal_hdr])
                fu_buf += body[2:]
                fu_active = True
            elif fu_active:
                fu_buf += body[2:]
            if end and fu_active:
                out += STARTCODE + bytes(fu_buf)
                fu_active = False
                fu_buf = bytearray()
        # nal_type 25-27/29 (STAP-B/MTAP/FU-B) are uncommon for SIP; skipped.
    if gaps_reset:
        print(f"WARNING: {gaps_reset} incomplete FU-A NAL(s) discarded due to "
              "RTP packet loss (sequence gaps); affected frames will be missing "
              "but the bitstream remains valid.")
    return bytes(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--pcap", required=True, help="captured pcap from the call")
    ap.add_argument("--port", type=int, required=True,
                    help="local video RTP port (m=video port we advertised)")
    ap.add_argument("--out", default="received_video",
                    help="output basename (writes <out>.h264 and <out>.mp4)")
    ap.add_argument("--direction", choices=["in", "out", "both"], default="in",
                    help="in = packets arriving at our port (received video)")
    ap.add_argument("--fps", type=float, default=0,
                    help="override playback fps for the muxed mp4 "
                         "(default: auto-detect from RTP timestamps)")
    ap.add_argument("--rtp-clock-rate", type=int, default=90000,
                    help="RTP clock rate for the video stream (default: 90000)")
    args = ap.parse_args()

    if not os.path.exists(args.pcap):
        print(f"ERROR: pcap not found: {args.pcap}", file=sys.stderr)
        return 1

    by_ssrc = {}
    total = 0
    for linktype, frame in read_pcap(args.pcap):
        ip_pkt = l3_from_link(linktype, frame)
        if ip_pkt is None:
            continue
        udp = parse_udp(ip_pkt)
        if udp is None:
            continue
        src_port, dst_port, payload = udp
        if args.direction == "in" and dst_port != args.port:
            continue
        if args.direction == "out" and src_port != args.port:
            continue
        if args.direction == "both" and args.port not in (src_port, dst_port):
            continue
        rtp = parse_rtp(payload)
        if rtp is None:
            continue
        seq, ts, pt, body = rtp
        ssrc = struct.unpack("!I", payload[8:12])[0]
        # Store (seq, rtp_timestamp, payload_body) so we can derive real fps.
        by_ssrc.setdefault(ssrc, []).append((seq, ts, body))
        total += 1

    if not by_ssrc:
        print(f"No RTP video packets found on port {args.port} in {args.pcap}.",
              file=sys.stderr)
        print("Check: did the call connect, is --port the m=video port (-mp + 2), "
              "and did tcpdump run with sufficient privileges?", file=sys.stderr)
        return 2

    # Pick the SSRC with the most packets (the active inbound video stream).
    ssrc = max(by_ssrc, key=lambda s: len(by_ssrc[s]))
    pkts = by_ssrc[ssrc]
    print(f"Found {total} RTP packets across {len(by_ssrc)} SSRC(s); "
          f"using SSRC 0x{ssrc:08x} with {len(pkts)} packets.")

    # Reorder by sequence number relative to the first packet, unwrapping the
    # 16-bit wrap so a stream that crosses 65535->0 stays in order.
    base_seq = pkts[0][0]
    ordered = sorted(pkts, key=lambda p: ((p[0] - base_seq) & 0xFFFF))

    # Derive the real frame rate from RTP timestamps.
    # H.264 RTP clock is 90 000 Hz.  We count how many unique timestamps appear
    # (= frames) and how many clock ticks they span, then compute fps.
    detected_fps = 15.0  # safe fallback
    unique_ts = sorted({p[1] for p in ordered})
    if args.fps > 0:
        detected_fps = args.fps
        print(f"Using user-supplied fps: {detected_fps:.3f}")
    elif len(unique_ts) >= 2:
        # Unwrap 32-bit RTP timestamp rollover.
        ts_span = (unique_ts[-1] - unique_ts[0]) & 0xFFFFFFFF
        num_frames = len(unique_ts)
        if ts_span > 0:
            # Average ticks per frame → fps.
            ticks_per_frame = ts_span / (num_frames - 1)
            detected_fps = args.rtp_clock_rate / ticks_per_frame
            duration_s = ts_span / args.rtp_clock_rate
            print(f"RTP timestamps: first={unique_ts[0]}, last={unique_ts[-1]}, "
                  f"span={ts_span} ticks ({duration_s:.1f} s), "
                  f"unique frames={num_frames}, "
                  f"detected fps={detected_fps:.2f}")
        else:
            print("WARNING: all packets have the same RTP timestamp; "
                  f"falling back to fps={detected_fps}")
    else:
        print(f"WARNING: too few unique timestamps to detect fps; "
              f"falling back to {detected_fps}")

    # Round fps to a sensible value (e.g. 14.97 → 15).
    rounded_fps = round(detected_fps)
    if abs(detected_fps - rounded_fps) / max(detected_fps, 1) < 0.05:
        detected_fps = float(rounded_fps)
    fps_str = f"{detected_fps:.3f}".rstrip("0").rstrip(".")

    # Compute monotonically-increasing (unwrapped) sequence numbers so the
    # depacketizer can detect gaps caused by RTP packet loss.  The raw RTP
    # sequence is 16-bit and wraps at 65535; the sort above already handles
    # the wrap for ordering, but the raw values would produce a false "gap"
    # (e.g. 65535 → 0 looks like a gap of 65535).
    ext_packets = []
    ext_seq = 0
    prev_raw_seq = None
    for p in ordered:
        raw_seq = p[0]
        if prev_raw_seq is not None:
            diff = (raw_seq - prev_raw_seq) & 0xFFFF
            # diff > 0x8000 would mean a large backwards jump — treat as 0
            # (duplicate or reordered beyond the jitter window; skip).
            if diff <= 0x8000:
                ext_seq += diff
        prev_raw_seq = raw_seq
        ext_packets.append((ext_seq, p[2]))

    annexb = depacketize_h264(ext_packets)

    h264_path = args.out + ".h264"
    with open(h264_path, "wb") as f:
        f.write(annexb)
    print(f"Wrote raw Annex-B stream: {h264_path} ({len(annexb)} bytes)")

    ffmpeg = shutil.which("ffmpeg")
    if ffmpeg:
        mp4_path = args.out + ".mp4"
        # Use the detected fps so the mp4 plays at real-time speed regardless
        # of the configured server frame rate.  -fflags +genpts regenerates PTS
        # from DTS (needed when the Annex-B stream lacks timing info).
        # +genpts regenerates PTS from DTS (needed for Annex-B streams without
        # timing info).  +discardcorrupt tells the demuxer to drop any corrupt
        # packets rather than passing them into the muxer, which prevents a
        # single bad frame from making the rest of the recording unplayable.
        cmd = [ffmpeg, "-y", "-hide_banner", "-loglevel", "warning",
               "-fflags", "+genpts+discardcorrupt", "-r", fps_str, "-i", h264_path,
               "-c", "copy", mp4_path]
        print(f"Muxing to mp4 at {fps_str} fps:", " ".join(cmd))
        rc = subprocess.run(cmd).returncode
        if rc == 0:
            print(f"Wrote {mp4_path}")
        else:
            print("ffmpeg mux failed; the raw .h264 is still usable "
                  f"(try: ffmpeg -framerate {fps_str} -i {h264_path} {args.out}.mp4)",
                  file=sys.stderr)
    else:
        print("ffmpeg not found; play the raw stream with:\n"
              f"  ffmpeg -framerate {fps_str} -i {h264_path} {args.out}.mp4")
    return 0


if __name__ == "__main__":
    sys.exit(main())
