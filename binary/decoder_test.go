// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package binary

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"fmt"
	mathrand "math/rand/v2"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func fakeUser(i int) string { return fmt.Sprintf("1555%07d", i) }

func msgNode() Node {
	return Node{
		Tag: "message",
		Attrs: Attrs{
			"from":        types.NewJID("120363000000000001", types.GroupServer),
			"participant": types.NewADJID(fakeUser(42), 0, 3),
			"notify":      "Fictitious Person",
			"t":           "1760000000",
			"id":          "3EB0A1B2C3D4E5F60718",
			"type":        "text",
		},
		Content: []Node{
			{Tag: "enc", Attrs: Attrs{"v": "2", "type": "skmsg"}, Content: randBytes(180)},
		},
	}
}

func receiptNode() Node {
	items := make([]Node, 50)
	for i := range items {
		items[i] = Node{Tag: "item", Attrs: Attrs{"id": fmt.Sprintf("3EB0%016X", i*7919)}}
	}
	return Node{
		Tag: "receipt",
		Attrs: Attrs{
			"from": types.NewJID(fakeUser(7), types.DefaultUserServer),
			"id":   "3EB0FFFFFFFFFFFFFFFF",
			"t":    "1760000001",
			"type": "read",
		},
		Content: []Node{{Tag: "list", Content: items}},
	}
}

func usyncNode() Node {
	users := make([]Node, 200)
	for i := range users {
		devs := []Node{{Tag: "device", Attrs: Attrs{"id": "0"}}}
		for d := 1; d <= i%4; d++ {
			devs = append(devs, Node{Tag: "device", Attrs: Attrs{"id": fmt.Sprint(d), "key-index": fmt.Sprint(d + 10)}})
		}
		users[i] = Node{
			Tag:   "user",
			Attrs: Attrs{"jid": types.NewJID(fakeUser(i), types.DefaultUserServer)},
			Content: []Node{{
				Tag: "devices",
				Content: []Node{
					{Tag: "device-list", Content: devs},
					{Tag: "key-index-list", Attrs: Attrs{"ts": "1759990000"}, Content: randBytes(40)},
				},
			}},
		}
	}
	return Node{
		Tag:   "iq",
		Attrs: Attrs{"from": types.ServerJID, "id": "123.456-7", "type": "result"},
		Content: []Node{{
			Tag:   "usync",
			Attrs: Attrs{"sid": "1760000000-1", "index": "0", "last": "true", "mode": "query", "context": "message"},
			Content: []Node{
				{Tag: "result", Content: []Node{{Tag: "devices"}}},
				{Tag: "list", Content: users},
			},
		}},
	}
}

func notificationNode() Node {
	return Node{
		Tag: "notification",
		Attrs: Attrs{
			"from": types.NewJID(fakeUser(9), types.DefaultUserServer),
			"type": "picture",
			"id":   "1234567890",
			"t":    "1760000002",
		},
		Content: []Node{{Tag: "set", Attrs: Attrs{"jid": types.NewJID(fakeUser(9), types.DefaultUserServer), "id": "1760000002"}}},
	}
}

var benchNodes = []struct {
	name string
	fn   func() Node
}{
	{"message", msgNode},
	{"receipt50", receiptNode},
	{"usync200", usyncNode},
	{"notification", notificationNode},
}

func BenchmarkMarshal(b *testing.B) {
	for _, bn := range benchNodes {
		n := bn.fn()
		b.Run(bn.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = Marshal(n)
			}
		})
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	for _, bn := range benchNodes {
		data, _ := Marshal(bn.fn())
		data = data[1:] // strip leading frame-flag byte, as handleFrame does via Unpack
		b.Run(fmt.Sprintf("%s_%dB", bn.name, len(data)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, err := Unmarshal(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func compressedFrame(n Node) []byte {
	data, _ := Marshal(n)
	var buf bytes.Buffer
	buf.WriteByte(2)
	zw := zlib.NewWriter(&buf)
	zw.Write(data[1:])
	zw.Close()
	return buf.Bytes()
}

// BenchmarkUnpackZlib measures the compressed-frame path: Unpack (zlib) + Unmarshal.
func BenchmarkUnpackZlib(b *testing.B) {
	for _, bn := range []struct {
		name string
		fn   func() Node
	}{{"usync200", usyncNode}, {"notification", notificationNode}} {
		frame := compressedFrame(bn.fn())
		b.Run(fmt.Sprintf("%s_unpack_%dB", bn.name, len(frame)), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Unpack(frame); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("%s_unpack+unmarshal", bn.name), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				d, err := Unpack(frame)
				if err != nil {
					b.Fatal(err)
				}
				if _, err = Unmarshal(d); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkUnpackPlain(b *testing.B) {
	data, _ := Marshal(msgNode())
	b.ReportAllocs()
	for b.Loop() {
		d, _ := Unpack(data)
		_, _ = Unmarshal(d)
	}
}

// BenchmarkXMLString measures node.String(), which handleFrame's recvLog.Debugf("%s", node) pays per frame when debug logging is on.
func BenchmarkXMLString(b *testing.B) {
	for _, bn := range benchNodes {
		n := bn.fn()
		b.Run(bn.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = n.String()
			}
		})
	}
}

func TestRoundTripBench(t *testing.T) {
	for _, bn := range benchNodes {
		data, _ := Marshal(bn.fn())
		if _, err := Unmarshal(data[1:]); err != nil {
			t.Fatal(bn.name, err)
		}
		frame := compressedFrame(bn.fn())
		d, err := Unpack(frame)
		if err != nil {
			t.Fatal("unpack", bn.name, err)
		}
		if _, err := Unmarshal(d); err != nil {
			t.Fatal("unpack+unmarshal", bn.name, err)
		}
	}
}

func TestDecodeEquivalence(t *testing.T) {
	for _, bn := range benchNodes {
		n := bn.fn()
		data, _ := Marshal(n)
		got, err := Unmarshal(data[1:])
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != n.String() {
			t.Fatalf("%s mismatch:\n%s\n---\n%s", bn.name, got.String(), n.String())
		}
	}
}

// Malformed frames must come back as errors, never panics.
func TestDecodeGarbageDoesNotPanic(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(1, 2))
	valid, _ := Marshal(usyncNode())
	for i := range 20000 {
		var data []byte
		if i%2 == 0 {
			data = make([]byte, rng.IntN(64))
			for j := range data {
				data[j] = byte(rng.UintN(256))
			}
		} else {
			// Corrupt a few bytes of a real stanza.
			data = append([]byte(nil), valid[1:]...)
			for range 1 + rng.IntN(4) {
				data[rng.IntN(len(data))] = byte(rng.UintN(256))
			}
		}
		_, _ = Unmarshal(data)
		_, _ = Unpack(data)
	}
	if _, err := Unpack(nil); err == nil {
		t.Fatal("empty frame accepted")
	}
}
