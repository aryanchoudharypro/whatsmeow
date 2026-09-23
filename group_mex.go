// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"encoding/json"
	"fmt"

	"go.mau.fi/whatsmeow/types"
)

const querySubGroupParticipantCounts = "24079399904996141"

// GetSubGroupParticipantCounts gets how many people are in each group of a
// community, which is how WhatsApp Web fills in the member counts in its
// community group list (the sub_groups query doesn't include them).
func (cli *Client) GetSubGroupParticipantCounts(ctx context.Context, community types.JID) (map[types.JID]int, error) {
	data, err := cli.sendMexIQ(ctx, querySubGroupParticipantCounts, map[string]any{
		"input": map[string]any{
			"group_jid":     community.String(),
			"query_context": "INTERACTIVE",
		},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Group struct {
			SubGroups struct {
				Edges []struct {
					Node struct {
						ID    string      `json:"id"`
						Count json.Number `json:"total_participants_count"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"sub_groups"`
		} `json:"xwa2_group_query_by_id"`
	}
	if err = json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse subgroup participant counts: %w", err)
	}
	counts := make(map[types.JID]int, len(resp.Group.SubGroups.Edges))
	for _, edge := range resp.Group.SubGroups.Edges {
		jid, err := types.ParseJID(edge.Node.ID)
		if err != nil {
			continue
		}
		if count, err := edge.Node.Count.Int64(); err == nil {
			counts[jid] = int(count)
		}
	}
	return counts, nil
}
