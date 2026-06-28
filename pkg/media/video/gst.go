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

//go:build gst

// This file implements the video.Compositor interface on top of GStreamer
// using go-gst. It is only compiled with the "gst" build tag, which requires
// GStreamer (>= 1.20) and the base/good/bad/ugly/libav plugin sets to be
// installed on the build and runtime hosts.
package video

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"

	"github.com/livekit/protocol/logger"
)

func clockTimeToDuration(ct gst.ClockTime) time.Duration {
	if d := ct.AsDuration(); d != nil {
		return *d
	}
	return 0
}

var initOnce sync.Once

// Available reports whether GStreamer-backed video support is compiled in.
func Available() bool { return true }

// NewCompositor creates a GStreamer compositor pipeline that mixes participant
// tiles into a single H.264 grid stream.
func NewCompositor(log logger.Logger, cfg Config, onSample func(Sample)) (Compositor, error) {
	cfg = cfg.WithDefaults()
	initOnce.Do(func() { gst.Init(nil) })

	pipeline, err := gst.NewPipeline("sip-video-compositor")
	if err != nil {
		return nil, err
	}

	comp, err := gst.NewElement("compositor")
	if err != nil {
		return nil, fmt.Errorf("compositor element: %w", err)
	}
	// Fill the canvas with black before any tile is added.
	_ = comp.SetProperty("background", 1) // 1 = black

	convert, err := gst.NewElement("videoconvert")
	if err != nil {
		return nil, err
	}
	rawCaps, err := gst.NewElement("capsfilter")
	if err != nil {
		return nil, err
	}
	_ = rawCaps.SetProperty("caps", gst.NewCapsFromString(
		fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1", cfg.Width, cfg.Height, cfg.FPS),
	))

	enc, err := gst.NewElement("x264enc")
	if err != nil {
		return nil, fmt.Errorf("x264enc element: %w", err)
	}
	_ = enc.SetProperty("bitrate", uint(cfg.BitrateKbps))
	_ = enc.SetProperty("tune", 4)         // zerolatency
	_ = enc.SetProperty("speed-preset", 1) // ultrafast
	_ = enc.SetProperty("byte-stream", true)
	_ = enc.SetProperty("key-int-max", uint(cfg.FPS*int(cfg.KeyFrameInterval.Seconds())))
	// Cap encoder threads so a single call does not fan out across all cores.
	_ = enc.SetProperty("threads", uint(2))
	_ = enc.SetProperty("option-string", "rc-lookahead=0:sync-lookahead=0:sliced-threads=0")

	encCaps, err := gst.NewElement("capsfilter")
	if err != nil {
		return nil, err
	}
	// constrained-baseline is the most broadly interoperable H.264 profile for SIP endpoints.
	_ = encCaps.SetProperty("caps", gst.NewCapsFromString("video/x-h264,profile=constrained-baseline,stream-format=byte-stream,alignment=au"))

	sink, err := app.NewAppSink()
	if err != nil {
		return nil, err
	}
	sink.SetProperty("emit-signals", false)
	sink.SetProperty("sync", false)

	c := &gstCompositor{
		log:      log,
		cfg:      cfg,
		pipeline: pipeline,
		comp:     comp,
		enc:      enc,
		inputs:   make(map[string]*gstInput),
		stopCh:   make(chan struct{}),
	}

	sink.SetCallbacks(&app.SinkCallbacks{
		NewSampleFunc: func(s *app.Sink) gst.FlowReturn {
			sample := s.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}
			buf := sample.GetBuffer()
			if buf == nil {
				return gst.FlowOK
			}
			data := buf.Bytes()
			if len(data) == 0 {
				return gst.FlowOK
			}
			c.firstSample.Do(func() {
				c.log.Infow("§ first composited frame from GStreamer appsink",
					"data_len", len(data),
					"keyframe", !buf.HasFlags(gst.BufferFlagDeltaUnit),
				)
			})
			out := Sample{
				Data:     data,
				Duration: clockTimeToDuration(buf.Duration()),
				KeyFrame: !buf.HasFlags(gst.BufferFlagDeltaUnit),
			}
			if onSample != nil {
				onSample(out)
			}
			return gst.FlowOK
		},
	})

	if err := pipeline.AddMany(comp, convert, rawCaps, enc, encCaps, sink.Element); err != nil {
		return nil, err
	}
	if err := gst.ElementLinkMany(comp, convert, rawCaps, enc, encCaps, sink.Element); err != nil {
		return nil, err
	}
	if err := pipeline.SetState(gst.StatePlaying); err != nil {
		return nil, err
	}
	log.Infow("§ GStreamer compositor pipeline started",
		"width", cfg.Width, "height", cfg.Height, "fps", cfg.FPS, "bitrate_kbps", cfg.BitrateKbps,
	)
	go c.watchBus()
	return c, nil
}

