// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import "testing"

func TestParseNewsletterDirectoryPage(t *testing.T) {
	data := []byte(`{"xwa2_newsletters_directory_search":{"page_info":{"endCursor":"abc","hasNextPage":true},
		"result":[{"id":"120363000000000001@newsletter","thread_metadata":{"name":{"text":"News"},"subscribers_count":"1500","verification":"VERIFIED","invite":"0029Va"}},
		{"id":"120363000000000002@newsletter","thread_metadata":{"name":{"text":"Other"},"subscribers_count":42}}]}}`)
	page, err := parseNewsletterDirectoryPage(data, "xwa2_newsletters_directory_search")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Channels) != 2 || page.NextCursor != "abc" {
		t.Fatalf("page = %+v", page)
	}
	first, second := page.Channels[0], page.Channels[1]
	if first.Name != "News" || first.Subscribers != 1500 || !first.Verified || first.InviteCode != "0029Va" {
		t.Errorf("first = %+v", first)
	}
	if second.Subscribers != 42 || second.Verified {
		t.Errorf("second = %+v", second)
	}
	last, _ := parseNewsletterDirectoryPage([]byte(`{"xwa2_newsletters_directory_list":{"page_info":{"endCursor":"x","hasNextPage":"false"},"result":[]}}`), "xwa2_newsletters_directory_list")
	if last == nil || last.NextCursor != "" {
		t.Errorf("last page = %+v", last)
	}
}
