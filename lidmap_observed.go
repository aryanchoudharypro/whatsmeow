package whatsmeow

import (
	"context"
	"sync"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// A phone number can move to a new account, and with it a new LID. The old
// LID is then "deprecated": WhatsApp keeps its chat, shows "This phone number
// is connected to a new WhatsApp account" in it and blocks sending there. A
// LID-PN pair that was only seen in passing (a history sync conversation or
// mapping list) can name such an old LID, and storing it would make every
// message sent to the number go to the old account's chat, which the phone
// then shows as a second chat for the same person.
//
// So, like WA Web's createLidPnMappings for such sources, an observed pair is
// stored only when its phone number has no LID yet. When the number already
// has a different LID, the server is asked which one is current instead.

// lidRecheckInterval keeps the same number from being asked about for every
// history chunk that names its old LID again.
const lidRecheckInterval = time.Hour

var lidRechecked sync.Map // PN user -> time.Time

// StoreObservedLIDMappings stores LID-PN pairs that came from a source which
// doesn't say they're current (history sync), without overwriting a phone
// number's existing LID.
func (cli *Client) StoreObservedLIDMappings(ctx context.Context, mappings []store.LIDMapping) {
	fresh := make([]store.LIDMapping, 0, len(mappings))
	var recheck []types.JID
	for _, mapping := range mappings {
		if mapping.LID.Server != types.HiddenUserServer || mapping.PN.Server != types.DefaultUserServer {
			continue
		}
		current, err := cli.Store.LIDs.GetLIDForPN(ctx, mapping.PN)
		if err != nil {
			cli.Log.Warnf("Failed to get LID for %s: %v", mapping.PN, err)
			continue
		}
		if current.IsEmpty() {
			fresh = append(fresh, mapping)
		} else if current.User != mapping.LID.User {
			recheck = append(recheck, mapping.PN.ToNonAD())
		}
	}
	if len(fresh) > 0 {
		if err := cli.Store.LIDs.PutManyLIDMappings(ctx, fresh); err != nil {
			cli.Log.Errorf("Failed to store %d observed LID mappings: %v", len(fresh), err)
		}
	}
	if len(recheck) > 0 {
		go cli.recheckLIDs(context.WithoutCancel(ctx), recheck)
	}
}

// recheckLIDs asks the server for these numbers' current LIDs and stores them.
func (cli *Client) recheckLIDs(ctx context.Context, pns []types.JID) {
	now := time.Now()
	ask := make([]types.JID, 0, len(pns))
	for _, pn := range pns {
		if last, ok := lidRechecked.Load(pn.User); ok && now.Sub(last.(time.Time)) < lidRecheckInterval {
			continue
		}
		lidRechecked.Store(pn.User, now)
		ask = append(ask, pn)
	}
	if len(ask) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	list, err := cli.usync(ctx, ask, "query", "background", []waBinary.Node{{Tag: "lid"}})
	if err != nil {
		cli.Log.Warnf("Failed to recheck LIDs of %d numbers: %v", len(ask), err)
		for _, pn := range ask {
			lidRechecked.Delete(pn.User)
		}
		return
	}
	mappings := make([]store.LIDMapping, 0, len(ask))
	for _, child := range list.GetChildren() {
		pn, ok := child.Attrs["jid"].(types.JID)
		if child.Tag != "user" || !ok || pn.Server != types.DefaultUserServer {
			continue
		}
		lidNode := child.GetChildByTag("lid")
		lid := lidNode.AttrGetter().OptionalJIDOrEmpty("val")
		if lid.Server == types.HiddenUserServer {
			mappings = append(mappings, store.LIDMapping{LID: lid, PN: pn})
		}
	}
	if len(mappings) == 0 {
		return
	}
	cli.Log.Debugf("Rechecked LIDs of %d numbers whose history named another LID", len(mappings))
	if err = cli.Store.LIDs.PutManyLIDMappings(ctx, mappings); err != nil {
		cli.Log.Errorf("Failed to store rechecked LID mappings: %v", err)
	}
}
