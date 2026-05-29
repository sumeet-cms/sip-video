// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"math"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"

	prtp "github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	msdkrtp "github.com/livekit/media-sdk/rtp"

	"github.com/livekit/sip/pkg/config"
	"github.com/livekit/sip/pkg/media/video"
)

// videoConfigFrom converts the YAML video config into the compositor config,
// applying defaults for any unset fields.
func videoConfigFrom(c config.VideoConfig) video.Config {
	return video.Config{
		Width:            c.Width,
		Height:           c.Height,
		FPS:              c.FPS,
		BitrateKbps:      c.BitrateKbps,
		KeyFrameInterval: c.KeyFrameInterval,
	}.WithDefaults()
}

const (
	// VideoClockRate is the RTP clock rate for H.264.
	VideoClockRate = video.H264ClockRate
	// videoMTU is the maximum RTP payload size for outgoing video packets.
	videoMTU = 1200
	// h264SDPName is the SDP encoding name for H.264.
	h264SDPName = "H264"
	// videoSampleBufferMaxLate is the max reordering window for inbound video.
	videoSampleBufferMaxLate = 100
)

// videoMediaConf holds the negotiated SIP video parameters.
type videoMediaConf struct {
	Type              byte           // negotiated RTP payload type
	ClockRate         int            // RTP clock rate (90000)
	ProfileLevelID    string         // H.264 profile-level-id from fmtp, if any
	PacketizationMode int            // H.264 packetization-mode from fmtp (default 1)
	Local             netip.AddrPort // our local video RTP address
	Remote            netip.AddrPort // remote video RTP address
}

// VideoStats tracks video traffic for a single call.
type VideoStats struct {
	OutFrames    atomic.Uint64
	OutKeyFrames atomic.Uint64
	OutPackets   atomic.Uint64
	OutBytes     atomic.Uint64

	InPackets atomic.Uint64
	InBytes   atomic.Uint64
	InFrames  atomic.Uint64

	// Compositor (room side).
	CompositorInputs atomic.Int64
}

type VideoStatsSnapshot struct {
	OutFrames        uint64 `json:"out_frames"`
	OutKeyFrames     uint64 `json:"out_key_frames"`
	OutPackets       uint64 `json:"out_packets"`
	OutBytes         uint64 `json:"out_bytes"`
	InPackets        uint64 `json:"in_packets"`
	InBytes          uint64 `json:"in_bytes"`
	InFrames         uint64 `json:"in_frames"`
	CompositorInputs int64  `json:"compositor_inputs"`
}

func (s *VideoStats) Load() VideoStatsSnapshot {
	if s == nil {
		return VideoStatsSnapshot{}
	}
	return VideoStatsSnapshot{
		OutFrames:        s.OutFrames.Load(),
		OutKeyFrames:     s.OutKeyFrames.Load(),
		OutPackets:       s.OutPackets.Load(),
		OutBytes:         s.OutBytes.Load(),
		InPackets:        s.InPackets.Load(),
		InBytes:          s.InBytes.Load(),
		InFrames:         s.InFrames.Load(),
		CompositorInputs: s.CompositorInputs.Load(),
	}
}

// videoSampleWriter accepts encoded H.264 access units for relay to SIP.
type videoSampleWriter interface {
	WriteVideoSample(video.Sample) error
}

// gatedVideoWriter wraps a videoSampleWriter with an enable flag and supports
// hot-swapping the underlying writer, mirroring the audio msdk.SwitchWriter.
type gatedVideoWriter struct {
	enabled atomic.Bool
	mu      sync.Mutex
	w       videoSampleWriter
}

func (g *gatedVideoWriter) Enable()  { g.enabled.Store(true) }
func (g *gatedVideoWriter) Disable() { g.enabled.Store(false) }

func (g *gatedVideoWriter) Swap(w videoSampleWriter) videoSampleWriter {
	g.mu.Lock()
	defer g.mu.Unlock()
	old := g.w
	g.w = w
	return old
}

func (g *gatedVideoWriter) WriteVideoSample(s video.Sample) error {
	if !g.enabled.Load() {
		return nil
	}
	g.mu.Lock()
	w := g.w
	g.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.WriteVideoSample(s)
}

