package whatsmeow

import (
	"encoding/json"
	"testing"
)

func TestGroupLimitSharingUpdateJSON(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"data":{"xwa2_notify_group_on_prop_change":{"id":"120363000000000001","updated_by":{"id":"1@lid"},"properties":{"limit_sharing":{"limit_sharing_enabled":true,"limit_sharing_trigger":"CHAT_SETTING"}},"update_time":"1700000000"}}}`: true,
		`{"data":{"xwa2_notify_group_on_prop_change":{"id":"120363000000000001","updated_by":{"id":"1@lid"},"properties":{},"update_time":1700000000}}}`:                                                                                        false,
	} {
		var w newsLetterEventWrapper
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			t.Fatal(err)
		}
		evt := w.Data.GroupPropChange
		if evt == nil || evt.UpdateTime.String() != "1700000000" || evt.Enabled() != want {
			t.Fatalf("bad parse %+v", evt)
		}
	}
}
