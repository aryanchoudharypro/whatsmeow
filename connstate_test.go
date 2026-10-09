package whatsmeow

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.mau.fi/libsignal/ecc"
	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCert"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
)

// memConnState is a ConnStateStore that lives in memory.
type memConnState struct {
	sync.Mutex
	values map[string][]byte
}

func (m *memConnState) GetConnState(_ context.Context, key string) ([]byte, error) {
	m.Lock()
	defer m.Unlock()
	return m.values[key], nil
}

func (m *memConnState) PutConnState(_ context.Context, key string, value []byte) error {
	m.Lock()
	defer m.Unlock()
	if m.values == nil {
		m.values = make(map[string][]byte)
	}
	if value == nil {
		delete(m.values, key)
	} else {
		m.values[key] = bytes.Clone(value)
	}
	return nil
}

type memContainer struct{ saved int }

func (m *memContainer) PutDevice(context.Context, *store.Device) error    { m.saved++; return nil }
func (m *memContainer) DeleteDevice(context.Context, *store.Device) error { return nil }

func newTestDevice(state store.ConnStateStore) *store.Device {
	jid := types.NewJID("911234567890", types.DefaultUserServer)
	jid.Device = 3
	device := &store.Device{
		ID:             &jid,
		NoiseKey:       keys.NewKeyPair(),
		IdentityKey:    keys.NewKeyPair(),
		RegistrationID: 1234,
		Container:      &memContainer{},
	}
	device.SignedPreKey = device.IdentityKey.CreateSignedPreKey(1)
	device.SetAllStores(&store.NoopStore{})
	device.ConnState = state
	return device
}

// fakeNoiseServer answers the client's Noise handshake the way the WhatsApp
// edge does: XX for a plain hello, IK for a hello that carries a static key
// encrypted to the server's current one, and XXfallback when that static key
// was encrypted to a key the server no longer has.
type fakeNoiseServer struct {
	t      *testing.T
	server *httptest.Server

	sync.Mutex
	rootKey         *keys.KeyPair
	intermediateKey *keys.KeyPair
	staticKey       *keys.KeyPair
	certChain       []byte
	// hangUpOnResume makes the server drop a connection whose hello tries
	// to resume, like a server that no longer understands the hint.
	hangUpOnResume bool

	patterns    []string
	edgeRouting [][]byte
	payloads    []*waWa6.ClientPayload
}

func signCert(signer *keys.KeyPair, details *waCert.CertChain_NoiseCertificate_Details) *waCert.CertChain_NoiseCertificate {
	raw, err := proto.Marshal(details)
	if err != nil {
		panic(err)
	}
	signature := ecc.CalculateSignature(ecc.NewDjbECPrivateKey(*signer.Priv), raw)
	return &waCert.CertChain_NoiseCertificate{Details: raw, Signature: signature[:]}
}

// rotateStatic gives the server a new static key and a leaf certificate for it.
func (srv *fakeNoiseServer) rotateStatic() {
	srv.Lock()
	defer srv.Unlock()
	srv.staticKey = keys.NewKeyPair()
	notBefore := uint64(time.Now().Add(-time.Hour).Unix())
	notAfter := uint64(time.Now().Add(time.Hour).Unix())
	chain, err := proto.Marshal(&waCert.CertChain{
		Intermediate: signCert(srv.rootKey, &waCert.CertChain_NoiseCertificate_Details{
			Serial:       proto.Uint32(7),
			IssuerSerial: proto.Uint32(0),
			Key:          srv.intermediateKey.Pub[:],
			NotBefore:    &notBefore,
			NotAfter:     &notAfter,
		}),
		Leaf: signCert(srv.intermediateKey, &waCert.CertChain_NoiseCertificate_Details{
			Serial:       proto.Uint32(8),
			IssuerSerial: proto.Uint32(7),
			Key:          srv.staticKey.Pub[:],
			NotBefore:    &notBefore,
			NotAfter:     &notAfter,
		}),
	})
	if err != nil {
		panic(err)
	}
	srv.certChain = chain
}

