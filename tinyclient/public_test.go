package tinyclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FelineStateMachine/tinyrelay/tinyclient"
)

type exampleBackend struct{}

func (exampleBackend) Query(context.Context, string, []json.RawMessage, string) (any, error) {
	return nil, nil
}
func (exampleBackend) Policy() tinyclient.Policy {
	return tinyclient.DefaultPolicy(strings.Repeat("a", 64))
}
func (exampleBackend) URL() string      { return "http://relay.example" }
func (exampleBackend) Slug() string     { return "standalone example" }
func (exampleBackend) Identity() string { return strings.Repeat("a", 64) }

func TestPublicBackendRendersWithoutInternalImports(t *testing.T) {
	app, err := tinyclient.New(exampleBackend{}, tinyclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "standalone example") || !strings.Contains(response.Body.String(), "<html") {
		t.Fatalf("render: %d %s", response.Code, response.Body.String())
	}
}
