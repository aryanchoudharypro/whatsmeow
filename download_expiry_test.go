package whatsmeow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMediaURLExpiry(t *testing.T) {
	at := time.Unix(1790000000, 0)
	got, ok := MediaURLExpiry("/v/t62/abc?ccb=11-4&oh=x&oe=" + strings.ToUpper(strconv.FormatInt(at.Unix(), 16)))
	if !ok || !got.Equal(at) {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok = MediaURLExpiry("/v/t62/abc?ccb=11-4"); ok {
		t.Fatal("no oe should report none")
	}
}

func TestIsMediaExpired403(t *testing.T) {
	cli := &Client{mediaHTTP: http.DefaultClient}
	for name, tc := range map[string]struct {
		body, query string
		want        bool
	}{
		"signature expired": {body: "URL signature expired", want: true},
		"past oe":           {query: "?oe=" + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 16), want: true},
		"future oe":         {query: "?oe=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 16), want: false},
		"plain forbidden":   {body: "nope", want: false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := cli.doMediaDownloadRequest(context.Background(), srv.URL+"/x"+tc.query)
		srv.Close()
		if err == nil || !errors_Is403(err) {
			t.Fatalf("%s: expected a 403, got %v", name, err)
		}
		if got := IsMediaExpired(err); got != tc.want {
			t.Errorf("%s: IsMediaExpired = %v, want %v", name, got, tc.want)
		}
	}
}

func errors_Is403(err error) bool {
	httpErr, ok := err.(DownloadHTTPError)
	return ok && httpErr.StatusCode == http.StatusForbidden
}

func TestMediaDownloadURLMode(t *testing.T) {
	base := mediaDownloadURL(context.Background(), "h", "/v/p?oe=1", []byte{0xfb, 0xff}, "image")
	if base != "https://h/v/p?oe=1&hash=-_8%3D&mms-type=image&__wa-mms=" {
		t.Fatalf("unexpected URL %s", base)
	}
	manual := mediaDownloadURL(WithMediaDownloadMode(context.Background(), true), "h", "/v/p?oe=1", nil, "image")
	if !strings.Contains(manual, "&mode=manual&mms-type=image") {
		t.Fatalf("manual mode missing: %s", manual)
	}
}

func TestShouldRetryMediaDownloadStatus(t *testing.T) {
	for status, want := range map[int]bool{401: true, 408: true, 500: true, 503: true, 507: false, 403: false, 404: false, 429: true} {
		if got := shouldRetryMediaDownloadStatus(status); got != want {
			t.Errorf("status %d: got %v want %v", status, got, want)
		}
	}
}