type gstCompositor struct {
	log      logger.Logger
	cfg      Config
	pipeline *gst.Pipeline
	comp     *gst.Element
	enc      *gst.Element

	mu     sync.Mutex
	inputs map[string]*gstInput
	closed bool

	stopCh      chan struct{}
	firstSample sync.Once

	// ptsBase is the Go wall-clock time captured on the very first WriteRTP
	// call across all inputs.  Using pipeline-creation time as the PTS origin
	// caused the first ~15 s of encoded video to carry timestamps ≥15 s,
	// making players / SIP decoders show a checkerboard for the opening 14-15 s
	// of the recording (no frame existed for t=0..15 s in the H.264 stream).
	// Anchoring to the first pushed buffer means the stream starts at PTS≈0.
	ptsBase     time.Time
	ptsBaseOnce sync.Once
}

func depayDecodeFor(codec InputCodec) (depay, decoder string, err error) {
	switch codec {
	case CodecVP8:
		return "rtpvp8depay", "vp8dec", nil
	case CodecVP9:
		return "rtpvp9depay", "vp9dec", nil
	case CodecH264:
		return "rtph264depay", "avdec_h264", nil
	default:
		return "", "", fmt.Errorf("unsupported input codec %v", codec)
	}
}

func (c *gstCompositor) AddInput(id string, codec InputCodec, clockRate int, payloadType int) (Input, error) {
	depayName, decName, err := depayDecodeFor(codec)
	if err != nil {
		return nil, err
	}
	if clockRate <= 0 {
		clockRate = H264ClockRate
	}
	if payloadType <= 0 {
		payloadType = 96 // dynamic range default; overridden by actual negotiated PT
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("compositor closed")
	}
	if _, ok := c.inputs[id]; ok {
		return nil, fmt.Errorf("input %q already exists", id)
	}

	src, err := app.NewAppSrc()
	if err != nil {
		return nil, err
	}
	src.SetProperty("is-live", true)
	src.SetProperty("format", gst.FormatTime)
	encName := map[InputCodec]string{CodecVP8: "VP8", CodecVP9: "VP9", CodecH264: "H264"}[codec]
	src.SetCaps(gst.NewCapsFromString(fmt.Sprintf(
		"application/x-rtp,media=video,encoding-name=%s,clock-rate=%d,payload=%d", encName, clockRate, payloadType,
	)))

	jitter, _ := gst.NewElement("rtpjitterbuffer")
	// SLAVE mode (the default) waits for RTCP SR to synchronize the RTP
	// clock before releasing any buffered packets. Because we forward only
	// raw RTP – never RTCP SR – from the WebRTC subscriber track, the
	// jitter buffer would stall indefinitely in SLAVE mode, producing zero
	// decoded frames. NONE mode (0) simply reorders packets by sequence
	// number and passes them through without any clock synchronisation.
	_ = jitter.SetProperty("mode", 0)
	// Bound the reorder window so out-of-order packets are not held too long.
	_ = jitter.SetProperty("latency", uint(50))
	depay, err := gst.NewElement(depayName)
	if err != nil {
		return nil, err
	}
	dec, err := gst.NewElement(decName)
	if err != nil {
		return nil, err
	}
	// Disable VP8 post-processing / deblocking to reduce decode CPU. Quality
	// loss is negligible at SIP tile sizes (each tile is a fraction of 720p).
	if codec == CodecVP8 {
		_ = dec.SetProperty("post-processing", false)
		_ = dec.SetProperty("deblock", false)
	}
	conv, _ := gst.NewElement("videoconvert")
	scale, _ := gst.NewElement("videoscale")
	// Preserve aspect ratio by letterboxing / pillarboxing with black borders
	// instead of stretching the source image to fill the tile dimensions.
	_ = scale.SetProperty("add-borders", true)
	queue, _ := gst.NewElement("queue")
	// Allow a small buffer before the compositor pad. When the compositor is
	// momentarily slower than the input, this absorbs short bursts; upstream
	// leaky ensures the oldest (most stale) frame is dropped rather than
	// blocking the decoder goroutine.
	_ = queue.SetProperty("max-size-buffers", uint(4))
	_ = queue.SetProperty("max-size-time", uint64(0))
	_ = queue.SetProperty("max-size-bytes", uint(0))
	_ = queue.SetProperty("leaky", 1) // upstream: drop newest arrival, keep existing frames
	scaleCaps, _ := gst.NewElement("capsfilter")

	in := &gstInput{
		id:     id,
		comp:   c,
		src:    src,
		elems:  []*gst.Element{src.Element, jitter, depay, dec, conv, scale, scaleCaps, queue},
		scaler: scaleCaps,
	}

	if err := c.pipeline.AddMany(in.elems...); err != nil {
		return nil, err
	}
	if err := gst.ElementLinkMany(in.elems...); err != nil {
		return nil, err
	}

	// Request a compositor sink pad and link the input branch to it.
	pad := c.comp.GetRequestPad("sink_%u")
	if pad == nil {
		return nil, fmt.Errorf("could not request compositor pad")
	}
	in.compPad = pad
	queuePad := queue.GetStaticPad("src")
	if r := queuePad.Link(pad); r != gst.PadLinkOK {
		return nil, fmt.Errorf("could not link tile to compositor: %v", r)
	}

	for _, e := range in.elems {
		if ok := e.SyncStateWithParent(); !ok {
			c.log.Warnw("§ GStreamer element failed to sync state with parent pipeline", nil,
				"input_id", id, "element", e.GetName(),
			)
		}
	}

	c.inputs[id] = in
	c.relayoutLocked()
	c.log.Infow("§ video tile added to GStreamer compositor",
		"id", id,
		"codec", codec,
		"clock_rate", clockRate,
		"payload_type", payloadType,
		"depay", depayName,
		"decoder", decName,
		"total_inputs", len(c.inputs),
	)
	return in, nil
}

