package gitrelay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrivateProxySignsAndStreamsGitRequest(t *testing.T) {
	var authHeader string
	var body string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_, _ = w.Write([]byte("pack-response"))
	}))
	t.Cleanup(remote.Close)
	proxy, err := newPrivateProxy(context.Background(), remote.URL+"/r/owner/repo.git", func(_ context.Context, method, target, payload string) (string, error) {
		if method != http.MethodGet || target != remote.URL+"/r/owner/repo.git" || payload != "" {
			return "", &testError{"unexpected signed request"}
		}
		return "Nostr signed:" + payload, nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	req, err := http.NewRequest(http.MethodPost, proxy.URL()+"/info/refs", strings.NewReader("git-request"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", res.StatusCode)
	}
	if got, _ := io.ReadAll(res.Body); string(got) != "pack-response" {
		t.Fatalf("response=%q", got)
	}
	if body != "git-request" || !strings.HasPrefix(authHeader, "Nostr signed:") {
		t.Fatalf("forwarded body/auth=%q/%q", body, authHeader)
	}
}

func TestProbePrivatePeerRequiresGRASP08(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "private", body: `{"supported_grasps":["GRASP-08"]}`, want: true},
		{name: "public", body: `{"supported_grasps":["GRASP-01"]}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/nostr+json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			err := probePrivatePeer(context.Background(), server.URL, true)
			if (err == nil) != tc.want {
				t.Fatalf("probe error=%v", err)
			}
		})
	}
}

type testError struct{ text string }

func (e *testError) Error() string { return e.text }
