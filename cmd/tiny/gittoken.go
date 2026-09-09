package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/auth"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// gitToken mints the GRASP-08 repository-root proof that private Git hosting
// expects, so plain git and agents can clone and push with one header. The
// key never leaves the process: it is read from an environment variable and
// used once to sign a kind 27235 event.
func gitToken(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tiny git-token", flag.ContinueOnError)
	fs.SetOutput(out)
	repo := fs.String("repo", "", "repository URL ending in .git, or any URL beneath it")
	keyEnv := fs.String("key-env", "TINY_AGENT_KEY", "environment variable holding the signing key as hex or nsec")
	format := fs.String("format", "header", "output: header (Authorization line), value (token only) or git (http.extraHeader config value)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, ok := auth.GRASP08RepositoryRoot(*repo)
	if !ok {
		return errors.New("git-token: --repo must be an HTTP or HTTPS repository URL ending in .git")
	}
	secret, err := secretKey(os.Getenv(*keyEnv))
	if err != nil {
		return fmt.Errorf("git-token: %s: %w", *keyEnv, err)
	}
	e := event.Event{Kind: 27235, CreatedAt: time.Now().Unix(), Tags: [][]string{{"u", root}, {"method", "GET"}}}
	if err := event.Sign(&e, secret); err != nil {
		return err
	}
	raw, err := event.Canonical(e)
	if err != nil {
		return err
	}
	token := base64.StdEncoding.EncodeToString(raw)
	switch *format {
	case "value":
		_, err = fmt.Fprintln(out, token)
	case "git":
		_, err = fmt.Fprintf(out, "http.extraHeader=Authorization: Nostr %s\n", token)
	case "header":
		_, err = fmt.Fprintf(out, "Authorization: Nostr %s\n", token)
	default:
		return fmt.Errorf("git-token: unknown --format %q", *format)
	}
	return err
}

// secretKey accepts a 64-character hex key or an nsec.
func secretKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("no key set")
	}
	if len(value) == 64 && strings.Trim(strings.ToLower(value), "0123456789abcdef") == "" {
		return strings.ToLower(value), nil
	}
	if strings.HasPrefix(strings.ToLower(value), "nsec1") {
		data, ok := bech32Data(strings.ToLower(value))
		if !ok || len(data) != 32 {
			return "", errors.New("invalid nsec")
		}
		return hex.EncodeToString(data), nil
	}
	return "", errors.New("expected a hex key or an nsec")
}

const bech32Alphabet = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32Data decodes a bech32 string and returns its 8-bit payload.
func bech32Data(value string) ([]byte, bool) {
	separator := strings.LastIndexByte(value, '1')
	if separator < 1 || separator+7 > len(value) {
		return nil, false
	}
	hrp, encoded := value[:separator], value[separator+1:]
	values := make([]byte, len(encoded))
	for i, char := range encoded {
		position := strings.IndexRune(bech32Alphabet, char)
		if position < 0 {
			return nil, false
		}
		values[i] = byte(position)
	}
	expanded := make([]byte, 0, len(hrp)*2+1+len(values))
	for _, c := range hrp {
		expanded = append(expanded, byte(c)>>5)
	}
	expanded = append(expanded, 0)
	for _, c := range hrp {
		expanded = append(expanded, byte(c)&31)
	}
	expanded = append(expanded, values...)
	generators := [...]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	checksum := uint32(1)
	for _, v := range expanded {
		top := checksum >> 25
		checksum = (checksum&0x1ffffff)<<5 ^ uint32(v)
		for i, generator := range generators {
			if top>>i&1 == 1 {
				checksum ^= generator
			}
		}
	}
	if checksum != 1 {
		return nil, false
	}
	data := values[:len(values)-6]
	accumulator, bits := 0, 0
	var result []byte
	for _, v := range data {
		accumulator = accumulator<<5 | int(v)
		bits += 5
		if bits >= 8 {
			bits -= 8
			result = append(result, byte(accumulator>>bits&255))
		}
	}
	return result, true
}
