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
	"errors"
	"io"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/media/video"
)

// keyFrameRequestInterval is how often we re-request key frames from room
// participants, ensuring newly added/recovered tiles refresh quickly.
const keyFrameRequestInterval = 5 * time.Second

// VideoEnabled reports whether video compositing is active for this room.
func (r *Room) VideoEnabled() bool {
	if r == nil {
		return false
	}
	r.videoMu.Lock()
	defer r.videoMu.Unlock()
	return r.comp != nil
}

// EnableVideo creates the GStreamer grid compositor for this room. Composited
// H.264 access units are forwarded to the writer set via SwapVideoOutput.
func (r *Room) EnableVideo(cfg video.Config, stats *VideoStats) error {
	if !video.Available() {
		return video.ErrNotSupported
	}
	r.videoMu.Lock()
	defer r.videoMu.Unlock()
	if r.comp != nil {
		return nil
	}
	r.videoCfg = cfg
	r.videoStats = stats
	r.videoOut = &gatedVideoWriter{}
	r.videoOut.Enable()

	out := r.videoOut
	comp, err := video.NewCompositor(r.log, cfg, func(s video.Sample) {
		_ = out.WriteVideoSample(s)
	})
	if err != nil {
		return err
	}
	r.comp = comp
	r.videoInputs = make(map[string]video.Input)
	r.log.Infow("video compositor enabled", "width", cfg.Width, "height", cfg.Height, "fps", cfg.FPS, "bitrate_kbps", cfg.BitrateKbps)
	return nil
}

// ForceKeyFrame requests the GStreamer encoder to emit a fresh IDR frame as
// soon as possible. Call this after enabling video output so the SIP client
// can start decoding from a clean state.
func (r *Room) ForceKeyFrame() {
	r.videoMu.Lock()
	comp := r.comp
	r.videoMu.Unlock()
	if comp != nil {
		comp.ForceKeyFrame()
	}
}

// SwapVideoOutput sets the destination for composited video (the SIP video
// writer) and returns the previous one.
func (r *Room) SwapVideoOutput(w videoSampleWriter) videoSampleWriter {
	if r == nil || r.videoOut == nil {
		return nil
	}
	return r.videoOut.Swap(w)
}

// handleVideoTrack registers a subscribed participant video track as a
// compositor tile and pumps its RTP into the pipeline.
func (r *Room) handleVideoTrack(log logger.Logger, track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	r.videoMu.Lock()
	comp := r.comp
	r.videoMu.Unlock()
	if comp == nil {
		return
	}

	codec := video.CodecFromMimeType(track.Codec().MimeType)
	if codec == video.CodecUnknown {
		log.Warnw("unsupported video codec for compositing", nil, "mime", track.Codec().MimeType)
		return
	}
	id := pub.SID()
	// Use the participant's display name for the on-screen label; fall back to
	// identity (unique string) if the name hasn't been set.
	label := rp.Name()
	if label == "" {
		label = rp.Identity()
	}
	in, err := comp.AddInput(id, codec, int(track.Codec().ClockRate), int(track.Codec().PayloadType), label)
	if err != nil {
		log.Errorw("cannot add compositor input", err)
		return
	}
	r.videoMu.Lock()
	r.videoInputs[id] = in
	r.videoMu.Unlock()
	if r.videoStats != nil {
		r.videoStats.CompositorInputs.Add(1)
	}
	log.Infow("added video tile", "codec", codec.String())
	defer r.removeVideoInput(id)

	// Request an initial key frame so the decoder can start immediately, and a
	// fresh key frame for the SIP encoder.
	log.Infow("§ sending initial PLI to request keyframe from participant",
		"ssrc", track.SSRC(), "codec", track.Codec().MimeType,
	)
	rp.WritePLI(track.SSRC())
	comp.ForceKeyFrame()

	// Periodically re-request key frames from the publisher.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(keyFrameRequestInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-r.stopped.Watch():
				return
			case <-t.C:
				log.Debugw("§ periodic PLI sent to participant", "ssrc", track.SSRC())
				rp.WritePLI(track.SSRC())
			}
		}
	}()

	const maxConsecutiveErrors = 10
	consecutiveErrors := 0
	var rtpCount uint64
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Debugw("video track read ended", "error", err)
			}
			return
		}
		raw, err := pkt.Marshal()
		if err != nil {
			continue
		}
		rtpCount++
		if rtpCount == 1 || rtpCount == 5 || rtpCount == 20 || rtpCount%500 == 0 {
			log.Infow("§ VP8 RTP packet read from LiveKit track",
				"rtp_count", rtpCount, "seq", pkt.SequenceNumber, "ssrc", pkt.SSRC,
				"payload_len", len(pkt.Payload), "marker", pkt.Marker,
			)
		}
		if err := in.WriteRTP(raw); err != nil {
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveErrors {
				log.Warnw("compositor input write failed repeatedly, closing tile", err, "consecutive_errors", consecutiveErrors)
				return
			}
			log.Debugw("compositor input write failed (transient)", "error", err, "consecutive_errors", consecutiveErrors)
			continue
		}
		consecutiveErrors = 0
	}
}

func (r *Room) removeVideoInput(id string) {
	r.videoMu.Lock()
	in := r.videoInputs[id]
	if in != nil {
		delete(r.videoInputs, id)
	}
	r.videoMu.Unlock()
	if in == nil {
		return
	}
	_ = in.Close()
	if r.videoStats != nil {
		r.videoStats.CompositorInputs.Add(-1)
	}
}

func (r *Room) closeVideo() {
	r.videoMu.Lock()
	comp := r.comp
	r.comp = nil
	inputs := r.videoInputs
	r.videoInputs = nil
	r.videoMu.Unlock()

	for _, in := range inputs {
		_ = in.Close()
	}
	if comp != nil {
		_ = comp.Close()
	}
}

// NewParticipantVideoTrack publishes an H.264 video track for the SIP caller's
// inbound video and returns a writer for encoded access units.
func (r *Room) NewParticipantVideoTrack() (VideoTrackWriter, error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		return nil, err
	}
	p := r.room.LocalParticipant
	width, height := r.videoCfg.Width, r.videoCfg.Height
	pub, err := p.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:        p.Identity() + "-video",
		Source:      livekit.TrackSource_CAMERA,
		VideoWidth:  width,
		VideoHeight: height,
	})
	if err != nil {
		return nil, err
	}
	return &videoTrackWriter{track: track, pub: pub, p: p}, nil
}

type videoTrackWriter struct {
	track *webrtc.TrackLocalStaticSample
	pub   *lksdk.LocalTrackPublication
	p     *lksdk.LocalParticipant
}

func (w *videoTrackWriter) WriteSample(s media.Sample) error {
	return w.track.WriteSample(s)
}

func (w *videoTrackWriter) Close() error {
	if w.pub != nil && w.p != nil {
		return w.p.UnpublishTrack(w.pub.SID())
	}
	return nil
}
