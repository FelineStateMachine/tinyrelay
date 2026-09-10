package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/blob"
	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

func TestRoomAttachmentAccessFollowsMembership(t *testing.T) {
	h := newRoomHarness(t)
	ctx := context.Background()
	h.must("alice", event.KIND_CREATE_GROUP, [][]string{{"h", "secret"}, {"closed"}}, "")
	entry, err := h.tenant.blobs.Put(ctx, blob.PutOptions{Reader: strings.NewReader("room secret"), Type: "text/plain", Uploader: h.keys["alice"]})
	if err != nil {
		t.Fatal(err)
	}
	room, err := h.tenant.community.Room(ctx, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tenant.store.DB().ExecContext(ctx, "INSERT INTO room_attachments(room_id,room_event_id,sha256,filename,created_at) VALUES(?,?,?,?,unixepoch())", "secret", room.EventID, entry.SHA256, "secret.txt"); err != nil {
		t.Fatal(err)
	}
	if err := h.tenant.roomAttachmentAccess(ctx, h.keys["stranger"], entry.SHA256); err == nil {
		t.Fatal("nonmember can read room attachment")
	}
	if err := h.tenant.roomAttachmentAccess(ctx, h.keys["owner"], entry.SHA256); err != nil {
		t.Fatalf("member cannot read room attachment: %v", err)
	}
}

func TestGenericBlobRemainsUnscoped(t *testing.T) {
	h := newRoomHarness(t)
	entry, err := h.tenant.blobs.Put(context.Background(), blob.PutOptions{Reader: strings.NewReader("public"), Type: "text/plain", Uploader: h.keys["alice"]})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.tenant.roomAttachmentAccess(context.Background(), "", entry.SHA256); err != nil {
		t.Fatalf("generic blob unexpectedly scoped: %v", err)
	}
}

func TestRoomAttachmentAgentGrantAndStorageLimits(t *testing.T) {
	_, tenant := testTenant(t)
	ctx := context.Background()
	owner := tenant.Policy().Owner
	agent, _ := event.PublicKey(testAgentSecret)
	now := time.Now().Unix()
	if err := publishAs(t, tenant, agentGrantEvent(t, agent, now, now+3600, []string{"k", "9"}, []string{"h", "main"})); err != nil {
		t.Fatal(err)
	}
	input := roomAttachmentInput{Body: []byte("generated attachment"), Type: "text/plain"}
	if _, err := tenant.storeRoomAttachment(ctx, agent, "main", input); err != nil {
		t.Fatalf("active agent upload: %v", err)
	}
	if _, err := tenant.Execute(ctx, owner, "pauseagent", []json.RawMessage{rawJSON(agent)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.storeRoomAttachment(ctx, agent, "main", input); err == nil {
		t.Fatal("paused agent uploaded")
	}
	if _, err := tenant.Execute(ctx, owner, "revokeagent", []json.RawMessage{rawJSON(agent)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.storeRoomAttachment(ctx, agent, "main", input); err == nil {
		t.Fatal("revoked agent uploaded")
	}
	if _, err := tenant.Execute(ctx, owner, "setpolicy", []json.RawMessage{rawJSON(map[string]any{"fileLimits": map[string]any{"maxFileBytes": 4}})}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.storeRoomAttachment(ctx, owner, "main", input); err == nil {
		t.Fatal("room upload bypassed file limit")
	}
}

func TestRoomAttachmentCannotPrivatizeExistingUnscopedBlob(t *testing.T) {
	h := newRoomHarness(t)
	entry, err := h.tenant.blobs.Put(h.ctx, blob.PutOptions{Reader: strings.NewReader("shared bytes"), Type: "text/plain", Uploader: h.keys["alice"]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tenant.storeRoomAttachment(h.ctx, h.keys["bob"], "main", roomAttachmentInput{Body: []byte("shared bytes"), Type: "text/plain"}); err == nil || !strings.HasPrefix(err.Error(), "conflict:") {
		t.Fatalf("existing public blob should conflict: %v", err)
	}
	scopes, err := h.tenant.roomAttachmentRooms(h.ctx, entry.SHA256)
	if err != nil || len(scopes) != 0 {
		t.Fatalf("conflicted upload changed visibility: %v %v", scopes, err)
	}
	var claims int
	if err := h.tenant.store.DB().QueryRowContext(h.ctx, "SELECT COUNT(*) FROM blob_claims WHERE sha256=? AND uploader=?", entry.SHA256, h.keys["bob"]).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("conflicted upload left a claim: %d %v", claims, err)
	}
}
