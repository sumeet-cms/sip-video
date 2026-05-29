# Copyright 2023 LiveKit, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG GOVERSION=1.26

# ENABLE_VIDEO=1 builds with H.264 video-over-SIP support (build tag "gst").
# This pulls in GStreamer at build and runtime. Defaults to 0 (audio-only).
ARG ENABLE_VIDEO=0

FROM golang:$GOVERSION AS builder

ARG TARGETPLATFORM
ARG VERSION=0.0.0-dev
ARG ENABLE_VIDEO

WORKDIR /workspace

# install libopus (and GStreamer dev libs when building with video support)
RUN apt-get update && apt-get install -y pkg-config libopus-dev libopusfile-dev libsoxr-dev && \
    if [ "$ENABLE_VIDEO" = "1" ]; then \
        apt-get install -y \
            libgstreamer1.0-dev \
            libgstreamer-plugins-base1.0-dev; \
    fi

# download go modules
COPY go.mod .
COPY go.sum .
RUN go mod download

# copy source
COPY res/ res/
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY version/ version/

# build (add the "gst" build tag when video support is enabled)
RUN if [ "$TARGETPLATFORM" = "linux/arm64" ]; then GOARCH=arm64; else GOARCH=amd64; fi && \
    if [ "$ENABLE_VIDEO" = "1" ]; then BUILD_TAGS="-tags gst"; else BUILD_TAGS=""; fi && \
    CGO_ENABLED=1 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -a ${BUILD_TAGS} \
    -ldflags "-X github.com/livekit/sip/version.Version=${VERSION}" \
    -o livekit-sip ./cmd/livekit-sip

FROM debian:trixie-slim

ARG ENABLE_VIDEO

# install wget for health check (and GStreamer runtime + plugins when video is enabled)
RUN apt-get update && \
    apt-get install -y libopus0 libopusfile0 libsoxr0 ca-certificates && \
    if [ "$ENABLE_VIDEO" = "1" ]; then \
        apt-get install -y \
            libgstreamer1.0-0 \
            gstreamer1.0-plugins-base \
            gstreamer1.0-plugins-good \
            gstreamer1.0-plugins-bad \
            gstreamer1.0-plugins-ugly \
            gstreamer1.0-libav; \
    fi && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# copy binary
COPY --from=builder /workspace/livekit-sip /bin/

WORKDIR /sip

# run
ENTRYPOINT ["livekit-sip", "--config=/sip/config.yaml"]