func (c *gstCompositor) RemoveInput(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.inputs[id]
	if !ok {
		return
	}
	delete(c.inputs, id)
	in.teardownLocked()
	c.relayoutLocked()
}

func (c *gstCompositor) ForceKeyFrame() {
	c.mu.Lock()
	enc := c.enc
	c.mu.Unlock()
	if enc == nil {
		return
	}
	// Send a downstream force-key-unit event to the encoder.
	s := gst.NewStructure("GstForceKeyUnit")
	_ = s.SetValue("all-headers", true)
	ev := gst.NewCustomEvent(gst.EventTypeCustomDownstream, s)
	if pad := enc.GetStaticPad("sink"); pad != nil {
		pad.SendEvent(ev)
	}
}

// relayoutLocked recomputes the grid coordinates of every tile so that all
// inputs are arranged into the smallest square-ish grid that fits them.
func (c *gstCompositor) relayoutLocked() {
	n := len(c.inputs)
	if n == 0 {
		return
	}
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := int(math.Ceil(float64(n) / float64(cols)))
	tileW := c.cfg.Width / cols
	tileH := c.cfg.Height / rows

	i := 0
	for _, in := range c.inputs {
		col := i % cols
		row := i / cols
		if in.scaler != nil {
			_ = in.scaler.SetProperty("caps", gst.NewCapsFromString(
				fmt.Sprintf("video/x-raw,width=%d,height=%d", tileW, tileH),
			))
		}
		if in.compPad != nil {
			_ = in.compPad.SetProperty("xpos", col*tileW)
			_ = in.compPad.SetProperty("ypos", row*tileH)
			_ = in.compPad.SetProperty("width", tileW)
			_ = in.compPad.SetProperty("height", tileH)
		}
		i++
	}
}

func (c *gstCompositor) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stopCh != nil {
		close(c.stopCh)
	}
	for id, in := range c.inputs {
		in.teardownLocked()
		delete(c.inputs, id)
	}
	return c.pipeline.SetState(gst.StateNull)
}

// watchBus polls the GStreamer pipeline bus for error and warning messages and
// logs them with the § diagnostic prefix. It runs as a background goroutine
// until Close() signals stopCh.
func (c *gstCompositor) watchBus() {
	bus := c.pipeline.GetBus()
	if bus == nil {
		c.log.Warnw("§ GStreamer pipeline bus unavailable; error monitoring disabled", nil)
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			// Drain all pending error/warning/EOS messages without blocking.
			for {
				msg := bus.PopFiltered(gst.MessageError | gst.MessageWarning | gst.MessageEOS)
				if msg == nil {
					break
				}
				switch msg.Type() {
				case gst.MessageError:
					gerr := msg.ParseError()
					c.log.Errorw("§ GStreamer pipeline error", gerr,
						"debug", gerr.DebugString(), "src", msg.Source(),
					)
				case gst.MessageWarning:
					gerr := msg.ParseWarning()
					c.log.Warnw("§ GStreamer pipeline warning", gerr,
						"debug", gerr.DebugString(), "src", msg.Source(),
					)
				case gst.MessageEOS:
					c.log.Warnw("§ GStreamer pipeline EOS received", nil, "src", msg.Source())
				}
			}
		}
	}
}

