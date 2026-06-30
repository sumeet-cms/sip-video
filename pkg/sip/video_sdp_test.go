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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const videoOfferSDP = `v=0
o=- 123456 123456 IN IP4 203.0.113.5
s=-
c=IN IP4 203.0.113.5
t=0 0
m=audio 40000 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
m=video 40002 RTP/AVP 97
a=rtpmap:97 H264/90000
a=fmtp:97 profile-level-id=42e01f;packetization-mode=1
a=sendrecv
`

const ciscoComplexOfferSDP = `v=0
o=tandberg 0 2 IN IP4 10.6.1.113
s=-
c=IN IP4 164.100.103.40
b=AS:4000
t=0 0
m=audio 45184 RTP/AVP 108 107 96 109 110 9 99 111 100 104 103 0 8 15 102 18 101
a=rtpmap:108 opus/48000/2
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-15
a=sendrecv
m=video 45316 RTP/AVP 97 116 96 34 31 121
a=rtpmap:97 H264/90000
a=fmtp:97 profile-level-id=428014;max-mbps=489600;max-fs=8160;max-cpb=4000;max-dpb=4752;max-br=3120;max-fps=6000
a=rtpmap:116 H264/90000
a=fmtp:116 profile-level-id=428014;max-mbps=489600;max-fs=8160;max-cpb=4000;max-dpb=4752;max-br=3120;max-fps=6000;packetization-mode=1
a=rtpmap:96 H263-1998/90000
a=rtcp-fb:* nack pli
a=sendrecv
a=content:main
a=label:11
a=rtcp:45317 IN IP4 164.100.103.40
m=video 36716 RTP/AVP 97 116 96 34 121
a=rtpmap:97 H264/90000
a=fmtp:97 profile-level-id=428014;max-mbps=270000;max-fs=32400;max-cpb=4000;max-dpb=4752;max-br=3333;max-fps=3000
a=rtpmap:116 H264/90000
a=fmtp:116 profile-level-id=428014;max-mbps=270000;max-fs=32400;max-cpb=4000;max-dpb=4752;max-br=3333;max-fps=3000;packetization-mode=1
a=sendrecv
a=content:slides
a=label:12
a=rtcp:36717 IN IP4 164.100.103.40
m=application 36188 UDP/BFCP *
a=confid:1
a=userid:15377
a=floorid:2 mstrm:12
a=floorctrl:s-only
m=application 45560 UDP/UDT/IX *
a=fingerprint:sha-256 29:CA:C2:64:BC:59:71:D9:41:0C:1F:51:12:18:EF:FC:98:84:37:06:4E:FD:0F:70:02:8F:04:D2:C1:F5:EA:56
a=setup:actpass
a=ixmap:0 ping
a=ixmap:2 xccp
`

const audioOnlyAnswerSDP = `v=0
o=- 3245388592986587240 3245388592986587244 IN IP4 135.235.161.185
s=LiveKit
c=IN IP4 135.235.161.185
t=0 0
m=audio 18637 RTP/AVP 9 101
a=rtpmap:9 G722/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
`

func TestParseVideoOffer(t *testing.T) {
	v, ok := parseVideoOffer([]byte(videoOfferSDP))
	require.True(t, ok)
	assert.Equal(t, byte(97), v.Type)
	assert.Equal(t, VideoClockRate, v.ClockRate)
	assert.Equal(t, "42e01f", v.ProfileLevelID)
	assert.Equal(t, 1, v.PacketizationMode)
	assert.Equal(t, "203.0.113.5:40002", v.Remote.String())
}

func TestParseVideoOfferNoVideo(t *testing.T) {
	const audioOnly = `v=0
o=- 1 1 IN IP4 203.0.113.5
s=-
c=IN IP4 203.0.113.5
t=0 0
m=audio 40000 RTP/AVP 0
a=rtpmap:0 PCMU/8000
`
	_, ok := parseVideoOffer([]byte(audioOnly))
	assert.False(t, ok)
}

