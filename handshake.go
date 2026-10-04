// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/libsignal/ecc"
	"google.golang.org/protobuf/proto"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCert"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/util/keys"
)

const NoiseHandshakeResponseTimeout = 20 * time.Second
const WACertIssuerSerial = 0

var WACertPubKey = [...]byte{0x14, 0x23, 0x75, 0x57, 0x4d, 0xa, 0x58, 0x71, 0x66, 0xaa, 0xe7, 0x1e, 0xbe, 0x51, 0x64, 0x37, 0xc4, 0xa2, 0x8b, 0x73, 0xe3, 0x69, 0x5c, 0x6c, 0xe1, 0xf7, 0xf9, 0x54, 0x5d, 0xa8, 0xee, 0x6b}

// errIKRejected marks a Noise IK resume that failed in a way the cached
// server key explains (a decrypt or parse failure after the server answered),
// as opposed to a timeout or a dropped connection.
var errIKRejected = errors.New("noise IK resume rejected")

var errHandshakeTimeout = errors.New("timed out waiting for handshake response")

// doHandshake runs the Noise handshake for the WhatsApp web API.
//
// Like WhatsApp Web (WAWebOpenChatSocket), it uses one of three patterns:
// XX on a first connect, IK when the server's static key is cached from an
// earlier XX (one round trip, with the login payload in the first message),
// and XXfallback when the server answers an IK hello with a new static key.
//
// The returned chain is the server's certificate chain when one was verified
// (XX and XXfallback), for the caller to cache.
func (cli *Client) doHandshake(fs *socket.FrameSocket, ephemeralKP keys.KeyPair, serverStatic *[32]byte) (chan *waBinary.Node, *serverCertChain, error) {
	var clientPayload *waWa6.ClientPayload
	if cli.GetClientPayload != nil {
		clientPayload = cli.GetClientPayload()
	} else {
		clientPayload = cli.Store.GetClientPayload()
	}
	if clientPayload == nil {
		return nil, nil, fmt.Errorf("got nil client payload")
	}
	payload, err := proto.Marshal(clientPayload)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal client finish payload: %w", err)
	}

	var nh *socket.NoiseHandshake
	var chain *serverCertChain
	if serverStatic != nil {
		nh, chain, err = cli.doIKHandshake(fs, ephemeralKP, *serverStatic, payload)
	} else {
		nh, chain, err = cli.doXXHandshake(fs, ephemeralKP, payload)
	}
	if err != nil {
		return nil, nil, err
	}

	queue := make(chan *waBinary.Node, handlerQueueSize)
	ns, err := nh.Finish(fs, cli.makeFrameHandler(queue), cli.onDisconnect)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create noise socket: %w", err)
	}

	cli.socket = ns

	return queue, chain, nil
}

func sendClientHello(fs *socket.FrameSocket, hello *waWa6.HandshakeMessage_ClientHello) (*waWa6.HandshakeMessage_ServerHello, error) {
	data, err := proto.Marshal(&waWa6.HandshakeMessage{ClientHello: hello})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal handshake message: %w", err)
	}
	err = fs.SendFrame(data)
	if err != nil {
		return nil, fmt.Errorf("failed to send handshake message: %w", err)
	}
	var resp []byte
	select {
	case resp = <-fs.Frames:
	case <-fs.Context().Done():
		// Without this, a server that hangs up on the hello would only be
		// noticed when the timeout below runs out.
		return nil, fmt.Errorf("socket closed while waiting for handshake response")
	case <-time.After(NoiseHandshakeResponseTimeout):
		return nil, errHandshakeTimeout
	}
	var handshakeResponse waWa6.HandshakeMessage
	err = proto.Unmarshal(resp, &handshakeResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal handshake response: %w", err)
	}
	return handshakeResponse.GetServerHello(), nil
}

