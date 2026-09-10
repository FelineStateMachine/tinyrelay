package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/mcp"
)

func TestMCPAttachmentRegistryRoundTrip(t *testing.T) {
	_, tenant := testTenant(t)
	registry, err := tenant.mcpTools()
	if err != nil {
		t.Fatal(err)
	}
	upload, ok := registry.Lookup("upload_attachment")
	if !ok {
		t.Fatal("upload_attachment is not registered")
	}
	owner := tenant.Policy().Owner
	raw := []byte("agent document")
	result, err := upload.Handler(context.Background(), mcp.Call{Actor: owner, Arguments: map[string]any{"room": "main", "data": base64.StdEncoding.EncodeToString(raw), "type": "text/plain", "filename": "note.txt"}})
	if err != nil || result.IsError {
		t.Fatalf("upload: %v %+v", err, result)
	}
	descriptor := result.StructuredContent.(map[string]any)
	encodedDescriptor, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encodedDescriptor, &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor["sha256"] == "" || descriptor["url"] == "" || descriptor["filename"] != "note.txt" {
		t.Fatalf("descriptor: %#v", descriptor)
	}
	post, ok := registry.Lookup("post_message")
	if !ok {
		t.Fatal("post_message is not registered")
	}
	if err := mcp.Validate(post.InputSchema, map[string]any{"room": "main", "attachments": []any{descriptor}}); err != nil {
		t.Fatalf("descriptor schema: %v", err)
	}
	unsigned, err := mcpBuildMessage(mcp.Call{Actor: owner, Arguments: map[string]any{"room": "main", "attachments": []any{descriptor}}})
	if err != nil || !strings.Contains(unsigned.Content, descriptor["url"].(string)) || len(unsigned.Tags) != 2 {
		t.Fatalf("attachment-only message: %+v %v", unsigned, err)
	}
	if thread, err := mcpBuildThread(mcp.Call{Arguments: map[string]any{"room": "main", "attachments": []any{descriptor}}}); err != nil || thread.Content == "" {
		t.Fatalf("attachment-only thread: %+v %v", thread, err)
	}
	if reply, err := mcpBuildReply(mcp.Call{Arguments: map[string]any{"room": "main", "root": strings.Repeat("a", 64), "attachments": []any{descriptor}}}); err != nil || reply.Content == "" {
		t.Fatalf("attachment-only reply: %+v %v", reply, err)
	}
	read, ok := registry.Lookup("read_attachment")
	if !ok {
		t.Fatal("read_attachment is not registered")
	}
	readResult, err := read.Handler(context.Background(), mcp.Call{Actor: owner, Arguments: map[string]any{"sha256": descriptor["sha256"], "max_bytes": float64(1024)}})
	if err != nil || readResult.IsError {
		t.Fatalf("read: %v %+v", err, readResult)
	}
	readData := readResult.StructuredContent.(map[string]any)["data"].(string)
	if got, _ := base64.StdEncoding.DecodeString(readData); string(got) != string(raw) {
		t.Fatalf("read data %q", got)
	}
	for _, media := range []struct {
		typ, want string
		body      []byte
	}{{"image/png", "image", []byte("\x89PNG\r\n\x1a\n")}, {"audio/ogg", "audio", []byte("OggS test")}} {
		mediaResult, err := upload.Handler(context.Background(), mcp.Call{Actor: owner, Arguments: map[string]any{"room": "main", "data": base64.StdEncoding.EncodeToString(media.body), "type": media.typ}})
		if err != nil || mediaResult.IsError {
			t.Fatalf("media upload %s: %v %+v", media.typ, err, mediaResult)
		}
		mediaDescriptor := mediaResult.StructuredContent.(map[string]any)
		got, err := read.Handler(context.Background(), mcp.Call{Actor: owner, Arguments: map[string]any{"sha256": mediaDescriptor["sha256"], "max_bytes": float64(1024)}})
		if err != nil || got.IsError || len(got.Content) != 1 || got.Content[0].Type != media.want {
			t.Fatalf("media read %s: %v %+v", media.typ, err, got)
		}
	}
	unsignedEvent, err := mcpBuildMessage(mcp.Call{Actor: owner, Arguments: map[string]any{"room": "main", "attachments": []any{descriptor}}})
	if err != nil {
		t.Fatal(err)
	}
	published := event.Event{Kind: unsignedEvent.Kind, CreatedAt: unsignedEvent.CreatedAt, Tags: unsignedEvent.Tags, Content: unsignedEvent.Content}
	if err := event.Sign(&published, strings.Repeat("0", 63)+"1"); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(published)
	var signed map[string]any
	_ = json.Unmarshal(encoded, &signed)
	posted, err := post.Handler(context.Background(), mcp.Call{Actor: owner, Arguments: map[string]any{"event": signed}})
	if err != nil || posted.IsError {
		t.Fatalf("signed attachment post: %v %+v", err, posted)
	}
	roomResult, err := tenant.Execute(context.Background(), owner, "browseroom", []json.RawMessage{json.RawMessage(`{"id":"main"}`)})
	if err != nil || !strings.Contains(string(mcpJSON(roomResult)), "imeta") {
		t.Fatalf("room lost attachment metadata: %v %#v", err, roomResult)
	}
}