// h264RTPWriter packetizes encoded H.264 access units (Annex-B) into RTP and
// writes them to the SIP video stream.
type h264RTPWriter struct {
	w         msdkrtp.WriteStream
	payloader *codecs.H264Payloader
	pt        uint8
	ssrc      uint32
	clockRate uint32
	frameDur  uint32 // fallback timestamp increment when sample duration is unknown
	stats     *VideoStats

	mu      sync.Mutex
	seq     uint16
	ts      uint32
	started bool
}

func newH264RTPWriter(w msdkrtp.WriteStream, pt uint8, clockRate, fps int, stats *VideoStats) *h264RTPWriter {
	if clockRate <= 0 {
		clockRate = VideoClockRate
	}
	if fps <= 0 {
		fps = video.DefaultFPS
	}
	return &h264RTPWriter{
		w:         w,
		payloader: &codecs.H264Payloader{},
		pt:        pt,
		ssrc:      rand.Uint32(),
		clockRate: uint32(clockRate),
		frameDur:  uint32(clockRate / fps),
		stats:     stats,
		seq:       uint16(rand.Uint32()),
		ts:        rand.Uint32(),
	}
}

func (h *h264RTPWriter) WriteVideoSample(s video.Sample) error {
	if len(s.Data) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.started {
		inc := h.frameDur
		if s.Duration > 0 {
			inc = uint32(math.Round(s.Duration.Seconds() * float64(h.clockRate)))
		}
		if inc == 0 {
			inc = h.frameDur
		}
		h.ts += inc
	}
	h.started = true

	payloads := h.payloader.Payload(videoMTU, s.Data)
	for i, pl := range payloads {
		hdr := &prtp.Header{
			Version:        2,
			PayloadType:    h.pt,
			SequenceNumber: h.seq,
			Timestamp:      h.ts,
			SSRC:           h.ssrc,
			Marker:         i == len(payloads)-1,
		}
		n, err := h.w.WriteRTP(hdr, pl)
		if err != nil {
			return err
		}
		h.seq++
		if h.stats != nil {
			h.stats.OutPackets.Add(1)
			h.stats.OutBytes.Add(uint64(n))
		}
	}
	if h.stats != nil {
		h.stats.OutFrames.Add(1)
		if s.KeyFrame {
			h.stats.OutKeyFrames.Add(1)
		}
	}
	return nil
}

// h264StreamIn depacketizes inbound SIP H.264 RTP into access units and
// forwards them to onSample (typically a LiveKit video track writer).
type h264StreamIn struct {
	mu       sync.Mutex
	sb       *samplebuilder.SampleBuilder
	onSample func(media.Sample)
	stats    *VideoStats
}

func newH264StreamIn(clockRate int, onSample func(media.Sample), stats *VideoStats) *h264StreamIn {
	if clockRate <= 0 {
		clockRate = VideoClockRate
	}
	return &h264StreamIn{
		sb:       samplebuilder.New(videoSampleBufferMaxLate, &codecs.H264Packet{}, uint32(clockRate)),
		onSample: onSample,
		stats:    stats,
	}
}

func (h *h264StreamIn) String() string { return "H264Depay" }

func (h *h264StreamIn) HandleRTP(hdr *prtp.Header, payload []byte) error {
	if h.stats != nil {
		h.stats.InPackets.Add(1)
		h.stats.InBytes.Add(uint64(len(payload)))
	}
	pkt := &prtp.Packet{Header: *hdr, Payload: slices.Clone(payload)}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.sb.Push(pkt)
	for {
		s := h.sb.Pop()
		if s == nil {
			break
		}
		if h.stats != nil {
			h.stats.InFrames.Add(1)
		}
		if h.onSample != nil {
			h.onSample(*s)
		}
	}
	return nil
}

// gridDims returns the column/row counts for an n-tile square-ish grid.
func gridDims(n int) (cols, rows int) {
	if n <= 0 {
		return 0, 0
	}
	cols = int(math.Ceil(math.Sqrt(float64(n))))
	rows = int(math.Ceil(float64(n) / float64(cols)))
	return cols, rows
}
