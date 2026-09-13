package daemon

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCustomViewTransformCountsSuccessfulBlocks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		artifacts []map[string]any
		status    string
		stored    int
		failures  int
		retries   int
	}{
		{"complete", []map[string]any{svgArtifact(0), svgArtifact(1)}, "ok", 2, 0, 0},
		{"partial", []map[string]any{svgArtifact(0)}, "partial", 1, 0, 0},
		{"duplicate", []map[string]any{svgArtifact(0), svgArtifact(0)}, "partial", 1, 0, 0},
		{"invalid", []map[string]any{{"block": 0, "type": "image/png", "body": "invalid"}}, "no valid artifacts", 0, 1, 1},
		{"empty", nil, "no valid artifacts", 0, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant, server, _ := viewTenant(t, []int{1}, nil)
			server.answer(http.StatusOK, tc.artifacts...)
			note := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\nA\n```\n\n```mermaid\nB\n```")
			if err := publishAs(t, tenant, note); err != nil {
				t.Fatal(err)
			}
			runPendingView(t, tenant, "diagrams", note.ID)
			var status string
			var failures, stored int
			if err := tenant.store.DB().QueryRowContext(context.Background(), `SELECT last_status,failures FROM custom_views WHERE name='diagrams'`).Scan(&status, &failures); err != nil {
				t.Fatal(err)
			}
			if err := tenant.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM custom_view_artifacts`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			retries := len(pendingViewIntents(t, tenant, "diagrams", note.ID))
			if status != tc.status || stored != tc.stored || failures != tc.failures || retries != tc.retries {
				t.Fatalf("status=%q stored=%d failures=%d retries=%d; want %q %d %d %d", status, stored, failures, retries, tc.status, tc.stored, tc.failures, tc.retries)
			}
		})
	}
}

func TestCustomViewInvalidArtifactsRetryThenPause(t *testing.T) {
	tenant, server, _ := viewTenant(t, []int{1}, nil)
	server.answer(http.StatusOK, map[string]any{"block": 0, "type": "image/svg+xml", "body": "<svg><script/></svg>"})
	note := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\nA\n```")
	if err := publishAs(t, tenant, note); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runPendingView(t, tenant, "diagrams", note.ID)
	}
	if got := pendingViewIntents(t, tenant, "diagrams", note.ID); len(got) != 0 {
		t.Fatalf("invalid artifacts exceeded retry limit: %+v", got)
	}
	ctx := context.Background()
	if _, err := tenant.store.DB().ExecContext(ctx, `UPDATE custom_views SET failures=19 WHERE name='diagrams'`); err != nil {
		t.Fatal(err)
	}
	other := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\nB\n```")
	if err := publishAs(t, tenant, other); err != nil {
		t.Fatal(err)
	}
	runPendingView(t, tenant, "diagrams", other.ID)
	var failures, enabled int
	var status string
	if err := tenant.store.DB().QueryRowContext(ctx, `SELECT failures,enabled,last_status FROM custom_views WHERE name='diagrams'`).Scan(&failures, &enabled, &status); err != nil {
		t.Fatal(err)
	}
	if failures != 20 || enabled != 0 || status != "paused after 20 failures: no valid artifacts" {
		t.Fatalf("failures=%d enabled=%d status=%q", failures, enabled, status)
	}
}

func TestCustomViewSVGReferenceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"XML declaration", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`, true},
		{"literal text", `<svg><text>@import url(example)</text></svg>`, true},
		{"CSS comment", `<svg><style>/* colors */ .x{fill:url(#paint)}</style></svg>`, true},
		{"style chunks", `<svg><style>.x{fill:u<![CDATA[rl]]>(https://example.com/x)}</style></svg>`, false},
		{"nested style", `<svg><style>.x{fill:url(https://example.com/x)}<style/></style></svg>`, false},
		{"XML base", `<svg xml:base="https://example.com/"><use href="#secret"/></svg>`, false},
		{"attribute import", `<svg style="@import 'https://example.com/a.css'"/>`, false},
		{"other namespace", `<svg xmlns="http://www.w3.org/1999/xhtml"/>`, false},
		{"stylesheet instruction", `<?xml-stylesheet href="https://example.com/a.css"?><svg/>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkSVG(tc.body); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestCustomViewRejectsOversizedPNGDimensions(t *testing.T) {
	image := []byte(testPNGRaw)
	binary.BigEndian.PutUint32(image[16:20], 8192)
	binary.BigEndian.PutUint32(image[20:24], 8192)
	binary.BigEndian.PutUint32(image[29:33], crc32.ChecksumIEEE(image[12:29]))
	_, err := checkArtifact(viewArtifact{Type: "image/png", Body: base64.StdEncoding.EncodeToString(image)}, 1024)
	if err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("oversized PNG error=%v", err)
	}
}
