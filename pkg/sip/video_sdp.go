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

// SDP helpers for negotiating an H.264 m=video line. The media-sdk SDP package
// only understands a single audio m-line, so we post-process the
// SessionDescription it produces (and parse the remote SDP) here.

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

const (
	// defaultH264PayloadType is the dynamic RTP payload type we offer for H.264.
	defaultH264PayloadType = 96
	// defaultH264ProfileLevelID is constrained-baseline level 3.1, the most
	// broadly interoperable profile for SIP video endpoints.
	defaultH264ProfileLevelID = "42e01f"
	// defaultH264PacketizationMode follows RFC 6184 mode 1 (non-interleaved).
	defaultH264PacketizationMode = 1
)

// addVideoOffer appends an H.264 m=video offer to s, advertising the given
// local RTP port. The session-level connection address is reused.
func addVideoOffer(s *sdp.SessionDescription, port int) {
	pt := strconv.Itoa(defaultH264PayloadType)
	md := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "video",
			Port:    sdp.RangedPort{Value: port},
			Protos:  []string{"RTP", "AVP"},
			Formats: []string{pt},
		},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: fmt.Sprintf("%s %s/%d", pt, h264SDPName, VideoClockRate)},
			{Key: "fmtp", Value: fmt.Sprintf("%s profile-level-id=%s;packetization-mode=%d;level-asymmetry-allowed=1",
				pt, defaultH264ProfileLevelID, defaultH264PacketizationMode)},
			{Key: "rtcp-fb", Value: pt + " nack pli"},
			{Key: "sendrecv"},
		},
	}
	s.MediaDescriptions = append(s.MediaDescriptions, md)
}

// addVideoAnswer appends an H.264 m=video answer to s using the negotiated
// payload type and profile, advertising the given local RTP port.
func addVideoAnswer(s *sdp.SessionDescription, port int, v *videoMediaConf) {
	pt := strconv.Itoa(int(v.Type))
	profile := v.ProfileLevelID
	if profile == "" {
		profile = defaultH264ProfileLevelID
	}
	pktMode := v.PacketizationMode
	if pktMode == 0 {
		pktMode = defaultH264PacketizationMode
	}
	md := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "video",
			Port:    sdp.RangedPort{Value: port},
			Protos:  []string{"RTP", "AVP"},
			Formats: []string{pt},
		},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: fmt.Sprintf("%s %s/%d", pt, h264SDPName, VideoClockRate)},
			{Key: "fmtp", Value: fmt.Sprintf("%s profile-level-id=%s;packetization-mode=%d", pt, profile, pktMode)},
			{Key: "rtcp-fb", Value: pt + " nack pli"},
			{Key: "sendrecv"},
		},
	}
	s.MediaDescriptions = append(s.MediaDescriptions, md)
}

// rejectVideo appends a rejected (port 0) m=video answer to s, echoing the
// offered payload types. Per RFC 3264 a declined media must still appear.
func rejectVideo(s *sdp.SessionDescription, offered *sdp.MediaDescription) {
	md := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "video",
			Port:    sdp.RangedPort{Value: 0},
			Protos:  offered.MediaName.Protos,
			Formats: offered.MediaName.Formats,
		},
	}
	s.MediaDescriptions = append(s.MediaDescriptions, md)
}

// findVideoMedia returns the first m=video description in s, or nil.
func findVideoMedia(s *sdp.SessionDescription) *sdp.MediaDescription {
	for _, md := range s.MediaDescriptions {
		if strings.EqualFold(md.MediaName.Media, "video") {
			return md
		}
	}
	return nil
}

// parseVideoOffer parses the remote SDP for an H.264 video m-line. It returns
// (nil, false) when there is no usable, active (non-zero port) H.264 video.
func parseVideoOffer(data []byte) (*videoMediaConf, bool) {
	var s sdp.SessionDescription
	if err := s.Unmarshal(data); err != nil {
		return nil, false
	}
	return parseVideoSession(&s)
}

func parseVideoSession(s *sdp.SessionDescription) (*videoMediaConf, bool) {
	md := findVideoMedia(s)
	if md == nil || md.MediaName.Port.Value == 0 {
		return nil, false
	}
	pt, profile, pktMode, ok := selectH264(md)
	if !ok {
		return nil, false
	}
	addr, err := videoRemoteAddr(s, md)
	if err != nil {
		return nil, false
	}
	return &videoMediaConf{
		Type:              pt,
		ClockRate:         VideoClockRate,
		ProfileLevelID:    profile,
		PacketizationMode: pktMode,
		Remote:            addr,
	}, true
}

// selectH264 finds the H.264 payload type and its fmtp parameters in md.
func selectH264(md *sdp.MediaDescription) (pt byte, profileLevelID string, packetizationMode int, ok bool) {
	packetizationMode = defaultH264PacketizationMode
	var h264PT = -1
	for _, a := range md.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		// "<pt> H264/90000"
		fields := strings.SplitN(a.Value, " ", 2)
		if len(fields) != 2 {
			continue
		}
		name := fields[1]
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[:i]
		}
		if strings.EqualFold(name, h264SDPName) {
			if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
				h264PT = v
				break
			}
		}
	}
	if h264PT < 0 {
		return 0, "", 0, false
	}
	ptStr := strconv.Itoa(h264PT)
	for _, a := range md.Attributes {
		if a.Key != "fmtp" {
			continue
		}
		fields := strings.SplitN(a.Value, " ", 2)
		if len(fields) != 2 || strings.TrimSpace(fields[0]) != ptStr {
			continue
		}
		for _, kv := range strings.Split(fields[1], ";") {
			kv = strings.TrimSpace(kv)
			k, v, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "profile-level-id":
				profileLevelID = strings.TrimSpace(v)
			case "packetization-mode":
				if m, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					packetizationMode = m
				}
			}
		}
	}
	return byte(h264PT), profileLevelID, packetizationMode, true
}

// videoRemoteAddr resolves the remote RTP address for the video m-line,
// preferring a media-level connection line over the session-level one.
func videoRemoteAddr(s *sdp.SessionDescription, md *sdp.MediaDescription) (netip.AddrPort, error) {
	ci := md.ConnectionInformation
	if ci == nil {
		ci = s.ConnectionInformation
	}
	if ci == nil || ci.Address == nil {
		return netip.AddrPort{}, fmt.Errorf("no connection information for video")
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ci.Address.Address))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid video address %q: %w", ci.Address.Address, err)
	}
	return netip.AddrPortFrom(addr, uint16(md.MediaName.Port.Value)), nil
}
