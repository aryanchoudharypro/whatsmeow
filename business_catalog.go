// Copyright (c) 2026 Aryan Choudhary
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"go.mau.fi/whatsmeow/types"
)

// WhatsApp Web reads catalogs and orders over MEX (persisted GraphQL) and has
// no IQ fallback for the catalog. The doc ids, variable spellings and response
// paths follow whatsapp-rust's features/business.rs, which checks them
// against WA Web's WAWebQueryCatalogQuery and WAWebBizQueryOrderJobQuery.
const (
	queryBusinessCatalog = "30445081048424116"
	queryBusinessOrder   = "26593811266898374"
)

// CatalogPrice is a price in thousandths of the currency's main unit, as
// WhatsApp sends it (priceAmount1000): 1990 is 1.99.
type CatalogPrice struct {
	Amount1000 int64
	Currency   string
}

// CatalogProduct is one product in a business catalog. Only ID is always
// set: the server leaves out what it doesn't have.
type CatalogProduct struct {
	ID           string
	RetailerID   string
	Name         string
	Description  string
	URL          string
	Price        *CatalogPrice
	SalePrice    *CatalogPrice
	SaleStart    string
	SaleEnd      string
	Hidden       bool
	Availability string // IN_STOCK, OUT_OF_STOCK, AVAILABLE_FOR_ANOTHER_POSTCODE, or another value the server sends
	MaxAvailable int64
	ImageURLs    []string
}

// BusinessCatalog is one page of a business catalog. After is the cursor for
// the next page, empty on the last one.
type BusinessCatalog struct {
	Products []CatalogProduct
	After    string
}

// GetBusinessCatalog fetches one page of a business's product catalog. Pass
// the previous page's After to get the next one; limit 0 asks for 10.
func (cli *Client) GetBusinessCatalog(ctx context.Context, jid types.JID, limit int, after string) (*BusinessCatalog, error) {
	if limit <= 0 {
		limit = 10
	}
	// WA Web sends the numbers as strings and allow_shop_source as an enum.
	request := map[string]any{
		"jid":               jid.ToNonAD().String(),
		"limit":             strconv.Itoa(limit),
		"width":             "100",
		"height":            "100",
		"allow_shop_source": "ALLOWSHOPSOURCE_FALSE",
	}
	if after != "" {
		request["after"] = after
	}
	data, err := cli.sendMexIQ(ctx, queryBusinessCatalog, map[string]any{
		"request": map[string]any{"product_catalog": request},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Root struct {
			Catalog *struct {
				Products []json.RawMessage `json:"products"`
				Paging   struct {
					After any `json:"after"`
				} `json:"paging"`
			} `json:"product_catalog"`
		} `json:"xwa_product_catalog_get_product_catalog"`
	}
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse catalog: %w", err)
	}
	if resp.Root.Catalog == nil {
		return nil, fmt.Errorf("catalog response has no product_catalog")
	}
	out := &BusinessCatalog{After: mexString(resp.Root.Catalog.Paging.After)}
	for _, raw := range resp.Root.Catalog.Products {
		var p map[string]any
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		product := parseCatalogProduct(p)
		if product.ID != "" {
			out.Products = append(out.Products, product)
		}
	}
	return out, nil
}

func parseCatalogProduct(p map[string]any) CatalogProduct {
	currency := mexString(p["currency"])
	product := CatalogProduct{
		ID:           mexString(p["id"]),
		RetailerID:   mexString(p["retailer_id"]),
		Name:         mexString(p["name"]),
		Description:  mexString(p["description"]),
		URL:          mexString(p["url"]),
		Price:        mexPrice(p["price"], currency),
		Hidden:       mexBool(p["is_hidden"]),
		Availability: mexString(p["product_availability"]),
		MaxAvailable: mexInt(p["max_available"]),
	}
	if sale, ok := p["sale_price"].(map[string]any); ok {
		product.SalePrice = mexPrice(sale["price"], currency)
		// A sale window is both dates or neither, as WA Web reads it.
		start, end := mexString(sale["start_date"]), mexString(sale["end_date"])
		if start != "" && end != "" {
			product.SaleStart, product.SaleEnd = start, end
		}
	}
	if media, ok := p["media"].(map[string]any); ok {
		images, _ := media["images"].([]any)
		for _, img := range images {
			if m, ok := img.(map[string]any); ok {
				if u := mexString(m["original_image_url"]); u != "" {
					product.ImageURLs = append(product.ImageURLs, u)
				} else if u = mexString(m["request_image_url"]); u != "" {
					product.ImageURLs = append(product.ImageURLs, u)
				}
			}
		}
	}
	return product
}