func TestParseVideoOfferCiscoComplex(t *testing.T) {
	v, ok := parseVideoOffer([]byte(ciscoComplexOfferSDP))
	require.True(t, ok)
	// PT 116 has explicit packetization-mode=1 and is preferred over PT 97.
	assert.Equal(t, byte(116), v.Type)
	assert.Equal(t, VideoClockRate, v.ClockRate)
	assert.Equal(t, "428014", v.ProfileLevelID)
	assert.Equal(t, 1, v.PacketizationMode)
	assert.Equal(t, "164.100.103.40:45316", v.Remote.String())
	// Capacity params from PT 116's fmtp must be preserved for the answer.
	assert.Contains(t, v.H264FmtpExtra, "max-mbps=489600")
	assert.Contains(t, v.H264FmtpExtra, "max-fs=8160")
	assert.Contains(t, v.H264FmtpExtra, "max-br=3120")
	assert.Contains(t, v.H264FmtpExtra, "max-dpb=4752")
}

func TestSetVideoAnswerOnLocalSDP(t *testing.T) {
	v, ok := parseVideoOffer([]byte(ciscoComplexOfferSDP))
	require.True(t, ok)

	updated, err := setVideoAnswerOnLocalSDP([]byte(audioOnlyAnswerSDP), 13645, v)
	require.NoError(t, err)

	parsed, ok := parseVideoOffer(updated)
	require.True(t, ok)
	assert.Equal(t, byte(116), parsed.Type)
	assert.Equal(t, "428014", parsed.ProfileLevelID)
	assert.Equal(t, 1, parsed.PacketizationMode)
	assert.Equal(t, "135.235.161.185:13645", parsed.Remote.String())

	// Capacity params must be echoed verbatim in the answer fmtp.
	assert.Contains(t, string(updated), "max-mbps=489600")
	assert.Contains(t, string(updated), "max-fs=8160")
	assert.Contains(t, string(updated), "max-br=3120")
}

// ciscoDX80OfferSDP reflects a real Cisco DX-80 INVITE where PT 97 is
// packetization-mode=0 and PT 126 is packetization-mode=1.  The server must
// prefer PT 126 and echo its capacity params back in the answer.
const ciscoDX80OfferSDP = `v=0
o=tandberg 15 1 IN IP4 10.1.2.5
s=-
c=IN IP4 10.1.2.5
t=0 0
m=audio 2336 RTP/AVP 9 0 8 101
a=rtpmap:9 G722/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-15
a=sendrecv
m=video 2372 RTP/AVP 97 126
a=rtpmap:97 H264/90000
a=fmtp:97 packetization-mode=0;profile-level-id=428014;max-br=2500;max-mbps=245000;max-fs=8160;max-dpb=16320;max-smbps=245000
a=rtpmap:126 H264/90000
a=fmtp:126 packetization-mode=1;profile-level-id=428014;max-br=2500;max-mbps=122400;max-fs=8160;max-dpb=16320;max-smbps=122400
a=rtcp-fb:* nack pli
a=sendrecv
`

func TestParseVideoOfferCiscoDX80(t *testing.T) {
	v, ok := parseVideoOffer([]byte(ciscoDX80OfferSDP))
	require.True(t, ok)
	// PT 97 is mode=0; PT 126 is mode=1 — we must prefer PT 126.
	assert.Equal(t, byte(126), v.Type)
	assert.Equal(t, VideoClockRate, v.ClockRate)
	assert.Equal(t, "428014", v.ProfileLevelID)
	assert.Equal(t, 1, v.PacketizationMode)
	assert.Equal(t, "10.1.2.5:2372", v.Remote.String())
	// Capacity params from PT 126's fmtp.
	assert.Contains(t, v.H264FmtpExtra, "max-br=2500")
	assert.Contains(t, v.H264FmtpExtra, "max-mbps=122400")
	assert.Contains(t, v.H264FmtpExtra, "max-fs=8160")
	assert.Contains(t, v.H264FmtpExtra, "max-dpb=16320")
	assert.Contains(t, v.H264FmtpExtra, "max-smbps=122400")
}

