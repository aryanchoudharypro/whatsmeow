// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// The channel directory queries WhatsApp Web's "Find channels" screen uses.
const (
	queryNewsletterDirectoryList   = "26125047313831973" // WAWebMexFetchNewsletterDirectoryListJobQuery
	queryNewsletterDirectorySearch = "26301059626252132" // WAWebMexFetchNewsletterDirectorySearchResultsJobQuery
)

// NewsletterDirectoryView is one of the directory's lists.
type NewsletterDirectoryView string

const (
	NewsletterDirectoryRecommended NewsletterDirectoryView = "RECOMMENDED"
	NewsletterDirectoryTrending    NewsletterDirectoryView = "TRENDING"
	NewsletterDirectoryPopular     NewsletterDirectoryView = "POPULAR"
	NewsletterDirectoryNew         NewsletterDirectoryView = "NEW"
)

// NewsletterDirectoryPage is one page of channels from the directory.
type NewsletterDirectoryPage struct {
	Channels []types.NewsletterDirectoryEntry
	// NextCursor asks for the following page; empty when there are no more.
	NextCursor string
}

// GetNewsletterDirectory lists channels from the directory, a page at a
// time. Pass the previous page's NextCursor to continue.
func (cli *Client) GetNewsletterDirectory(ctx context.Context, view NewsletterDirectoryView, cursor string, limit int) (*NewsletterDirectoryPage, error) {
	input := map[string]any{"view": string(view), "limit": limit}
	if cursor != "" {
		input["start_cursor"] = cursor
	}
	data, err := cli.sendMexIQ(ctx, queryNewsletterDirectoryList, map[string]any{
		"fetch_status_metadata": false,
		"input":                 input,
	})
	if err != nil {
		return nil, err
	}
	return parseNewsletterDirectoryPage(data, "xwa2_newsletters_directory_list")
}

// SearchNewsletterDirectory searches the channel directory by name.
func (cli *Client) SearchNewsletterDirectory(ctx context.Context, query, cursor string, limit int) (*NewsletterDirectoryPage, error) {
	input := map[string]any{"search_text": query, "limit": limit}
	if cursor != "" {
		input["start_cursor"] = cursor
	}
	data, err := cli.sendMexIQ(ctx, queryNewsletterDirectorySearch, map[string]any{
		"fetch_status_metadata": false,
		"input":                 input,
	})
	if err != nil {
		return nil, err
	}
	return parseNewsletterDirectoryPage(data, "xwa2_newsletters_directory_search")
}

// directoryResult is a directory entry as the server sends it. Counts and
// flags come as numbers or strings depending on the query, so they're read
// loosely.
type directoryResult struct {
	ID     string `json:"id"`
	Thread struct {
		Name struct {
			Text string `json:"text"`
		} `json:"name"`
		Description struct {
			Text string `json:"text"`
		} `json:"description"`
		Handle       string          `json:"handle"`
		Invite       string          `json:"invite"`
		Subscribers  json.RawMessage `json:"subscribers_count"`
		Verification string          `json:"verification"`
	} `json:"thread_metadata"`
}

func parseNewsletterDirectoryPage(data json.RawMessage, field string) (*NewsletterDirectoryPage, error) {
	var wrapper map[string]struct {
		PageInfo struct {
			EndCursor   string          `json:"endCursor"`
			HasNextPage json.RawMessage `json:"hasNextPage"`
		} `json:"page_info"`
		Result []directoryResult `json:"result"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse channel directory: %w", err)
	}
	list, ok := wrapper[field]
	if !ok {
		return nil, fmt.Errorf("channel directory response missing %s", field)
	}
	page := &NewsletterDirectoryPage{}
	for _, result := range list.Result {
		jid, err := types.ParseJID(result.ID)
		if err != nil {
			continue
		}
		page.Channels = append(page.Channels, types.NewsletterDirectoryEntry{
			ID:          jid,
			Name:        result.Thread.Name.Text,
			Description: result.Thread.Description.Text,
			Handle:      result.Thread.Handle,
			InviteCode:  result.Thread.Invite,
			Subscribers: looseInt(result.Thread.Subscribers),
			Verified:    strings.EqualFold(result.Thread.Verification, "verified"),
		})
	}
	if looseBool(list.PageInfo.HasNextPage) {
		page.NextCursor = list.PageInfo.EndCursor
	}
	return page, nil
}

func looseInt(raw json.RawMessage) int {
	n, _ := strconv.Atoi(strings.Trim(string(raw), `"`))
	return n
}

func looseBool(raw json.RawMessage) bool {
	value := strings.Trim(string(raw), `"`)
	return value == "true" || value == "1"
}
