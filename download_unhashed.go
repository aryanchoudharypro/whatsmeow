// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/util/cbcutil"
)

// DownloadMediaWithoutEncHash downloads and decrypts media whose
// encrypted-file hash isn't known, such as a favorite sticker from app state,
// which only names the plain file's hash. The MAC and the plain file's hash
// are still checked. It also returns the hash of the encrypted file, which a
// message sending the same media again needs.
func (cli *Client) DownloadMediaWithoutEncHash(
	ctx context.Context,
	directPath string,
	fileHash, mediaKey []byte,
	mediaType MediaType,
	mmsType string,
) (data, encFileHash []byte, err error) {
	if !strings.HasPrefix(directPath, "/") {
		return nil, nil, fmt.Errorf("media download path does not start with slash: %s", directPath)
	}
	if len(fileHash) != 32 {
		return nil, nil, fmt.Errorf("invalid file hash length: expected 32, got %d", len(fileHash))
	}
	mediaConn, err := cli.refreshMediaConn(ctx, false)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to refresh media connections: %w", err)
	}
	if mmsType == "" {
		mmsType = mediaTypeToMMSType[mediaType]
	}
	iv, cipherKey, macKey, _ := getMediaKeys(mediaKey, mediaType)
	for _, host := range mediaConn.Hosts {
		var body []byte
		body, err = cli.downloadMedia(ctx, fmt.Sprintf("https://%s%s&mms-type=%s&__wa-mms=", host.Hostname, directPath, mmsType))
		if err != nil {
			cli.Log.Warnf("Failed to download media without hash from %s: %v", host.Hostname, err)
			continue
		}
		if len(body) <= mediaHMACLength {
			return nil, nil, ErrTooShortFile
		}
		ciphertext, mac := body[:len(body)-mediaHMACLength], body[len(body)-mediaHMACLength:]
		if err = validateMedia(iv, ciphertext, macKey, mac); err != nil {
			return nil, nil, err
		}
		if data, err = cbcutil.Decrypt(cipherKey, iv, ciphertext); err != nil {
			return nil, nil, fmt.Errorf("failed to decrypt file: %w", err)
		}
		if sha256.Sum256(data) != *(*[32]byte)(fileHash) {
			return nil, nil, ErrInvalidMediaSHA256
		}
		sum := sha256.Sum256(body)
		return data, sum[:], nil
	}
	if err == nil {
		err = fmt.Errorf("no media hosts to download from")
	}
	return nil, nil, err
}
