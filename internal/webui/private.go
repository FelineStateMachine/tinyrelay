package webui

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

// PrivateReader is implemented by the daemon so web pages use the same
// membership check as websocket and HTTP data paths. It is optional to keep
// webui usable with small embedders; those embedders get owner-only fallback.
type PrivateReader interface {
	ReadAllowed(context.Context, string) error
}

func privatePolicyEnabled(p policy.Policy) bool { return p.Features.Grasp08 }

func privateShellPath(path string) bool {
	if path == "/" || path == "/signin" || path == "/manage/connect" {
		return true
	}
	if path == "/manage/rpc" || path == "/manage/jobs/status" {
		return false
	}
	return strings.HasPrefix(path, "/manage/") && !strings.HasSuffix(path, ".json")
}

func privateProtectedPath(path string) bool {
	if privateShellPath(path) || path == "/webmcp.js" || path == "/signer.js" || path == "/fixi.js" || path == "/qr.svg" {
		return false
	}
	return true
}

func (a *App) privateReadAllowed(request *http.Request, actor string) error {
	if actor == "" {
		return errors.New("auth-required: private relay membership required")
	}
	if reader, ok := a.backend.(PrivateReader); ok {
		return reader.ReadAllowed(request.Context(), actor)
	}
	if actor != a.backend.Policy().Owner {
		return errors.New("restricted: private relay membership required")
	}
	return nil
}

func (a *App) denyPrivateEndpoint(writer http.ResponseWriter, request *http.Request) bool {
	if !privatePolicyEnabled(a.backend.Policy()) || !privateProtectedPath(request.URL.Path) {
		return false
	}
	actor, err := a.resolveActor(request)
	if err != nil || actor == "" {
		writer.Header().Set("Cache-Control", "private, no-store")
		http.Error(writer, "authentication required", http.StatusUnauthorized)
		return true
	}
	if err := a.privateReadAllowed(request, actor); err != nil {
		writer.Header().Set("Cache-Control", "private, no-store")
		http.Error(writer, err.Error(), http.StatusForbidden)
		return true
	}
	return false
}

func (a *App) privatePageData(data *PageData, request *http.Request, actor string) {
	if !privatePolicyEnabled(data.Policy) || !privateShellPath(request.URL.Path) {
		return
	}
	if err := a.privateReadAllowed(request, actor); err == nil {
		return
	}
	data.Private = true
	data.Title = "Private relay"
	data.Slug = "private relay"
	data.URL = ""
	data.Identity = ""
	data.Policy = policy.Policy{}
	data.Actor = ""
	data.Owner = false
	data.Event = nil
	data.Feed = nil
	data.Methods = nil
}
