package whatsmeow

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestBuildQuizPollCreation(t *testing.T) {
	cli := NewClient(&store.Device{}, waLog.Noop)
	msg, err := cli.BuildQuizPollCreation("Capital?", []string{"Paris", "Rome"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	quiz := msg.GetPollCreationMessageV3()
	if quiz.GetPollType() != waE2E.PollType_QUIZ || quiz.GetSelectableOptionsCount() != 1 ||
		quiz.GetCorrectAnswer().GetOptionName() != "Rome" || len(quiz.GetCorrectAnswer().GetOptionHash()) != 64 {
		t.Fatalf("quiz = %v", quiz)
	}
	for _, bad := range []struct {
		opts    []string
		correct int
	}{{[]string{"a"}, 0}, {[]string{"a", "a"}, 0}, {[]string{"a", "b"}, 2}} {
		if _, err := cli.BuildQuizPollCreation("q", bad.opts, bad.correct); err == nil {
			t.Errorf("accepted %v with answer %d", bad.opts, bad.correct)
		}
	}
}