// doXXHandshake implements the Noise_XX_25519_AESGCM_SHA256 handshake.
func (cli *Client) doXXHandshake(fs *socket.FrameSocket, ephemeralKP keys.KeyPair, payload []byte) (*socket.NoiseHandshake, *serverCertChain, error) {
	nh := socket.NewNoiseHandshake()
	nh.Start(socket.NoiseStartPattern, socket.WAConnHeader)
	nh.Authenticate(ephemeralKP.Pub[:])
	serverHello, err := sendClientHello(fs, &waWa6.HandshakeMessage_ClientHello{
		Ephemeral: ephemeralKP.Pub[:],
	})
	if err != nil {
		return nil, nil, err
	}
	chain, err := cli.finishXXHandshake(nh, fs, ephemeralKP, serverHello, payload)
	if err != nil {
		return nil, nil, err
	}
	return nh, chain, nil
}

// doIKHandshake implements the Noise_IK_25519_AESGCM_SHA256 handshake against
// a cached server static key, falling back to XXfallback when the server
// answers with a static key of its own.
func (cli *Client) doIKHandshake(fs *socket.FrameSocket, ephemeralKP keys.KeyPair, serverStatic [32]byte, payload []byte) (*socket.NoiseHandshake, *serverCertChain, error) {
	nh := socket.NewNoiseHandshake()
	nh.Start(socket.NoiseIKStartPattern, socket.WAConnHeader)
	// Pre-message: the server's static key is already known to both sides.
	nh.Authenticate(serverStatic[:])
	nh.Authenticate(ephemeralKP.Pub[:])
	err := nh.MixSharedSecretIntoKey(*ephemeralKP.Priv, serverStatic)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to mix server static key in: %w", err)
	}
	encryptedPubkey := nh.Encrypt(cli.Store.NoiseKey.Pub[:])
	err = nh.MixSharedSecretIntoKey(*cli.Store.NoiseKey.Priv, serverStatic)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to mix noise private key in: %w", err)
	}
	encryptedPayload := nh.Encrypt(payload)

	serverHello, err := sendClientHello(fs, &waWa6.HandshakeMessage_ClientHello{
		Ephemeral: ephemeralKP.Pub[:],
		Static:    encryptedPubkey,
		Payload:   encryptedPayload,
	})
	if err != nil {
		return nil, nil, err
	}

	if serverHello.GetStatic() != nil {
		// The server declined the cached key and sent its current one. The
		// ephemeral key already sent is reused; the rest is a plain XX.
		cli.Log.Debugf("Noise IK resume declined by server, continuing with XXfallback")
		fallback := socket.NewNoiseHandshake()
		fallback.Start(socket.NoiseXXFallbackStartPattern, socket.WAConnHeader)
		fallback.Authenticate(ephemeralKP.Pub[:])
		chain, err := cli.finishXXHandshake(fallback, fs, ephemeralKP, serverHello, payload)
		if err != nil {
			return nil, nil, err
		}
		return fallback, chain, nil
	}

	serverEphemeral := serverHello.GetEphemeral()
	certificateCiphertext := serverHello.GetPayload()
	if len(serverEphemeral) != 32 || certificateCiphertext == nil {
		return nil, nil, fmt.Errorf("%w: missing parts of handshake response", errIKRejected)
	}
	serverEphemeralArr := *(*[32]byte)(serverEphemeral)
	nh.Authenticate(serverEphemeral)
	err = nh.MixSharedSecretIntoKey(*ephemeralKP.Priv, serverEphemeralArr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to mix server ephemeral key in: %w", err)
	}
	err = nh.MixSharedSecretIntoKey(*cli.Store.NoiseKey.Priv, serverEphemeralArr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to mix noise private key in: %w", err)
	}
	// Decrypting the payload is what authenticates the server: only the
	// holder of the cached static key can produce a valid tag here.
	_, err = nh.Decrypt(certificateCiphertext)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: failed to decrypt noise certificate ciphertext: %w", errIKRejected, err)
	}
	return nh, nil, nil
}