func newFakeNoiseServer(t *testing.T) *fakeNoiseServer {
	srv := &fakeNoiseServer{t: t, rootKey: keys.NewKeyPair(), intermediateKey: keys.NewKeyPair()}
	srv.rotateStatic()
	realRoot := WACertPubKey
	WACertPubKey = *srv.rootKey.Pub
	srv.server = httptest.NewTLSServer(http.HandlerFunc(srv.serve))
	t.Cleanup(func() {
		srv.server.Close()
		WACertPubKey = realRoot
	})
	return srv
}

// httpClient returns a client whose every connection lands on the fake server.
func (srv *fakeNoiseServer) httpClient() *http.Client {
	addr := srv.server.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
}

func readFrame(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	} else if len(data) < 3 {
		return nil, errors.New("short frame")
	}
	return data[3:], nil
}

func writeFrame(ctx context.Context, conn *websocket.Conn, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	frame := append([]byte{byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))}, data...)
	return conn.Write(ctx, websocket.MessageBinary, frame)
}

func (srv *fakeNoiseServer) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()

	_, first, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var routing []byte
	if bytes.HasPrefix(first, []byte{'E', 'D', 0, 1}) {
		length := int(first[4])<<16 | int(first[5])<<8 | int(first[6])
		routing = first[7 : 7+length]
		first = first[7+length:]
	}
	if !bytes.HasPrefix(first, socket.WAConnHeader) {
		srv.t.Errorf("first frame doesn't start with the connection header")
		return
	}
	var helloMsg waWa6.HandshakeMessage
	if err = proto.Unmarshal(first[len(socket.WAConnHeader)+3:], &helloMsg); err != nil {
		srv.t.Errorf("unreadable client hello: %v", err)
		return
	}
	hello := helloMsg.GetClientHello()
	clientEphemeral := *(*[32]byte)(hello.GetEphemeral())

	srv.Lock()
	staticKey, certChain, hangUp := srv.staticKey, srv.certChain, srv.hangUpOnResume
	srv.edgeRouting = append(srv.edgeRouting, routing)
	srv.Unlock()
	if hangUp && (routing != nil || hello.GetStatic() != nil) {
		return
	}

	var payload []byte
	pattern := "XX"
	nh := socket.NewNoiseHandshake()
	ephemeral := keys.NewKeyPair()
	resumed := false
	if hello.GetStatic() != nil {
		// IK: the hello is encrypted to the static key the client cached.
		nh.Start(socket.NoiseIKStartPattern, socket.WAConnHeader)
		nh.Authenticate(staticKey.Pub[:])
		nh.Authenticate(clientEphemeral[:])
		must(srv.t, nh.MixSharedSecretIntoKey(*staticKey.Priv, clientEphemeral))
		if clientStatic, err := nh.Decrypt(hello.GetStatic()); err == nil {
			clientStaticArr := *(*[32]byte)(clientStatic)
			must(srv.t, nh.MixSharedSecretIntoKey(*staticKey.Priv, clientStaticArr))
			payload, err = nh.Decrypt(hello.GetPayload())
			must(srv.t, err)
			nh.Authenticate(ephemeral.Pub[:])
			must(srv.t, nh.MixSharedSecretIntoKey(*ephemeral.Priv, clientEphemeral))
			must(srv.t, nh.MixSharedSecretIntoKey(*ephemeral.Priv, clientStaticArr))
			must(srv.t, writeFrame(ctx, conn, &waWa6.HandshakeMessage{
				ServerHello: &waWa6.HandshakeMessage_ServerHello{
					Ephemeral: ephemeral.Pub[:],
					Payload:   nh.Encrypt(nil),
				},
			}))
			pattern = "IK"
			resumed = true
		} else {
			pattern = "XXfallback"
			nh = socket.NewNoiseHandshake()
			nh.Start(socket.NoiseXXFallbackStartPattern, socket.WAConnHeader)
			nh.Authenticate(clientEphemeral[:])
		}
	} else {
		nh.Start(socket.NoiseStartPattern, socket.WAConnHeader)
		nh.Authenticate(clientEphemeral[:])
	}
	if !resumed {
		nh.Authenticate(ephemeral.Pub[:])
		must(srv.t, nh.MixSharedSecretIntoKey(*ephemeral.Priv, clientEphemeral))
		encryptedStatic := nh.Encrypt(staticKey.Pub[:])
		must(srv.t, nh.MixSharedSecretIntoKey(*staticKey.Priv, clientEphemeral))
		must(srv.t, writeFrame(ctx, conn, &waWa6.HandshakeMessage{
			ServerHello: &waWa6.HandshakeMessage_ServerHello{
				Ephemeral: ephemeral.Pub[:],
				Static:    encryptedStatic,
				Payload:   nh.Encrypt(certChain),
			},
		}))
		finishRaw, err := readFrame(ctx, conn)
		if err != nil {
			srv.t.Errorf("no client finish: %v", err)
			return
		}
		var finishMsg waWa6.HandshakeMessage
		must(srv.t, proto.Unmarshal(finishRaw, &finishMsg))
		clientStatic, err := nh.Decrypt(finishMsg.GetClientFinish().GetStatic())
		must(srv.t, err)
		must(srv.t, nh.MixSharedSecretIntoKey(*ephemeral.Priv, *(*[32]byte)(clientStatic)))
		payload, err = nh.Decrypt(finishMsg.GetClientFinish().GetPayload())
		must(srv.t, err)
	}

	var clientPayload waWa6.ClientPayload
	must(srv.t, proto.Unmarshal(payload, &clientPayload))
	srv.Lock()
	srv.patterns = append(srv.patterns, pattern)
	srv.payloads = append(srv.payloads, &clientPayload)
	srv.Unlock()
	// Keep the connection open until the client goes away.
	_, _, _ = conn.Read(ctx)
}

