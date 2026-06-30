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

// h264ProfileLevelIDForResolution returns the H.264 profile-level-id string
// (3 bytes in lowercase hex) that matches the given encoder output resolution.
//
// We use Constrained Baseline Profile (0x42, 0xe0) because it is the most
// broadly supported profile across SIP endpoints, Cisco/Tandberg, and WebRTC.
// The level byte is chosen to match the minimum H.264 level required for the
// given frame size at up to 30 fps so that the remote decoder pre-allocates
// enough memory to display the full frame:
//
//   - Level 3.1  (0x1f / 31)  – up to 1280×720  @ 30 fps
//   - Level 4.0  (0x28 / 40)  – up to 1920×1080 @ 30 fps
//   - Level 5.0  (0x32 / 50)  – larger frames
func h264ProfileLevelIDForResolution(width, height int) string {
	// Count 16×16 macro-blocks per frame.
	mbs := ((width + 15) / 16) * ((height + 15) / 16)
	switch {
	case mbs <= 3600: // ≤ 1280×720 → Level 3.1
		return "42e01f"
	case mbs <= 8192: // ≤ 1920×1080 → Level 4.0
		return "42e028"
	default: // larger → Level 5.0
		return "42e032"
	}
}

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
// Capacity constraints parsed from the remote offer (H264FmtpExtra) are echoed
// back so that Cisco/Tandberg endpoints can determine bitrate and resolution limits.
func addVideoAnswer(s *sdp.SessionDescription, port int, v *videoMediaConf) {
	pt := strconv.Itoa(int(v.Type))
	// Use the LOCAL profile-level-id (derived from our encoder output resolution)
	// in preference to the remote's offered value.  Echoing the remote value is
	// wrong: Cisco DX80 advertises Level 2.0 (428014) even for 1080p, so if we
	// echo it back, Cisco's decoder only allocates a 352×288 buffer and renders
	// just the top-left corner of our full-HD frame.
	profile := v.LocalProfileLevelID
	if profile == "" {
		profile = v.ProfileLevelID
	}
	if profile == "" {
		profile = defaultH264ProfileLevelID
	}
	pktMode := v.PacketizationMode
	if pktMode == 0 {
		pktMode = defaultH264PacketizationMode
	}
	fmtp := fmt.Sprintf("%s profile-level-id=%s;packetization-mode=%d", pt, profile, pktMode)
	if v.H264FmtpExtra != "" {
		fmtp += ";" + v.H264FmtpExtra
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
			{Key: "fmtp", Value: fmtp},
			{Key: "rtcp-fb", Value: pt + " nack pli"},
			{Key: "sendrecv"},
		},
	}
	s.MediaDescriptions = append(s.MediaDescriptions, md)
}

// setVideoAnswerOnLocalSDP replaces any existing video m-lines in localSDP with
// a single negotiated H.264 video answer.
func setVideoAnswerOnLocalSDP(localSDP []byte, port int, v *videoMediaConf) ([]byte, error) {
	var s sdp.SessionDescription
	if err := s.Unmarshal(localSDP); err != nil {
		return nil, err
	}
	media := s.MediaDescriptions[:0]
	for _, md := range s.MediaDescriptions {
		if strings.EqualFold(md.MediaName.Media, "video") {
			continue
		}
		media = append(media, md)
	}
	s.MediaDescriptions = media
	addVideoAnswer(&s, port, v)
	return s.Marshal()
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
	pt, profile, pktMode, extra, ok := selectH264(md)
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
		H264FmtpExtra:     extra,
		Remote:            addr,
	}, true
}

// h264CapacityParams is the set of H.264 fmtp capacity parameters that are
// echoed back verbatim in the SDP answer so that Cisco/Tandberg endpoints can
// determine the bitrate and resolution limits they should apply.
var h264CapacityParams = map[string]struct{}{
	"max-br":    {},
	"max-mbps":  {},
	"max-fs":    {},
	"max-dpb":   {},
	"max-smbps": {},
	"max-cpb":   {},
	"max-fps":   {},
}

// selectH264 finds the best H.264 payload type and its fmtp parameters in md.
// It prefers a PT with explicit packetization-mode=1 (RFC 6184 non-interleaved)
// when multiple H.264 entries are offered, falling back to the first H.264 PT.
// It also collects H.264 capacity parameters (max-br, max-mbps, …) so they can
// be echoed in the SDP answer.
func selectH264(md *sdp.MediaDescription) (pt byte, profileLevelID string, packetizationMode int, fmtpExtra string, ok bool) {
	type candidate struct {
		pt                    int
		profileLevelID        string
		packetizationMode     int
		packetModeExplicit    bool   // true when packetization-mode appeared in fmtp
		extra                 string // semicolon-joined capacity params
	}

	// First pass: collect all H.264 PTs in offer order.
	var candidates []candidate
	ptIndex := make(map[string]int) // pt string → index into candidates
	for _, a := range md.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		fields := strings.SplitN(a.Value, " ", 2)
		if len(fields) != 2 {
			continue
		}
		name := fields[1]
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[:i]
		}
		if !strings.EqualFold(name, h264SDPName) {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		ptIndex[strconv.Itoa(v)] = len(candidates)
		candidates = append(candidates, candidate{
			pt:                v,
			packetizationMode: defaultH264PacketizationMode,
		})
	}
	if len(candidates) == 0 {
		return 0, "", 0, "", false
	}

	// Second pass: parse fmtp for each H.264 PT.
	for _, a := range md.Attributes {
		if a.Key != "fmtp" {
			continue
		}
		fields := strings.SplitN(a.Value, " ", 2)
		if len(fields) != 2 {
			continue
		}
		idx, found := ptIndex[strings.TrimSpace(fields[0])]
		if !found {
			continue
		}
		var extraParts []string
		for _, kv := range strings.Split(fields[1], ";") {
			kv = strings.TrimSpace(kv)
			k, v, hasSep := strings.Cut(kv, "=")
			if !hasSep {
				continue
			}
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.TrimSpace(v)
			switch k {
			case "profile-level-id":
				candidates[idx].profileLevelID = v
			case "packetization-mode":
				if m, err := strconv.Atoi(v); err == nil {
					candidates[idx].packetizationMode = m
					candidates[idx].packetModeExplicit = true
				}
			default:
				if _, isCapacity := h264CapacityParams[k]; isCapacity {
					extraParts = append(extraParts, k+"="+v)
				}
			}
		}
		candidates[idx].extra = strings.Join(extraParts, ";")
	}

	// Select the best candidate: prefer a PT that explicitly declares
	// packetization-mode=1 in its fmtp over one that only has the default.
	// This ensures Cisco endpoints that list both mode=0 (PT 97) and mode=1
	// (PT 126) get the mode=1 PT selected, while still falling back gracefully.
	selected := candidates[0]
	for _, c := range candidates[1:] {
		selectedIsExplicit1 := selected.packetModeExplicit && selected.packetizationMode == 1
		cIsExplicit1 := c.packetModeExplicit && c.packetizationMode == 1
		if cIsExplicit1 && !selectedIsExplicit1 {
			selected = c
		}
	}

	return byte(selected.pt), selected.profileLevelID, selected.packetizationMode, selected.extra, true
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
