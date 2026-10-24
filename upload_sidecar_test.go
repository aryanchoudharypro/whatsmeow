// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"testing"

	"go.mau.fi/whatsmeow/util/cbcutil"
)

// referenceSidecar is WhatsApp Web's calculateStreamingSidecar, done in one go.
func referenceSidecar(macKey, data []byte) []byte {
	var out []byte
	for start := 0; start < len(data)-sidecarOverlap; start += sidecarChunkSize {
		end := min(start+sidecarChunkSize+sidecarOverlap, len(data))
		h := hmac.New(sha256.New, macKey)
		h.Write(data[start:end])
		out = append(out, h.Sum(nil)[:sidecarMACSize]...)
	}
	return out
}

func TestStreamingSidecar(t *testing.T) {
	mediaKey := make([]byte, 32)
	_, _ = rand.Read(mediaKey)
	iv, cipherKey, macKey, _ := getMediaKeys(mediaKey, MediaAudio)
	for _, size := range []int{0, 1, 100, sidecarChunkSize - 27, sidecarChunkSize - 26, sidecarChunkSize, 3*sidecarChunkSize + 5, 1 << 20} {
		plaintext := make([]byte, size)
		_, _ = rand.Read(plaintext)

		temp, err := os.CreateTemp(t.TempDir(), "upload")
		if err != nil {
			t.Fatal(err)
		}
		sidecar := newSidecarHasher(macKey, iv)
		_, _, _, _, err = cbcutil.EncryptStream(cipherKey, iv, macKey, bytes.NewReader(plaintext), teeSidecar(temp, sidecar))
		if err != nil {
			t.Fatal(err)
		}
		uploaded, _ := os.ReadFile(temp.Name())
		temp.Close()
		want := referenceSidecar(macKey, append(append([]byte{}, iv...), uploaded...))
		if got := sidecar.Sidecar(); !bytes.Equal(got, want) {
			t.Errorf("size %d: sidecar is %d bytes, want %d bytes", size, len(got), len(want))
		}
	}
}