func must(t *testing.T, err error) {
	if err != nil {
		t.Errorf("fake server: %v", err)
		panic(http.ErrAbortHandler)
	}
}

// last returns what the server saw on the most recent completed handshake.
func (srv *fakeNoiseServer) last(t *testing.T, want int) (pattern string, routing []byte, payload *waWa6.ClientPayload) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.Lock()
		if len(srv.patterns) >= want {
			pattern = srv.patterns[len(srv.patterns)-1]
			routing = srv.edgeRouting[len(srv.edgeRouting)-1]
			payload = srv.payloads[len(srv.payloads)-1]
			count := len(srv.patterns)
			srv.Unlock()
			if count != want {
				t.Fatalf("server completed %d handshakes, want %d", count, want)
			}
			return
		}
		srv.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("server completed fewer than %d handshakes", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func connectTestClient(t *testing.T, srv *fakeNoiseServer, device *store.Device) *Client {
	t.Helper()
	cli := NewClient(device, nil)
	cli.websocketHTTP = srv.httpClient()
	reconnectTestClient(t, cli)
	return cli
}

func reconnectTestClient(t *testing.T, cli *Client) {
	t.Helper()
	cli.Disconnect()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := cli.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(cli.Disconnect)
}

func TestHandshakeResumesWithIKAndEdgeRouting(t *testing.T) {
	srv := newFakeNoiseServer(t)
	state := &memConnState{}
	ctx := context.Background()

	// A session from before any of this was stored: a full handshake, no
	// routing hint, and the login counter whatsmeow always used to send.
	cli := connectTestClient(t, srv, newTestDevice(state))
	pattern, routing, payload := srv.last(t, 1)
	if pattern != "XX" || routing != nil || payload.GetLc() != 1 {
		t.Fatalf("first connect: pattern %s, routing %x, lc %d", pattern, routing, payload.GetLc())
	}
	if payload.GetUsername() != 911234567890 || payload.GetDevice() != 3 {
		t.Fatalf("login payload names %d:%d", payload.GetUsername(), payload.GetDevice())
	}

	// What a login does: count it, and take the server's routing hint.
	cli.bumpLoginCounter(ctx)
	cli.storeEdgeRouting(ctx, []byte{0xAA, 0xBB, 0xCC})

	reconnectTestClient(t, cli)
	pattern, routing, payload = srv.last(t, 2)
	if pattern != "IK" || !bytes.Equal(routing, []byte{0xAA, 0xBB, 0xCC}) || payload.GetLc() != 2 {
		t.Fatalf("second connect: pattern %s, routing %x, lc %d", pattern, routing, payload.GetLc())
	}

	// A new process with the same store resumes straight away.
	device := newTestDevice(state)
	device.NoiseKey = cli.Store.NoiseKey
	cli.Disconnect()
	cli = connectTestClient(t, srv, device)
	pattern, routing, payload = srv.last(t, 3)
	if pattern != "IK" || !bytes.Equal(routing, []byte{0xAA, 0xBB, 0xCC}) || payload.GetLc() != 2 {
		t.Fatalf("restart: pattern %s, routing %x, lc %d", pattern, routing, payload.GetLc())
	}

	// The server changed its static key: the resume turns into XXfallback on
	// the same connection, and the new key is what the next resume uses.
	srv.rotateStatic()
	reconnectTestClient(t, cli)
	if pattern, _, _ = srv.last(t, 4); pattern != "XXfallback" {
		t.Fatalf("after server key change: pattern %s", pattern)
	}
	reconnectTestClient(t, cli)
	if pattern, _, _ = srv.last(t, 5); pattern != "IK" {
		t.Fatalf("after fallback: pattern %s", pattern)
	}
}

func TestHandshakeRetriesPlainWhenResumeFails(t *testing.T) {
	srv := newFakeNoiseServer(t)
	state := &memConnState{}
	ctx := context.Background()

	cli := connectTestClient(t, srv, newTestDevice(state))
	srv.last(t, 1)
	cli.storeEdgeRouting(ctx, []byte{1, 2, 3})

	srv.Lock()
	srv.hangUpOnResume = true
	srv.Unlock()
	reconnectTestClient(t, cli)
	pattern, routing, _ := srv.last(t, 2)
	if pattern != "XX" || routing != nil {
		t.Fatalf("retry: pattern %s, routing %x", pattern, routing)
	}
	if raw, _ := state.GetConnState(ctx, connStateEdgeRouting); raw != nil {
		t.Fatalf("routing hint survived a failed handshake: %x", raw)
	}
	// The plain handshake that worked cached a chain again.
	if raw, _ := state.GetConnState(ctx, connStateServerCertChain); raw == nil {
		t.Fatal("no certificate chain cached after the retry")
	}
}

func TestResumeDisabledAndUnpaired(t *testing.T) {
	srv := newFakeNoiseServer(t)
	state := &memConnState{}
	cli := connectTestClient(t, srv, newTestDevice(state))
	srv.last(t, 1)
	cli.storeEdgeRouting(context.Background(), []byte{9})

	cli.DisableConnectionResume = true
	reconnectTestClient(t, cli)
	pattern, routing, _ := srv.last(t, 2)
	if pattern != "XX" || routing != nil {
		t.Fatalf("resume disabled: pattern %s, routing %x", pattern, routing)
	}
}

func TestServerCertChainValidityWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := make([]byte, 32)
	valid := &serverCertChain{
		Intermediate: noiseCert{Key: key, NotBefore: 1_700_000_000, NotAfter: 1_900_000_000},
		Leaf:         noiseCert{Key: key, NotBefore: 1_700_000_000, NotAfter: 1_900_000_000},
	}
	if !valid.usableAt(now) {
		t.Fatal("chain inside its window isn't usable")
	}
	var missing *serverCertChain
	if missing.usableAt(now) {
		t.Fatal("no chain is usable")
	}
	for name, change := range map[string]func(*serverCertChain){
		"expired leaf":             func(c *serverCertChain) { c.Leaf.NotAfter = 1_800_000_000 },
		"expired intermediate":     func(c *serverCertChain) { c.Intermediate.NotAfter = 1_700_000_001 },
		"leaf from the future":     func(c *serverCertChain) { c.Leaf.NotBefore = 1_800_000_001 },
		"intermediate from future": func(c *serverCertChain) { c.Intermediate.NotBefore = 1_800_000_001 },
		"short key":                func(c *serverCertChain) { c.Leaf.Key = key[:31] },
	} {
		chain := *valid
		change(&chain)
		if chain.usableAt(now) {
			t.Errorf("%s: chain is usable", name)
		}
	}
}

