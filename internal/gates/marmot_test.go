package gates

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestMarmotGroupShape(t *testing.T) {
	e := event.Event{Kind: event.KIND_MARMOT_GROUP, Content: base64.StdEncoding.EncodeToString(make([]byte, 28)), Tags: [][]string{{"h", strings.Repeat("a", 64)}}}
	if err := MarmotShape(e); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*event.Event)
	}{
		{"bad h", func(x *event.Event) { x.Tags[0][1] = strings.Repeat("A", 64) }},
		{"short content", func(x *event.Event) { x.Content = base64.StdEncoding.EncodeToString([]byte("short")) }},
		{"unknown tag", func(x *event.Event) { x.Tags = append(x.Tags, []string{"x", "y"}) }},
		{"duplicate expiration", func(x *event.Event) {
			x.Tags = append(x.Tags, []string{"expiration", "1"}, []string{"expiration", "2"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := e
			copy.Tags = append([][]string(nil), e.Tags...)
			for i := range copy.Tags {
				copy.Tags[i] = append([]string(nil), copy.Tags[i]...)
			}
			tc.mutate(&copy)
			if MarmotShape(copy) == nil {
				t.Fatal("invalid shape accepted")
			}
		})
	}
}

func TestMarmotKeyPackageShape(t *testing.T) {
	e := event.Event{Kind: event.KIND_MARMOT_KEY_PACKAGE, Content: base64.StdEncoding.EncodeToString([]byte("package")), Tags: [][]string{{"d", strings.Repeat("a", 64)}, {"mls_protocol_version", "1.0"}, {"i", "01"}, {"mls_ciphersuite", "0x0001"}, {"mls_extensions", "0x0001"}, {"mls_proposals", "0x0001"}, {"app_components", "0x8009"}}}
	if err := MarmotShape(e); err != nil {
		t.Fatal(err)
	}
	e.Tags[3][1] = "0x0000"
	if err := MarmotShape(e); err != nil {
		t.Fatal(err)
	}
	e.Tags[6] = []string{"app_components", "0x8008"}
	if MarmotShape(e) == nil {
		t.Fatal("missing identity proof accepted")
	}
}
