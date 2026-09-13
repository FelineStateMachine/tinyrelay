package tinyrelay_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/protocol/nostr"
	"github.com/coder/websocket"
)

func authProof(t *testing.T, secret, relay, challenge string, created int64) nostr.Event {
	t.Helper()
	return signedEvent(t, secret, 22242, created, [][]string{{"relay", relay}, {"challenge", challenge}}, "")
}

func connectAuthClient(t *testing.T, httpURL string) *wireClient {
	t.Helper()
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(httpURL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &wireClient{t: t, conn: conn}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test complete") })
	msg := c.expect("AUTH", 2)
	if err := json.Unmarshal(msg[1], &c.challenge); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStandaloneNIP42RejectsProofMutations(t *testing.T) {
	_, _, c := openRelay(t, true)
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	cases := []struct {
		name  string
		proof nostr.Event
	}{
		{name: "challenge", proof: authProof(t, secret, "ws://relay.example", "wrong", now)},
		{name: "relay", proof: authProof(t, secret, "ws://other.example", c.challenge, now)},
		{name: "expired", proof: authProof(t, secret, "ws://relay.example", c.challenge, now-3600)},
		{name: "future", proof: authProof(t, secret, "ws://relay.example", c.challenge, now+3600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.send("AUTH", tc.proof)
			reply := c.expect("OK", 4)
			assertAccepted(t, reply, false)
			probe := signedEvent(t, secret, 1, time.Now().Unix(), nil, "unauthenticated probe")
			c.send("EVENT", probe)
			probeReply := c.expect("OK", 4)
			assertAccepted(t, probeReply, false)
		})
	}
	valid := authProof(t, secret, "ws://relay.example", c.challenge, now)
	c.send("AUTH", valid)
	assertAccepted(t, c.expect("OK", 4), true)
	// Repeating the proof on the same connection is idempotent. NIP-42 permits
	// authenticating more than one key on a connection, so the relay cannot
	// consume the event globally.
	c.send("AUTH", valid)
	replay := c.expect("OK", 4)
	assertAccepted(t, replay, true)
}

func TestStandaloneNIP42ProofCannotCrossConnections(t *testing.T) {
	_, httpServer, first := openRelay(t, true)
	secret, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	proof := authProof(t, secret, "ws://relay.example", first.challenge, time.Now().Unix())
	second := connectAuthClient(t, httpServer.URL)
	second.send("AUTH", proof)
	assertAccepted(t, second.expect("OK", 4), false)
}

func TestStandaloneAuthenticatedActorsCannotPublishAsEachOther(t *testing.T) {
	_, httpServer, first := openRelay(t, true)
	secrets := make([]string, 12)
	for i := range secrets {
		secret, err := nostr.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		secrets[i] = secret
	}
	for i, secret := range secrets {
		if i > 0 {
			first = connectAuthClient(t, httpServer.URL)
		}
		auth := authProof(t, secret, "ws://relay.example", first.challenge, time.Now().Unix())
		first.send("AUTH", auth)
		assertAccepted(t, first.expect("OK", 4), true)
		own := signedEvent(t, secret, 1, time.Now().Unix(), nil, "actor")
		publish(t, first, own)
		foreign := signedEvent(t, secrets[(i+1)%len(secrets)], 1, time.Now().Unix(), nil, "foreign")
		first.send("EVENT", foreign)
		assertAccepted(t, first.expect("OK", 4), false)
	}
}

func FuzzStandaloneAuthTagParsing(f *testing.F) {
	f.Add("wrong", "ws://relay.example")
	f.Add("", "ws://other.example")
	f.Add("\x00\xff", "not-a-url")
	f.Fuzz(func(t *testing.T, challenge, relay string) {
		if len(challenge) > 128 || len(relay) > 256 {
			t.Skip()
		}
		_, _, c := openRelay(t, true)
		secret, err := nostr.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		proof := authProof(t, secret, relay, challenge, time.Now().Unix())
		c.send("AUTH", proof)
		reply := c.expect("OK", 4)
		accepted := challenge == c.challenge && relay == "ws://relay.example"
		assertAccepted(t, reply, accepted)
		if !accepted {
			probe := signedEvent(t, secret, 1, time.Now().Unix(), nil, "unauthenticated probe")
			c.send("EVENT", probe)
			assertAccepted(t, c.expect("OK", 4), false)
		}
	})
}
