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