func mcpJSON(value any) []byte { raw, _ := json.Marshal(value); return raw }

func TestMCPAttachmentRejectsUnauthorizedAndMalformed(t *testing.T) {
	_, tenant := testTenant(t)
	registry, err := tenant.mcpTools()
	if err != nil {
		t.Fatal(err)
	}
	upload, _ := registry.Lookup("upload_attachment")
	if result, err := upload.Handler(context.Background(), mcp.Call{Actor: strings.Repeat("f", 64), Arguments: map[string]any{"room": "main", "data": "YQ==", "type": "text/plain"}}); err != nil || !result.IsError {
		t.Fatalf("unjoined upload accepted: %v %+v", err, result)
	}
	if err := mcpCheckAttachments(event.Event{Tags: [][]string{{"imeta", "url https://relay.test/media/not-a-hash", "m text/plain", "x bad"}}}); err == nil {
		t.Fatal("malformed imeta accepted")
	}
	h := newRoomHarness(t)
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "private"}, {"closed"}}, "")
	stored, err := h.tenant.storeRoomAttachment(h.ctx, h.keys["alice"], "private", roomAttachmentInput{Body: []byte("secret"), Type: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.tenant.mcpReadAttachment(h.ctx, mcp.Call{Actor: h.keys["bob"], Arguments: map[string]any{"sha256": stored["sha256"]}})
	if err != nil || !result.IsError {
		t.Fatalf("outsider read accepted: %v %+v", err, result)
	}
}

func TestMCPAttachmentEscapingAndValidation(t *testing.T) {
	descriptor := map[string]any{"url": "https://relay.test/media/" + strings.Repeat("a", 64), "sha256": strings.Repeat("a", 64), "size": float64(8), "type": "text/plain", "filename": "report] [x].txt"}
	call := mcp.Call{Arguments: map[string]any{"room": "main", "attachments": []any{descriptor}}}
	unsigned, err := mcpBuildMessage(call)
	if err != nil || !strings.HasPrefix(unsigned.Content, `[report\] \[x\].txt](`) {
		t.Fatalf("filename escaping: %q %v", unsigned.Content, err)
	}
	for _, invalid := range []string{"javascript:alert(1)", "https://user:password@relay.test/a", "https://relay.test/a)\n![bad](https://tracker.test)", "https://relay.test/a b"} {
		descriptor["url"] = invalid
		if _, err := mcpBuildMessage(call); err == nil {
			t.Errorf("unsafe URL accepted: %q", invalid)
		}
	}
	descriptor["url"] = "https://relay.test/video.mp4"
	descriptor["type"] = "video/mp4"
	if unsigned, err := mcpBuildMessage(call); err != nil || !strings.HasPrefix(unsigned.Content, "![video](") {
		t.Fatalf("Buzz video marker: %+v %v", unsigned, err)
	}
	for _, invalid := range []string{"invalid", "text/plain\r\nx bad", ""} {
		descriptor["type"] = invalid
		if _, err := mcpBuildMessage(call); err == nil {
			t.Errorf("unsafe MIME accepted: %q", invalid)
		}
	}
}
