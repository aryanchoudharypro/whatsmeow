package whatsmeow

import (
	"encoding/json"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
)

func TestParseCatalogProduct(t *testing.T) {
	var p map[string]any
	raw := `{"id":"7151496018233567","retailer_id":"SKU-1","name":"Blue shirt","currency":"INR",
		"price":"499000","sale_price":{"price":"399000","start_date":"2026-10-01"},
		"is_hidden":"ISHIDDEN_FALSE","product_availability":"OUT_OF_STOCK","max_available":"3",
		"media":{"images":[{"id":"1","request_image_url":"https://example.com/s.jpg","original_image_url":"https://example.com/o.jpg"}]}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	got := parseCatalogProduct(p)
	if got.ID != "7151496018233567" || got.Name != "Blue shirt" || got.RetailerID != "SKU-1" {
		t.Fatalf("bad ids/names: %+v", got)
	}
	if got.Price == nil || got.Price.Amount1000 != 499000 || got.Price.Currency != "INR" {
		t.Fatalf("bad price: %+v", got.Price)
	}
	if got.SalePrice == nil || got.SalePrice.Amount1000 != 399000 || got.SaleStart != "" {
		t.Fatalf("a lone sale start must not make a window: %+v %q", got.SalePrice, got.SaleStart)
	}
	if got.Hidden || got.Availability != "OUT_OF_STOCK" || got.MaxAvailable != 3 {
		t.Fatalf("bad flags: %+v", got)
	}
	if len(got.ImageURLs) != 1 || got.ImageURLs[0] != "https://example.com/o.jpg" {
		t.Fatalf("bad images: %v", got.ImageURLs)
	}
	if mexPrice("", "INR") != nil {
		t.Fatal("an empty amount is no price")
	}
}

func TestQuickReplyPatches(t *testing.T) {
	set := appstate.BuildQuickReply("1700000000", "hello", "Hi there", []string{"greeting"}, 3)
	m := set.Mutations[0]
	if set.Type != appstate.WAPatchRegular || m.Version != 2 || m.Index[0] != "quick_reply" || m.Index[1] != "1700000000" {
		t.Fatalf("bad set patch: %+v", set)
	}
	if a := m.Value.GetQuickReplyAction(); a.GetShortcut() != "hello" || a.GetMessage() != "Hi there" || a.GetDeleted() || a.GetCount() != 3 {
		t.Fatalf("bad set action: %v", a)
	}
	del := appstate.BuildDeleteQuickReply("1700000000")
	d := del.Mutations[0]
	if d.Operation != m.Operation || d.Index[1] != "1700000000" || !d.Value.GetQuickReplyAction().GetDeleted() {
		t.Fatalf("a delete is a SET with deleted on: %+v", d)
	}
}