func TestSetVideoAnswerOnLocalSDPCiscoDX80(t *testing.T) {
	v, ok := parseVideoOffer([]byte(ciscoDX80OfferSDP))
	require.True(t, ok)

	updated, err := setVideoAnswerOnLocalSDP([]byte(audioOnlyAnswerSDP), 19784, v)
	require.NoError(t, err)

	// The answer must echo all capacity params so Cisco doesn't show a blank tile.
	updatedStr := string(updated)
	assert.Contains(t, updatedStr, "profile-level-id=428014")
	assert.Contains(t, updatedStr, "packetization-mode=1")
	assert.Contains(t, updatedStr, "max-br=2500")
	assert.Contains(t, updatedStr, "max-mbps=122400")
	assert.Contains(t, updatedStr, "max-fs=8160")
	assert.Contains(t, updatedStr, "max-dpb=16320")
	assert.Contains(t, updatedStr, "max-smbps=122400")
}

// TestH264ProfileLevelIDForResolution verifies that common resolutions map to
// the correct H.264 level strings.
func TestH264ProfileLevelIDForResolution(t *testing.T) {
	cases := []struct {
		w, h     int
		expected string
	}{
		{640, 480, "42e01f"},   // VGA  → Level 3.1
		{1280, 720, "42e01f"},  // 720p → Level 3.1 (exactly 3600 MBs)
		{1920, 1080, "42e028"}, // 1080p → Level 4.0
		{1920, 1088, "42e028"}, // codec-aligned 1080p → Level 4.0
		{3840, 2160, "42e032"}, // 4K → Level 5.0
	}
	for _, c := range cases {
		got := h264ProfileLevelIDForResolution(c.w, c.h)
		assert.Equal(t, c.expected, got, "resolution %dx%d", c.w, c.h)
	}
}

// TestSetVideoAnswerLocalProfileLevelOverride verifies that when
// LocalProfileLevelID is set (as it is in production by setupVideo / setupOutboundVideo),
// the SDP answer carries the server's actual encoder level instead of echoing
// the (possibly lower) level from Cisco's offer.
func TestSetVideoAnswerLocalProfileLevelOverride(t *testing.T) {
	v, ok := parseVideoOffer([]byte(ciscoDX80OfferSDP))
	require.True(t, ok)

	// Simulate what setupVideo does: compute the level for our 1080p encoder output.
	v.LocalProfileLevelID = h264ProfileLevelIDForResolution(1920, 1080) // "42e028"

	updated, err := setVideoAnswerOnLocalSDP([]byte(audioOnlyAnswerSDP), 19784, v)
	require.NoError(t, err)

	updatedStr := string(updated)
	// Our level (4.0 = 42e028) must appear; Cisco's echoed level (428014) must NOT.
	assert.Contains(t, updatedStr, "profile-level-id=42e028")
	assert.NotContains(t, updatedStr, "profile-level-id=428014")
	// Capacity params and packetization-mode must still be present.
	assert.Contains(t, updatedStr, "packetization-mode=1")
	assert.Contains(t, updatedStr, "max-mbps=122400")
	assert.Contains(t, updatedStr, "max-fs=8160")
}

func TestParseVideoOfferRejected(t *testing.T) {
	// port 0 means the video stream is declined
	const rejected = `v=0
o=- 1 1 IN IP4 203.0.113.5
s=-
c=IN IP4 203.0.113.5
t=0 0
m=audio 40000 RTP/AVP 0
a=rtpmap:0 PCMU/8000
m=video 0 RTP/AVP 97
a=rtpmap:97 H264/90000
`
	_, ok := parseVideoOffer([]byte(rejected))
	assert.False(t, ok)
}

func TestGridDims(t *testing.T) {
	cases := []struct {
		n, cols, rows int
	}{
		{0, 0, 0},
		{1, 1, 1},
		{2, 2, 1},
		{3, 2, 2},
		{4, 2, 2},
		{5, 3, 2},
		{9, 3, 3},
		{10, 4, 3},
	}
	for _, c := range cases {
		cols, rows := gridDims(c.n)
		assert.Equal(t, c.cols, cols, "n=%d cols", c.n)
		assert.Equal(t, c.rows, rows, "n=%d rows", c.n)
	}
}
