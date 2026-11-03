package whatsmeow

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestGroupMetaAIParticipant(t *testing.T) {
	alice := types.NewJID("111", types.HiddenUserServer)
	if got := groupMetaAIParticipant([]types.JID{alice}); !got.IsEmpty() {
		t.Fatalf("no bot member: got %s", got)
	}
	if got := groupMetaAIParticipant([]types.JID{alice, types.TeeMetaAIJID}); got != types.TeeMetaAIJID {
		t.Fatalf("private processing group: got %s", got)
	}
	if got := groupMetaAIParticipant([]types.JID{types.TeeMetaAIJID, types.NewMetaAIJID}); got != types.NewMetaAIJID {
		t.Fatalf("open bot group wins: got %s", got)
	}
	left := withoutMetaAIParticipants([]types.JID{alice, types.TeeMetaAIJID, types.NewMetaAIJID})
	if len(left) != 1 || left[0] != alice {
		t.Fatalf("bots not removed from fan-out: %v", left)
	}
}

func TestCarriesBotGroupMetadata(t *testing.T) {
	if !carriesBotGroupMetadata(&waE2E.Message{Conversation: new("hi")}) {
		t.Fatal("text should carry it")
	}
	if carriesBotGroupMetadata(&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}}) {
		t.Fatal("reaction should not")
	}
	if !carriesBotGroupMetadata(&waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{}}) {
		t.Fatal("edit should carry it")
	}
}