func TestEdgeRoutingPreIntro(t *testing.T) {
	if got := socket.EdgeRoutingPreIntro(nil); got != nil {
		t.Fatalf("pre-intro for no routing info: %x", got)
	}
	got := socket.EdgeRoutingPreIntro([]byte{0xDE, 0xAD})
	if want := []byte{'E', 'D', 0, 1, 0, 0, 2, 0xDE, 0xAD}; !bytes.Equal(got, want) {
		t.Fatalf("pre-intro %x, want %x", got, want)
	}
}

func TestLoginCounterStartsAtZeroAfterPairing(t *testing.T) {
	state := &memConnState{}
	cli := NewClient(newTestDevice(state), nil)
	ctx := context.Background()
	cli.initConnStateAfterPair(ctx)
	if lc := cli.Store.GetClientPayload().GetLc(); lc != 0 {
		t.Fatalf("first login after pairing sends lc %d", lc)
	}
	cli.bumpLoginCounter(ctx)
	cli.bumpLoginCounter(ctx)

	restarted := NewClient(newTestDevice(state), nil)
	restarted.loadConnState(ctx)
	if lc := restarted.Store.GetClientPayload().GetLc(); lc != 2 {
		t.Fatalf("after two logins and a restart, lc is %d", lc)
	}
}

