// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package binary

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"sync"
)

type pooledInflater struct {
	src bytes.Reader
	zr  io.ReadCloser
}

var inflaterPool sync.Pool

// Unpack unpacks the given decrypted data from the WhatsApp web API.
//
// It checks the first byte to decide whether to uncompress the data with zlib or just return as-is
// (without the first byte). There's currently no corresponding Pack function because Marshal
// already returns the data with a leading zero (i.e. not compressed).
func Unpack(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty frame")
	}
	dataType, data := data[0], data[1:]
	if 2&dataType == 0 {
		return data, nil
	}
	pi, _ := inflaterPool.Get().(*pooledInflater)
	if pi == nil {
		pi = &pooledInflater{}
	}
	pi.src.Reset(data)
	var err error
	if pi.zr == nil {
		pi.zr, err = zlib.NewReader(&pi.src)
	} else {
		err = pi.zr.(zlib.Resetter).Reset(&pi.src, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create zlib reader: %w", err)
	}
	// The output is aliased by the decoded Node ([]byte contents point into it), so it can't be pooled.
	out := bytes.NewBuffer(make([]byte, 0, 4*len(data)+64))
	_, err = out.ReadFrom(pi.zr)
	pi.src.Reset(nil)
	inflaterPool.Put(pi)
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
