// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import "testing"

func TestABPropsBool(t *testing.T) {
	props := ABProps{1: "1", 2: "True", 3: "true", 4: "0", 5: "false", 6: "False", 7: ""}
	for code, want := range map[int]bool{1: true, 2: true, 3: true, 4: false, 5: false, 6: false, 7: false} {
		if got := props.Bool(code, !want); got != want {
			t.Errorf("Bool(%d) with %q = %v, want %v", code, props[code], got, want)
		}
	}
	if !props.Bool(99, true) || props.Bool(99, false) {
		t.Error("a flag the server didn't send should give the default")
	}
}