func TestSignedPreKeyRotation(t *testing.T) {
	state := &memConnState{}
	device := newTestDevice(state)
	cli := NewClient(device, nil)
	ctx := context.Background()
	cli.loadConnState(ctx)
	original := device.SignedPreKey
	now := time.Now()

	// A session from before rotation existed only starts counting.
	uploads := 0
	countUpload := func(context.Context, *keys.PreKey) error { uploads++; return nil }
	if err := cli.maybeRotateSignedPreKey(ctx); err != nil || device.SignedPreKey != original {
		t.Fatalf("legacy session rotated right away (err %v)", err)
	}
	if raw, _ := state.GetConnState(ctx, connStateSignedPreKeyRotate); raw == nil {
		t.Fatal("rotation baseline wasn't stored")
	}

	// An upload whose outcome is unknown keeps the staged key for next time.
	var staged *keys.PreKey
	err := cli.rotateSignedPreKey(ctx, now, func(_ context.Context, key *keys.PreKey) error {
		staged = key
		return errors.New("connection lost")
	})
	if err == nil || device.SignedPreKey != original {
		t.Fatalf("failed upload changed the current key (err %v)", err)
	}
	if record, _ := device.LoadSignedPreKey(ctx, staged.KeyID); record == nil {
		t.Fatal("staged key can't decrypt, though the server may have it")
	}
	err = cli.rotateSignedPreKey(ctx, now, func(_ context.Context, key *keys.PreKey) error {
		if key != staged && *key.Pub != *staged.Pub {
			t.Errorf("retry sent a different key under ID %d", key.KeyID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}
	if device.SignedPreKey.KeyID != original.KeyID+1 || *device.SignedPreKey.Pub != *staged.Pub {
		t.Fatalf("current key is %d after rotating from %d", device.SignedPreKey.KeyID, original.KeyID)
	}
	if device.Container.(*memContainer).saved != 1 {
		t.Fatalf("device saved %d times", device.Container.(*memContainer).saved)
	}
	if record, _ := device.LoadSignedPreKey(ctx, original.KeyID); record == nil {
		t.Fatal("the rotated-out key no longer decrypts")
	}

	// A key the server refuses outright is thrown away, not sent again.
	err = cli.rotateSignedPreKey(ctx, now, func(_ context.Context, key *keys.PreKey) error {
		staged = key
		return &IQError{Code: 406, Text: "not-acceptable"}
	})
	if err == nil {
		t.Fatal("a refused key was accepted")
	}
	if record, _ := device.LoadSignedPreKey(ctx, staged.KeyID); record != nil {
		t.Fatal("a refused key is still kept")
	}
	// The refusal used up the interval, so nothing is due.
	if err = cli.maybeRotateSignedPreKey(ctx); err != nil {
		t.Fatalf("rotation after a refusal: %v", err)
	}

	// Old keys survive a restart, and only so many are kept.
	for range retiredSignedPreKeyCount + 2 {
		if err = cli.rotateSignedPreKey(ctx, now, countUpload); err != nil {
			t.Fatalf("rotation: %v", err)
		}
	}
	if uploads != retiredSignedPreKeyCount+2 {
		t.Fatalf("%d uploads", uploads)
	}
	restartedDevice := newTestDevice(state)
	restarted := NewClient(restartedDevice, nil)
	restarted.loadConnState(ctx)
	if len(restartedDevice.OldSignedPreKeys) != retiredSignedPreKeyCount {
		t.Fatalf("%d retired keys after a restart", len(restartedDevice.OldSignedPreKeys))
	}
	previous := device.SignedPreKey.KeyID - 1
	if record, _ := restartedDevice.LoadSignedPreKey(ctx, previous); record == nil {
		t.Fatalf("retired key %d isn't loadable after a restart", previous)
	}
	if record, _ := device.LoadSignedPreKey(ctx, original.KeyID); record != nil {
		t.Fatal("the oldest retired key was never pruned")
	}
	if nextSignedPreKeyID(maxSignedPreKeyID) != 1 {
		t.Fatal("signed prekey IDs don't wrap")
	}
}

func TestABPropsDeltaUpdatesTheCache(t *testing.T) {
	cli := NewClient(newTestDevice(&memConnState{}), nil)
	prop := func(code, value string) waBinary.Node {
		return waBinary.Node{Tag: "prop", Attrs: waBinary.Attrs{"config_code": code, "config_value": value}}
	}
	cli.applyABProps(&waBinary.Node{Tag: "iq", Content: []waBinary.Node{{
		Tag:     "props",
		Attrs:   waBinary.Attrs{"protocol": "1", "hash": "first"},
		Content: []waBinary.Node{prop("10", "1"), prop("20", "old")},
	}}})
	cli.applyABProps(&waBinary.Node{Tag: "iq", Content: []waBinary.Node{{
		Tag:     "props",
		Attrs:   waBinary.Attrs{"protocol": "1", "hash": "second", "delta_update": "true"},
		Content: []waBinary.Node{prop("20", "new")},
	}}})
	props := cli.CachedABProps()
	if props[10] != "1" || props[20] != "new" || len(props) != 2 || cli.abPropsHash != "second" {
		t.Fatalf("after a delta: %v, hash %s", props, cli.abPropsHash)
	}
	// A full set replaces everything, including flags it no longer carries.
	cli.applyABProps(&waBinary.Node{Tag: "iq", Content: []waBinary.Node{{
		Tag:     "props",
		Attrs:   waBinary.Attrs{"protocol": "1", "hash": "third"},
		Content: []waBinary.Node{prop("30", "x")},
	}}})
	if props = cli.CachedABProps(); len(props) != 1 || props[30] != "x" {
		t.Fatalf("after a full set: %v", props)
	}
}

func TestABPropsAreOnlyFetchedAgainWhenTheServerNamesANewSet(t *testing.T) {
	cli := NewClient(newTestDevice(&memConnState{}), nil)
	props := func(attrs waBinary.Attrs) *waBinary.Node {
		return &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "props", Attrs: attrs}}}
	}
	cli.abPropsServerRefreshID = 7
	if cli.abPropsAreCurrent() {
		t.Fatal("nothing is cached yet, so there is nothing current")
	}
	cli.applyABProps(props(waBinary.Attrs{"protocol": "1", "hash": "a", "refresh_id": "7"}))
	if !cli.abPropsAreCurrent() {
		t.Fatal("the cached set is the one the server named")
	}
	cli.abPropsServerRefreshID = 8
	if cli.abPropsAreCurrent() {
		t.Fatal("the server named a newer set")
	}
	// An answer that names no ID counts for the ID it was fetched under.
	cli.applyABProps(props(waBinary.Attrs{"protocol": "1", "hash": "b"}))
	if !cli.abPropsAreCurrent() {
		t.Fatal("the set fetched under ID 8 is current for ID 8")
	}
	// A <success> without an ID gives no reason to ask again.
	cli.abPropsServerRefreshID = 0
	if !cli.abPropsAreCurrent() {
		t.Fatal("no ID from the server means the cache stands")
	}
}

func TestUnifiedSessionIsNotSentTwiceInARow(t *testing.T) {
	cli := NewClient(newTestDevice(&memConnState{}), nil)
	// Not connected, so nothing goes out; only the gate is under test.
	cli.sendUnifiedSession()
	first := cli.lastUnifiedSession.Load()
	if first == 0 {
		t.Fatal("the first send wasn't recorded")
	}
	cli.sendUnifiedSession()
	if cli.lastUnifiedSession.Load() != first {
		t.Fatal("a second send right after the first went through the gate")
	}
	cli.lastUnifiedSession.Store(first - unifiedSessionMinGap.Milliseconds() - 1)
	cli.sendUnifiedSession()
	if cli.lastUnifiedSession.Load() <= first-unifiedSessionMinGap.Milliseconds() {
		t.Fatal("a send after the gap was held back")
	}
}
