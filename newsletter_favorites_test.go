package whatsmeow

import (
	"context"
	"crypto/sha256"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func favoritesMutation(op waServerSync.SyncdMutation_SyncdOperation, action *waSyncAction.FavoritesAction) appstate.Mutation {
	return appstate.Mutation{
		Operation: op,
		Index:     []string{appstate.IndexFavorites},
		Action: &waSyncAction.SyncActionValue{
			Timestamp:       proto.Int64(1_700_000_000_000),
			FavoritesAction: action,
		},
	}
}

func dispatchFavorites(mutation appstate.Mutation) any {
	cli := &Client{Log: waLog.Noop}
	return cli.dispatchAppState(context.Background(), appstate.WAPatchRegularHigh, mutation, true)
}

func TestFavoritesMutationKeepsTheListInOrder(t *testing.T) {
	evt, ok := dispatchFavorites(favoritesMutation(waServerSync.SyncdMutation_SET, &waSyncAction.FavoritesAction{
		Favorites: []*waSyncAction.FavoritesAction_Favorite{
			{ID: proto.String("15550000002@s.whatsapp.net")},
			{ID: proto.String("120363000000000042@g.us")},
		},
	})).(*events.Favorites)
	if !ok {
		t.Fatal("favorites SET did not dispatch a Favorites event")
	}
	got := evt.Action.GetFavorites()
	if len(got) != 2 || got[0].GetID() != "15550000002@s.whatsapp.net" || got[1].GetID() != "120363000000000042@g.us" {
		t.Fatalf("favorites list not kept in order: %v", got)
	}
	if !evt.FromFullSync || evt.Timestamp.UnixMilli() != 1_700_000_000_000 {
		t.Fatalf("wrong metadata: %+v", evt)
	}
}

func TestFavoritesEmptyListStillFires(t *testing.T) {
	evt, ok := dispatchFavorites(favoritesMutation(waServerSync.SyncdMutation_SET, &waSyncAction.FavoritesAction{})).(*events.Favorites)
	if !ok || len(evt.Action.GetFavorites()) != 0 {
		t.Fatal("an empty favorites list is the valid no-favorites state and must fire")
	}
}

func TestFavoritesMalformedOrRemoveIsDropped(t *testing.T) {
	if evt := dispatchFavorites(favoritesMutation(waServerSync.SyncdMutation_SET, nil)); evt != nil {
		t.Fatalf("SET without favoritesAction dispatched %T", evt)
	}
	if evt := dispatchFavorites(favoritesMutation(waServerSync.SyncdMutation_REMOVE, &waSyncAction.FavoritesAction{})); evt != nil {
		t.Fatalf("REMOVE dispatched %T", evt)
	}
}

var testChannel = types.NewJID("120363000000000001", types.NewsletterServer)

func addOnVote(hashes ...[]byte) waBinary.Node {
	votes := make([]waBinary.Node, len(hashes))
	for i, h := range hashes {
		votes[i] = waBinary.Node{Tag: "vote", Content: h}
	}
	return waBinary.Node{Tag: "votes", Attrs: waBinary.Attrs{"t": "1790340039"}, Content: votes}
}

func myAddOnsNode(channel types.JID, messages ...waBinary.Node) *waBinary.Node {
	return &waBinary.Node{Tag: "my_addons", Content: []waBinary.Node{{
		Tag: "messages", Attrs: waBinary.Attrs{"jid": channel}, Content: messages,
	}}}
}

func TestParseNewsletterMyAddOns(t *testing.T) {
	a := sha256.Sum256([]byte("Good morning"))
	b := sha256.Sum256([]byte("Mondays"))
	node := myAddOnsNode(testChannel,
		waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{"server_id": "777"}, Content: []waBinary.Node{addOnVote(a[:], b[:])}},
		waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{"server_id": "778"}, Content: []waBinary.Node{
			{Tag: "reaction", Attrs: waBinary.Attrs{"code": "👍", "t": "1790340000"}},
		}},
		waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{"server_id": "779"}, Content: []waBinary.Node{addOnVote()}},
	)
	got, err := parseNewsletterMyAddOns(node, testChannel)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d add-ons, want 3", len(got))
	}
	if got[0].MessageServerID != 777 || got[0].PollVote == nil || len(got[0].PollVote.OptionHashes) != 2 ||
		got[0].PollVote.OptionHashes[0] != a || got[0].PollVote.OptionHashes[1] != b || got[0].Reaction != nil {
		t.Fatalf("vote parsed wrong: %+v", got[0])
	}
	if got[1].Reaction == nil || got[1].Reaction.Code != "👍" || got[1].Reaction.Timestamp.Unix() != 1790340000 || got[1].PollVote != nil {
		t.Fatalf("reaction parsed wrong: %+v", got[1])
	}
	if got[2].PollVote == nil || len(got[2].PollVote.OptionHashes) != 0 {
		t.Fatalf("a taken-back vote must be present with no options: %+v", got[2])
	}
}

func TestParseNewsletterMyAddOnsIsStrict(t *testing.T) {
	node := myAddOnsNode(testChannel, waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{"server_id": "777"},
		Content: []waBinary.Node{addOnVote([]byte("short"))}})
	if _, err := parseNewsletterMyAddOns(node, testChannel); err == nil {
		t.Fatal("a vote that isn't a 32-byte hash must fail the whole answer")
	}
	other := types.NewJID("120363000000000099", types.NewsletterServer)
	got, err := parseNewsletterMyAddOns(myAddOnsNode(other, waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{"server_id": "1"}}), testChannel)
	if err != nil || len(got) != 0 {
		t.Fatalf("another channel's add-ons must be skipped, got %v %v", got, err)
	}
}

func TestNewsletterSendPollVoteRefusesBadInput(t *testing.T) {
	cli := &Client{Log: waLog.Noop}
	ctx := context.Background()
	if err := cli.NewsletterSendPollVote(ctx, types.NewJID("123", types.DefaultUserServer), 1, nil); err == nil {
		t.Error("voting to a non-channel JID must fail before sending")
	}
	if err := cli.NewsletterSendPollVote(ctx, testChannel, 1, []string{"A", "B", "A"}); err == nil {
		t.Error("a repeated option must fail before sending")
	}
	if err := cli.NewsletterSendPollVote(ctx, testChannel, 1, make([]string, maxNewsletterPollVoteOptions+1)); err == nil {
		t.Error("too many options must fail before sending")
	}
}

func TestNewsletterLiveUpdateCarriesForwardsAndVotes(t *testing.T) {
	hash := sha256.Sum256([]byte("Yes"))
	node := waBinary.Node{Tag: "messages", Content: []waBinary.Node{{
		Tag: "message", Attrs: waBinary.Attrs{"server_id": "777"}, Content: []waBinary.Node{
			{Tag: "forwards_count", Attrs: waBinary.Attrs{"count": "9426"}},
			{Tag: "votes", Content: []waBinary.Node{{Tag: "vote", Attrs: waBinary.Attrs{"count": "183189"}, Content: hash[:]}}},
		},
	}}}
	cli := &Client{Log: waLog.Noop}
	msgs := cli.parseNewsletterMessages(&node)
	if len(msgs) != 1 || msgs[0].ForwardsCount != 9426 || msgs[0].PollVotes[hash] != 183189 {
		t.Fatalf("live update parsed wrong: %+v", msgs)
	}
}
