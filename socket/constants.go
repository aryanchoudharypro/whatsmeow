// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package socket implements a subset of the Noise protocol framework on top of websockets as used by WhatsApp.
//
// There shouldn't be any need to manually interact with this package.
// The Client struct in the top-level whatsmeow package handles everything.
package socket

import (
	"errors"

	"go.mau.fi/whatsmeow/binary/token"
)

const (
	// Origin is the Origin header for all WhatsApp websocket connections
	Origin = "https://web.whatsapp.com"
	// URL is the websocket URL for the new multidevice protocol
	URL = "wss://web.whatsapp.com/ws/chat"
)

const (
	NoiseStartPattern = "Noise_XX_25519_AESGCM_SHA256\x00\x00\x00\x00"
	// NoiseIKStartPattern is used to resume against a cached server static key.
	NoiseIKStartPattern = "Noise_IK_25519_AESGCM_SHA256\x00\x00\x00\x00"
	// NoiseXXFallbackStartPattern is used when the server declines an IK resume.
	NoiseXXFallbackStartPattern = "Noise_XXfallback_25519_AESGCM_SHA256"

	WAMagicValue = 6
)

var WAConnHeader = []byte{'W', 'A', WAMagicValue, token.DictVersion}

// MaxEdgeRoutingLength is the most routing info the pre-intro's 3-byte length
// can describe.
const MaxEdgeRoutingLength = 1<<24 - 1

// EdgeRoutingPreIntro builds the header sent ahead of the connection header
// when the server gave us routing info on an earlier connection:
// "ED", 0, 1, a 3-byte big-endian length, then the routing info.
// It returns nil when there is nothing usable to send.
func EdgeRoutingPreIntro(routingInfo []byte) []byte {
	length := len(routingInfo)
	if length == 0 || length > MaxEdgeRoutingLength {
		return nil
	}
	preIntro := make([]byte, 0, 7+length)
	preIntro = append(preIntro, 'E', 'D', 0, 1, byte(length>>16), byte(length>>8), byte(length))
	return append(preIntro, routingInfo...)
}

const (
	FrameMaxSize    = 1 << 24
	FrameLengthSize = 3
)

var (
	ErrFrameTooLarge     = errors.New("frame too large")
	ErrSocketClosed      = errors.New("frame socket is closed")
	ErrSocketAlreadyOpen = errors.New("frame socket is already open")
	ErrDialFailed        = errors.New("failed to dial whatsapp web websocket")
)

type ErrWithStatusCode struct {
	error
	StatusCode int
}
