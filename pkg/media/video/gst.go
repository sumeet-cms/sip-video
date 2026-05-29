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

func (c *gstCompositor) AddInput(id string, codec InputCodec, clockRate int) (Input, error) {
	depayName, decName, err := depayDecodeFor(codec)
	if err != nil {
		return nil, err
	}
	if clockRate <= 0 {
		clockRate = H264ClockRate
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
		"application/x-rtp,media=video,encoding-name=%s,clock-rate=%d,payload=96", encName, clockRate,
	)))

	jitter, _ := gst.NewElement("rtpjitterbuffer")
	depay, err := gst.NewElement(depayName)
	if err != nil {
		return nil, err
	}
	dec, err := gst.NewElement(decName)
	if err != nil {
		return nil, err
	}
	conv, _ := gst.NewElement("videoconvert")
	scale, _ := gst.NewElement("videoscale")
	queue, _ := gst.NewElement("queue")
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
		e.SyncStateWithParent()
	}

	c.inputs[id] = in
	c.relayoutLocked()
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
	for id, in := range c.inputs {
		in.teardownLocked()
		delete(c.inputs, id)
	}
	return c.pipeline.SetState(gst.StateNull)
}

type gstInput struct {
	id      string
	comp    *gstCompositor
	src     *app.Source
	elems   []*gst.Element
	scaler  *gst.Element
	compPad *gst.Pad
	closed  bool
}

func (in *gstInput) WriteRTP(pkt []byte) error {
	if in.closed {
		return nil
	}
	buf := gst.NewBufferFromBytes(pkt)
	if r := in.src.PushBuffer(buf); r != gst.FlowOK {
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
