package whatsmeow

import "testing"

func TestReplaceLongestFirst(t *testing.T) {
	got := replaceLongestFirst("@123 and @12345 hi", map[string]string{"@123": "@Ana", "@12345": "@Meta AI"})
	if got != "@Ana and @Meta AI hi" {
		t.Fatal(got)
	}
}
