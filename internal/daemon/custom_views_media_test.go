package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/views"
)

func TestCustomViewMediaPathServesStoredType(t *testing.T) {
	for _, mediaType := range []string{"image/svg+xml", "image/png"} {
		t.Run(mediaType, func(t *testing.T) {
			tenant, server, _ := viewTenant(t, []int{1}, nil)
			artifact := svgArtifact(0)
			artifact["type"] = mediaType
			if mediaType == "image/png" {
				artifact["body"] = "iVBORw0KGgoAAAANSUhEUg=="
			}
			server.answer(http.StatusOK, artifact)
			e := signedEvent(t, testOwnerSecret, 1, time.Now().Unix(), nil, "```mermaid\ngraph TD; a-->b;\n```")
			if err := publishAs(t, tenant, e); err != nil {
				t.Fatal(err)
			}
			runPendingView(t, tenant, "diagrams", e.ID)
			_, image, ok := strings.Cut(views.Figure("diagrams", "mermaid", "graph TD; a-->b;"), `<img src="`)
			if !ok {
				t.Fatal("custom view has no image")
			}
			path, _, _ := strings.Cut(image, `"`)
			res := getArtifact(t, tenant, path, "")
			if res.Code != http.StatusOK || res.Header().Get("Content-Type") != mediaType {
				t.Fatalf("media response: %d %s", res.Code, res.Body.String())
			}
			request := httptest.NewRequest(http.MethodHead, "http://127.0.0.1:8080"+path, nil)
			head := httptest.NewRecorder()
			tenant.ServeHTTP(head, request)
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("ETag") != res.Header().Get("ETag") {
				t.Fatalf("media HEAD: %d %s", head.Code, head.Body.String())
			}
		})
	}
}