// finishXXHandshake handles the server hello of an XX or XXfallback handshake
// and sends the client finish.
func (cli *Client) finishXXHandshake(
	nh *socket.NoiseHandshake, fs *socket.FrameSocket, ephemeralKP keys.KeyPair,
	serverHello *waWa6.HandshakeMessage_ServerHello, payload []byte,
) (*serverCertChain, error) {
	serverEphemeral := serverHello.GetEphemeral()
	serverStaticCiphertext := serverHello.GetStatic()
	certificateCiphertext := serverHello.GetPayload()
	if len(serverEphemeral) != 32 || serverStaticCiphertext == nil || certificateCiphertext == nil {
		return nil, fmt.Errorf("missing parts of handshake response")
	}
	serverEphemeralArr := *(*[32]byte)(serverEphemeral)

	nh.Authenticate(serverEphemeral)
	err := nh.MixSharedSecretIntoKey(*ephemeralKP.Priv, serverEphemeralArr)
	if err != nil {
		return nil, fmt.Errorf("failed to mix server ephemeral key in: %w", err)
	}

	staticDecrypted, err := nh.Decrypt(serverStaticCiphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt server static ciphertext: %w", err)
	} else if len(staticDecrypted) != 32 {
		return nil, fmt.Errorf("unexpected length of server static plaintext %d (expected 32)", len(staticDecrypted))
	}
	err = nh.MixSharedSecretIntoKey(*ephemeralKP.Priv, *(*[32]byte)(staticDecrypted))
	if err != nil {
		return nil, fmt.Errorf("failed to mix server static key in: %w", err)
	}

	certDecrypted, err := nh.Decrypt(certificateCiphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt noise certificate ciphertext: %w", err)
	}
	chain, err := verifyServerCert(certDecrypted, staticDecrypted)
	if err != nil {
		return nil, fmt.Errorf("failed to verify server cert: %w", err)
	}

	encryptedPubkey := nh.Encrypt(cli.Store.NoiseKey.Pub[:])
	err = nh.MixSharedSecretIntoKey(*cli.Store.NoiseKey.Priv, serverEphemeralArr)
	if err != nil {
		return nil, fmt.Errorf("failed to mix noise private key in: %w", err)
	}

	encryptedClientFinishPayload := nh.Encrypt(payload)
	data, err := proto.Marshal(&waWa6.HandshakeMessage{
		ClientFinish: &waWa6.HandshakeMessage_ClientFinish{
			Static:  encryptedPubkey,
			Payload: encryptedClientFinishPayload,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal handshake finish message: %w", err)
	}
	err = fs.SendFrame(data)
	if err != nil {
		return nil, fmt.Errorf("failed to send handshake finish message: %w", err)
	}
	return chain, nil
}

// noiseCert is the part of a Noise certificate worth keeping: the key and the
// window it is valid in. It mirrors what WhatsApp Web stores
// (setCertificateChain).
type noiseCert struct {
	Key       []byte `json:"key"`
	NotBefore int64  `json:"not_before"`
	NotAfter  int64  `json:"not_after"`
}

// serverCertChain is the server's verified certificate chain. The leaf key is
// the server's static key, which is what a Noise IK resume needs.
type serverCertChain struct {
	Intermediate noiseCert `json:"intermediate"`
	Leaf         noiseCert `json:"leaf"`
}

// usableAt reports whether both certificates are inside their validity window.
// Both ends are checked: not_after is ordinary expiry, not_before catches a
// clock that was set back.
func (chain *serverCertChain) usableAt(now time.Time) bool {
	if chain == nil || len(chain.Leaf.Key) != 32 {
		return false
	}
	ts := now.Unix()
	return ts >= chain.Leaf.NotBefore && ts >= chain.Intermediate.NotBefore &&
		ts < chain.Leaf.NotAfter && ts < chain.Intermediate.NotAfter
}

func checkCertValidity(cert *waCert.CertChain_NoiseCertificate_Details) error {
	notBefore := time.Unix(int64(cert.GetNotBefore()), 0)
	notAfter := time.Unix(int64(cert.GetNotAfter()), 0)
	now := time.Now()
	if now.Before(notBefore) {
		return fmt.Errorf("certificate not valid yet (current time %s is before %s)", now, notBefore)
	} else if now.After(notAfter) {
		return fmt.Errorf("certificate expired (current time %s is after %s)", now, notAfter)
	}
	return nil
}

func verifyServerCert(certDecrypted, staticDecrypted []byte) (*serverCertChain, error) {
	var certChain waCert.CertChain
	err := proto.Unmarshal(certDecrypted, &certChain)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal noise certificate: %w", err)
	}
	var intermediateCertDetails, leafCertDetails waCert.CertChain_NoiseCertificate_Details
	intermediateCertDetailsRaw := certChain.GetIntermediate().GetDetails()
	intermediateCertSignature := certChain.GetIntermediate().GetSignature()
	leafCertDetailsRaw := certChain.GetLeaf().GetDetails()
	leafCertSignature := certChain.GetLeaf().GetSignature()
	if intermediateCertDetailsRaw == nil || intermediateCertSignature == nil || leafCertDetailsRaw == nil || leafCertSignature == nil {
		return nil, fmt.Errorf("missing parts of noise certificate")
	} else if len(intermediateCertSignature) != 64 {
		return nil, fmt.Errorf("unexpected length of intermediate cert signature %d (expected 64)", len(intermediateCertSignature))
	} else if len(leafCertSignature) != 64 {
		return nil, fmt.Errorf("unexpected length of leaf cert signature %d (expected 64)", len(leafCertSignature))
	} else if !ecc.VerifySignature(ecc.NewDjbECPublicKey(WACertPubKey), intermediateCertDetailsRaw, [64]byte(intermediateCertSignature)) {
		return nil, fmt.Errorf("failed to verify intermediate cert signature")
	} else if err = proto.Unmarshal(intermediateCertDetailsRaw, &intermediateCertDetails); err != nil {
		return nil, fmt.Errorf("failed to unmarshal noise certificate details: %w", err)
	} else if intermediateCertDetails.GetIssuerSerial() != WACertIssuerSerial {
		return nil, fmt.Errorf("unexpected intermediate issuer serial %d (expected %d)", intermediateCertDetails.GetIssuerSerial(), WACertIssuerSerial)
	} else if len(intermediateCertDetails.GetKey()) != 32 {
		return nil, fmt.Errorf("unexpected length of intermediate cert key %d (expected 32)", len(intermediateCertDetails.GetKey()))
	} else if !ecc.VerifySignature(ecc.NewDjbECPublicKey([32]byte(intermediateCertDetails.GetKey())), leafCertDetailsRaw, [64]byte(leafCertSignature)) {
		return nil, fmt.Errorf("failed to verify intermediate cert signature")
	} else if err = checkCertValidity(&intermediateCertDetails); err != nil {
		return nil, fmt.Errorf("intermediate cert %w", err)
	} else if err = proto.Unmarshal(leafCertDetailsRaw, &leafCertDetails); err != nil {
		return nil, fmt.Errorf("failed to unmarshal noise certificate details: %w", err)
	} else if leafCertDetails.GetIssuerSerial() != intermediateCertDetails.GetSerial() {
		return nil, fmt.Errorf("unexpected leaf issuer serial %d (expected %d)", leafCertDetails.GetIssuerSerial(), intermediateCertDetails.GetSerial())
	} else if !hmac.Equal(leafCertDetails.GetKey(), staticDecrypted) {
		return nil, fmt.Errorf("cert key doesn't match decrypted static")
	} else if err = checkCertValidity(&leafCertDetails); err != nil {
		return nil, fmt.Errorf("leaf cert cert %w", err)
	}
	return &serverCertChain{
		Intermediate: noiseCert{
			Key:       intermediateCertDetails.GetKey(),
			NotBefore: int64(intermediateCertDetails.GetNotBefore()),
			NotAfter:  int64(intermediateCertDetails.GetNotAfter()),
		},
		Leaf: noiseCert{
			Key:       leafCertDetails.GetKey(),
			NotBefore: int64(leafCertDetails.GetNotBefore()),
			NotAfter:  int64(leafCertDetails.GetNotAfter()),
		},
	}, nil
}
