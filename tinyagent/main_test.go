package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestKeygenProducesUsableKeyPair(t *testing.T) {
	var out bytes.Buffer
	if err := keygen(&out); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["secret"] == "" || got["pubkey"] == "" {
		t.Fatalf("incomplete keygen result: %#v", got)
	}
}

func TestRPCUnknownMethodReturnsErrorBeforeEOF(t *testing.T) {
	var keyOut bytes.Buffer
	if err := keygen(&keyOut); err != nil {
		t.Fatal(err)
	}
	var pair map[string]string
	if err := json.Unmarshal(keyOut.Bytes(), &pair); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINY_PRIVATE_KEY_TEST", pair["secret"])
	var out bytes.Buffer
	input := strings.NewReader("{\"id\":7,\"method\":\"bogus\",\"params\":{}}\n")
	if err := rpc([]string{"--relay", "http://relay.invalid", "--key-env", "TINY_PRIVATE_KEY_TEST"}, input, &out); err != nil {
		t.Fatal(err)
	}
	var got response
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Message == "" {
		t.Fatalf("missing RPC error: %s", out.String())
	}
}
