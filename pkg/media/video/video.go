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

// Package video provides video compositing and H.264 encoding for the SIP
// bridge. Every room participant's video is decoded, scaled and laid out into
// a single grid tile, then encoded to H.264 for relaying over a SIP video
// m-line.
//
// The actual media processing is implemented with GStreamer and is only
// compiled when the "gst" build tag is set (see gst.go). Without that tag a
// stub implementation is used (see stub.go) and Available() returns false, so
// the service still builds and runs audio-only.
package video

import (
	"errors"
	"strings"
	"time"
)

// ErrNotSupported is returned by the constructors when the binary was built
// without GStreamer support (i.e. without the "gst" build tag).
var ErrNotSupported = errors.New("video over SIP is not supported by this build (rebuild with -tags gst and GStreamer installed)")

// H264ClockRate is the RTP clock rate used for H.264 (90 kHz).
const H264ClockRate = 90000

// Defaults for the composited output stream.
const (
	DefaultWidth            = 1280
	DefaultHeight           = 720
	DefaultFPS              = 30
	DefaultBitrateKbps      = 2000
	DefaultKeyFrameInterval = 2 * time.Second
)

// InputKind distinguishes camera tracks from screen-share tracks so the
// compositor can apply different layout strategies.
type InputKind int

const (
	KindCamera      InputKind = iota // regular webcam / camera
	KindScreenShare                  // screen-share track
)

// InputCodec identifies the codec of an incoming participant video track.
type InputCodec int

const (
	CodecUnknown InputCodec = iota
	CodecVP8
	CodecVP9
	CodecH264
)

func (c InputCodec) String() string {
	switch c {
	case CodecVP8:
		return "VP8"
	case CodecVP9:
		return "VP9"
	case CodecH264:
		return "H264"
	default:
		return "unknown"
	}
}

// CodecFromMimeType maps a WebRTC mime type (e.g. "video/VP8") to an InputCodec.
func CodecFromMimeType(mime string) InputCodec {
	switch {
	case strings.EqualFold(mime, "video/VP8"):
		return CodecVP8
	case strings.EqualFold(mime, "video/VP9"):
		return CodecVP9
	case strings.EqualFold(mime, "video/H264"):
		return CodecH264
	default:
		return CodecUnknown
	}
}

// Config controls the composited H.264 output stream.
type Config struct {
	Width            int           // output width in pixels
	Height           int           // output height in pixels
	FPS              int           // output frame rate
	BitrateKbps      int           // target encoder bitrate, in kbps
	KeyFrameInterval time.Duration // maximum interval between key frames
}

// WithDefaults returns a copy of the config with zero fields replaced by sane
// defaults.
func (c Config) WithDefaults() Config {
	if c.Width <= 0 {
		c.Width = DefaultWidth
	}
	if c.Height <= 0 {
		c.Height = DefaultHeight
	}
	if c.FPS <= 0 {
		c.FPS = DefaultFPS
	}
	if c.BitrateKbps <= 0 {
		c.BitrateKbps = DefaultBitrateKbps
	}
	if c.KeyFrameInterval <= 0 {
		c.KeyFrameInterval = DefaultKeyFrameInterval
	}
	return c
}

// Sample is an encoded H.264 access unit emitted by the compositor. Data is in
// Annex-B byte-stream format (NAL units prefixed with start codes).
type Sample struct {
	Data     []byte
	Duration time.Duration
	KeyFrame bool
}

// Input is a single participant tile. Raw RTP packets (full RTP packet bytes,
// header + payload) of the participant's video track are pushed into it.
type Input interface {
	// WriteRTP pushes a single raw RTP packet into the tile pipeline.
	WriteRTP(pkt []byte) error
	// Close removes the tile from the composited output.
	Close() error
}

// Compositor decodes, scales and lays out multiple participant video inputs
// into a single H.264 grid stream. Encoded access units are delivered via the
// onSample callback passed to NewCompositor.
type Compositor interface {
	// AddInput registers a new participant tile. id must be unique for the
	// lifetime of the compositor. clockRate is the RTP clock rate of the input
	// track (typically 90000 for video). payloadType is the negotiated RTP
	// dynamic payload type for the codec on this track. label is the display
	// name rendered below the tile (pass "" to suppress). kind controls layout
	// placement: cameras go into the grid area, screen-shares into the main area.
	AddInput(id string, codec InputCodec, clockRate int, payloadType int, label string, kind InputKind) (Input, error)
	// RemoveInput removes a participant tile by id. It is safe to call with an
	// unknown id.
	RemoveInput(id string)
	// ForceKeyFrame requests the encoder to emit a key frame as soon as
	// possible. Called when a new SIP viewer joins or the layout changes.
	ForceKeyFrame()
	// Close tears down the pipeline and releases all resources.
	Close() error
}
