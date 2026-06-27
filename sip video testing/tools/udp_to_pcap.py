#!/usr/bin/env python3
"""Receive RTP/UDP datagrams on a local port and write them to a classic pcap.

This lets us build SIPp-playable RTP pcaps WITHOUT needing tcpdump/root: point
ffmpeg's `-f rtp rtp://127.0.0.1:<port>` output at this listener, and every UDP
datagram is wrapped in synthetic Ethernet/IPv4/UDP headers and appended to the
pcap (DLT_EN10MB) with realistic capture timestamps so SIPp paces the replay
correctly.

Only the Python standard library is used.
"""
import argparse
import socket
import struct
import sys
import time


def ipv4_checksum(header: bytes) -> int:
    if len(header) % 2:
        header += b"\x00"
    total = 0
    for i in range(0, len(header), 2):
        total += (header[i] << 8) | header[i + 1]
    total = (total >> 16) + (total & 0xFFFF)
    total += total >> 16
    return (~total) & 0xFFFF


def build_frame(payload: bytes, src_ip: str, dst_ip: str, src_port: int, dst_port: int) -> bytes:
    # Ethernet II: dst MAC, src MAC, ethertype IPv4.
    eth = b"\x00\x00\x00\x00\x00\x02" + b"\x00\x00\x00\x00\x00\x01" + b"\x08\x00"

    udp_len = 8 + len(payload)
    udp = struct.pack("!HHHH", src_port, dst_port, udp_len, 0) + payload

    total_len = 20 + udp_len
    ihl_ver = (4 << 4) | 5
    ip_no_csum = struct.pack(
        "!BBHHHBBH4s4s",
        ihl_ver, 0, total_len, 0, 0x4000, 64, 17, 0,
        socket.inet_aton(src_ip), socket.inet_aton(dst_ip),
    )
    csum = ipv4_checksum(ip_no_csum)
    ip = ip_no_csum[:10] + struct.pack("!H", csum) + ip_no_csum[12:]
    return eth + ip + udp


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--port", type=int, required=True, help="UDP port to listen on")
    ap.add_argument("--out", required=True, help="output pcap path")
    ap.add_argument("--idle", type=float, default=2.0,
                    help="stop after this many seconds with no packets (after first packet)")
    ap.add_argument("--max", type=float, default=300.0, help="hard cap on capture seconds")
    ap.add_argument("--bind", default="127.0.0.1")
    args = ap.parse_args()

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((args.bind, args.port))
    sock.settimeout(args.idle)

    src_ip, dst_ip = "127.0.0.2", args.bind
    src_port = 40000 + (args.port % 1000)

    count = 0
    start = time.time()
    with open(args.out, "wb") as f:
        # pcap global header: magic, ver 2.4, tz, sigfigs, snaplen, DLT_EN10MB(1).
        f.write(struct.pack("!IHHiIII", 0xA1B2C3D4, 2, 4, 0, 0, 65535, 1))
        print(f"[udp_to_pcap] listening on {args.bind}:{args.port} -> {args.out}", flush=True)
        while True:
            try:
                data, _ = sock.recvfrom(65535)
            except socket.timeout:
                if count > 0:
                    break  # producer finished
                if time.time() - start > args.max:
                    break
                continue
            now = time.time()
            frame = build_frame(data, src_ip, dst_ip, src_port, args.port)
            f.write(struct.pack("!IIII", int(now), int((now % 1) * 1_000_000),
                                len(frame), len(frame)))
            f.write(frame)
            count += 1
            if now - start > args.max:
                break

    print(f"[udp_to_pcap] wrote {count} packets to {args.out}", flush=True)
    return 0 if count > 0 else 1


if __name__ == "__main__":
    sys.exit(main())