type gstInput struct {
	id      string
	comp    *gstCompositor
	src     *app.Source
	elems   []*gst.Element
	scaler  *gst.Element
	compPad *gst.Pad
	closed  bool

	// diagnostics — written only by the single RTP-pump goroutine
	pktCount uint64
	firstPkt sync.Once
}

func (in *gstInput) WriteRTP(pkt []byte) error {
	if in.closed {
		return nil
	}
	in.pktCount++

	buf := gst.NewBufferFromBytes(pkt)

	// Stamp the buffer with the pipeline running time so that the compositor
	// aggregator can schedule frames correctly.  GST_CLOCK_TIME_NONE buffers
	// (no PTS) cause the compositor to stall indefinitely because it cannot
	// determine which output frame a buffer belongs to.
	//
	// We prefer the GStreamer pipeline clock (exact running time), but fall
	// back to a Go monotonic elapsed time from pipeline start when the clock
	// is not yet available — this happens for a brief window right after
	// startup with is-live=true sources, and is exactly the window we were
	// previously pushing NONE-timestamped buffers.
	// Anchor the PTS base to the real-world moment the very first RTP buffer
	// is pushed (across all inputs).  Using the pipeline-creation time caused
	// PTS values of ~14-15 s on the first buffers, so the H.264 output stream
	// appeared to "start" 15 s into its own timeline — players showed a
	// checkerboard for the opening 14-15 s of any recording because no frame
	// existed for t=0..15 s.  With the base anchored here, the first frame has
	// PTS≈0 and the stream plays back immediately.
	in.comp.ptsBaseOnce.Do(func() { in.comp.ptsBase = time.Now() })

	var ptsNs uint64
	clockAvail := false
	const clockNone = gst.ClockTime(^uint64(0))
	if clock := in.comp.pipeline.GetClock(); clock != nil {
		base := in.comp.pipeline.GetBaseTime()
		now := clock.GetTime()
		if now != clockNone && base != clockNone && now >= base {
			clockAvail = true
			ptsNs = uint64(now - base)
		}
	}
	if !clockAvail {
		// Pipeline clock not yet available; derive PTS from Go monotonic time
		// anchored to the first WriteRTP call so the stream starts at PTS≈0.
		elapsed := time.Since(in.comp.ptsBase)
		if elapsed < 0 {
			elapsed = 0
		}
		ptsNs = uint64(elapsed.Nanoseconds())
	}
	buf.SetPresentationTimestamp(gst.ClockTime(ptsNs))

	in.firstPkt.Do(func() {
		in.comp.log.Infow("§ first RTP packet pushed to GStreamer appsrc",
			"input_id", in.id,
			"pkt_len", len(pkt),
			"clock_available", clockAvail,
			"pts_ns", ptsNs,
		)
	})
	if in.pktCount%200 == 0 {
		in.comp.log.Debugw("§ RTP packets pushed to GStreamer appsrc (periodic)",
			"input_id", in.id, "count", in.pktCount,
		)
	}

	if r := in.src.PushBuffer(buf); r != gst.FlowOK {
		if in.pktCount <= 5 || in.pktCount%200 == 0 {
			in.comp.log.Warnw("§ GStreamer appsrc PushBuffer returned non-OK flow",
				fmt.Errorf("flow: %v", r),
				"input_id", in.id, "flow", r, "pkt_count", in.pktCount,
			)
		}
		return fmt.Errorf("push buffer: %v", r)
	}
	return nil
}

func (in *gstInput) Close() error {
	in.comp.RemoveInput(in.id)
	return nil
}

// teardownLocked must be called with the compositor mutex held.
func (in *gstInput) teardownLocked() {
	if in.closed {
		return
	}
	in.closed = true
	in.src.EndStream()
	if in.compPad != nil {
		in.comp.comp.ReleaseRequestPad(in.compPad)
	}
	for _, e := range in.elems {
		_ = e.SetState(gst.StateNull)
		in.comp.pipeline.Remove(e)
	}
}