// BusinessOrderItem is one line of an order: a snapshot, so the price is
// what was quoted when the order was placed.
type BusinessOrderItem struct {
	ID       string
	Name     string
	Price    *CatalogPrice
	Quantity int64
	// Variant is the chosen size, colour and so on, as name/value pairs.
	Variant [][2]string
}

// BusinessOrder is an order's line items and totals.
type BusinessOrder struct {
	Items     []BusinessOrderItem
	Currency  string
	Subtotal  *CatalogPrice
	Total     *CatalogPrice
	CreatedAt int64 // Unix seconds, 0 when the server leaves it out
}

// GetBusinessOrder looks up an order over MEX, as WA Web does. orderID and
// token come from the OrderMessage; seller is the business it was placed
// with. GetOrderDetails is the older IQ form of the same lookup.
func (cli *Client) GetBusinessOrder(ctx context.Context, seller types.JID, orderID, token string) (*BusinessOrder, error) {
	data, err := cli.sendMexIQ(ctx, queryBusinessOrder, map[string]any{
		"request": map[string]any{"order": map[string]any{
			"id":               orderID,
			"jid":              seller.ToNonAD().String(),
			"token":            map[string]any{"sensitive_string_value": token},
			"image_dimensions": map[string]any{"width": 100, "height": 100},
		}},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Root struct {
			Order map[string]any `json:"order"`
		} `json:"xwa_checkout_get_order_info"`
	}
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse order: %w", err)
	}
	order := resp.Root.Order
	if order == nil {
		return nil, fmt.Errorf("order response has no order")
	}
	out := &BusinessOrder{CreatedAt: mexInt(order["creation_time_stamp"])}
	products, _ := order["products"].([]any)
	for _, raw := range products {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		item := BusinessOrderItem{
			ID:       mexString(p["id"]),
			Name:     mexString(p["name"]),
			Price:    mexPrice(p["price"], mexString(p["currency"])),
			Quantity: mexInt(p["quantity"]),
		}
		if variant, ok := p["variant_info"].(map[string]any); ok {
			props, _ := variant["variant_properties"].([]any)
			for _, prop := range props {
				if m, ok := prop.(map[string]any); ok {
					item.Variant = append(item.Variant, [2]string{mexString(m["name"]), mexString(m["value"])})
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	if details, ok := order["price_details"].(map[string]any); ok {
		out.Currency = mexString(details["currency"])
		out.Subtotal = mexPrice(details["subtotal_amount"], out.Currency)
		out.Total = mexPrice(details["total_amount"], out.Currency)
	}
	return out, nil
}

// mexString reads a GraphQL string, which may also arrive as a number.
func mexString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

func mexInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// mexBool reads a boolean, which may be true/false, "true"/"false", or
// is_hidden's own ISHIDDEN_TRUE spelling.
func mexBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "ISHIDDEN_TRUE"
	}
	return false
}

// mexPrice reads a scaled-integer amount; empty or missing is no price, not 0.
func mexPrice(v any, currency string) *CatalogPrice {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if x == "" || err != nil {
			return nil
		}
		return &CatalogPrice{Amount1000: n, Currency: currency}
	case float64:
		return &CatalogPrice{Amount1000: int64(x), Currency: currency}
	}
	return nil
}
