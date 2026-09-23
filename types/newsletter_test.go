// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package types

import (
	"encoding/json"
	"testing"
)

func TestNewsletterViewerMuteFromSettings(t *testing.T) {
	var meta NewsletterMetadata
	raw := `{"id":"1@newsletter","thread_metadata":{"name":{"text":"News"},"subscribers_count":"12"},
		"viewer_metadata":{"role":"SUBSCRIBER","settings":[{"type":"MUTE_FOLLOWER_ACTIVITY","value":"OFF"},{"type":"MUTE_ADMIN_ACTIVITY","value":"ON"}]}}`
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	if !meta.ViewerMeta.IsMuted() || meta.ViewerMeta.Role != NewsletterRoleSubscriber || meta.ThreadMeta.SubscriberCount != 12 {
		t.Errorf("parsed %+v / %+v", meta.ViewerMeta, meta.ThreadMeta)
	}
	old := &NewsletterViewerMetadata{Mute: NewsletterMuteOn}
	if !old.IsMuted() || (*NewsletterViewerMetadata)(nil).IsMuted() {
		t.Error("older mute field misread")
	}
}
