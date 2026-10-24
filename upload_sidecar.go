// Copyright (c) 2026 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"crypto/hmac"
	"crypto/sha256"
	"hash"
	"io"
)

const (
	sidecarChunkSize = 64 * 1024
	sidecarOverlap   = 16
	sidecarMACSize   = 10
)

// hasStreamingSidecar reports whether WhatsApp sends a streaming sidecar for this media type.
// WhatsApp Web computes one for video and audio uploads, which lets phones play them while
// they're still downloading.
func hasStreamingSidecar(appInfo MediaType) bool {
	return appInfo == MediaVideo || appInfo == MediaAudio
}

// sidecarHasher computes a streaming sidecar from the IV followed by the uploaded bytes
// (ciphertext and MAC). Each 64 KiB chunk gets the first 10 bytes of an HMAC-SHA256 over
// the chunk plus the 16 bytes after it, as WhatsApp Web's calculateStreamingSidecar does.
type sidecarHasher struct {
	mac     hash.Hash
	window  []byte
	sidecar []byte
}

func newSidecarHasher(macKey, iv []byte) *sidecarHasher {
	s := &sidecarHasher{
		mac:    hmac.New(sha256.New, macKey),
		window: make([]byte, 0, sidecarChunkSize+sidecarOverlap),
	}
	s.Write(iv)
	return s
}

func (s *sidecarHasher) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		take := min(cap(s.window)-len(s.window), len(p))
		s.window = append(s.window, p[:take]...)
		p = p[take:]
		if len(s.window) == cap(s.window) {
			s.sum()
			s.window = append(s.window[:0], s.window[sidecarChunkSize:]...)
		}
	}
	return n, nil
}

func (s *sidecarHasher) sum() {
	s.mac.Reset()
	s.mac.Write(s.window)
	s.sidecar = append(s.sidecar, s.mac.Sum(nil)[:sidecarMACSize]...)
}

// Sidecar returns the finished sidecar. Nothing may be written after calling it.
func (s *sidecarHasher) Sidecar() []byte {
	if len(s.window) > sidecarOverlap {
		s.sum()
		s.window = s.window[:0]
	}
	return s.sidecar
}

// sidecarTeeFile passes writes through to the temporary upload file and feeds them to a sidecarHasher.
type sidecarTeeFile struct {
	io.ReadWriteSeeker
	hasher *sidecarHasher
}

func (f *sidecarTeeFile) Write(p []byte) (int, error) {
	n, err := f.ReadWriteSeeker.Write(p)
	f.hasher.Write(p[:n])
	return n, err
}

// sidecarTeeFileAt is a sidecarTeeFile over a file with WriteAt, which EncryptStream prefers.
// EncryptStream writes the file front to back, so the offsets are always sequential.
type sidecarTeeFileAt struct {
	sidecarTeeFile
}

func (f *sidecarTeeFileAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := f.ReadWriteSeeker.(io.WriterAt).WriteAt(p, off)
	f.hasher.Write(p[:n])
	return n, err
}

func teeSidecar(file io.ReadWriteSeeker, hasher *sidecarHasher) io.ReadWriteSeeker {
	tee := sidecarTeeFile{ReadWriteSeeker: file, hasher: hasher}
	if _, ok := file.(io.WriterAt); ok {
		return &sidecarTeeFileAt{tee}
	}
	return &tee
}
