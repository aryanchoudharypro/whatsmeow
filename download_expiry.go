package whatsmeow

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type mediaDownloadModeKey struct{}

// WithMediaDownloadMode marks the downloads made with ctx as started by the
// user (manual) or by the app on its own (auto). WhatsApp Web adds this as a
// mode query parameter on every media download except thumbnails, so a
// download made without calling this leaves the parameter out.
func WithMediaDownloadMode(ctx context.Context, manual bool) context.Context {
	mode := "auto"
	if manual {
		mode = "manual"
	}
	return context.WithValue(ctx, mediaDownloadModeKey{}, mode)
}

func mediaDownloadURL(ctx context.Context, hostname, directPath string, encFileHash []byte, mmsType string) string {
	var sb strings.Builder
	sb.WriteString("https://")
	sb.WriteString(hostname)
	sb.WriteString(directPath)
	sb.WriteString("&hash=")
	sb.WriteString(url.QueryEscape(base64.URLEncoding.EncodeToString(encFileHash)))
	if mode, ok := ctx.Value(mediaDownloadModeKey{}).(string); ok {
		sb.WriteString("&mode=")
		sb.WriteString(mode)
	}
	sb.WriteString("&mms-type=")
	sb.WriteString(mmsType)
	sb.WriteString("&__wa-mms=")
	return sb.String()
}

// MediaURLExpiry reads the oe parameter of a media direct path or URL, the
// time after which WhatsApp's CDN refuses the link. ok is false when there is
// none.
func MediaURLExpiry(directPathOrURL string) (expiry time.Time, ok bool) {
	parsed, err := url.Parse(directPathOrURL)
	if err != nil {
		return time.Time{}, false
	}
	oe := parsed.Query().Get("oe")
	if oe == "" {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(oe, 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// IsMediaExpired reports whether err from a media download means the file's
// link is gone, so asking the phone to upload it again can help. Like
// WhatsApp Web (WAWebMmsClientMmsDownload) that is a 404, a 410, or a 403
// that says the signature expired or whose link is past its oe time. Any
// other 403 is a refusal a new upload won't fix.
func IsMediaExpired(err error) bool {
	var httpErr DownloadHTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return true
	case http.StatusForbidden:
		return httpErr.expired
	}
	return false
}

// isExpiredForbidden classifies a 403 the way WhatsApp Web does.
func isExpiredForbidden(resp *http.Response, requestURL string) bool {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if strings.Contains(string(body), "URL signature expired") {
		return true
	}
	expiry, ok := MediaURLExpiry(requestURL)
	return ok && !time.Now().Before(expiry)
}

// shouldRetryMediaDownloadStatus follows WAWebMmsClientIsErrorRetryable: 401,
// 408 and server errors other than 507 are tried again. 429 is kept as a
// rate limit retry that WhatsApp Web doesn't have.
func shouldRetryMediaDownloadStatus(status int) bool {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusRequestTimeout:
		return true
	case status == http.StatusInsufficientStorage:
		return false
	case status >= 500:
		return true
	}
	return status == http.StatusTooManyRequests
}
