package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestAuthorizationHookAllowsDeniesAndUpdatesSession(t *testing.T) {
	app := newTestServer(t)
	session := authSession{AccessToken: "old", User: "old-user"}
	token := (&oauth2.Token{
		AccessToken:  "hook-token",
		TokenType:    "Bearer",
		RefreshToken: "refresh",
		Expiry:       time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	}).WithExtra(map[string]any{"id_token": "id.jwt"})

	allowed, err := app.authorizationHook(context.Background(), token, &session)
	if err != nil || !allowed || session.User != "old-user" {
		t.Fatalf("empty hook allowed=%v updated=%#v err=%v", allowed, session, err)
	}

	var payload struct {
		Token   map[string]any `json:"token"`
		Session authSession    `json:"session"`
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("bad hook request method=%s content-type=%s", r.Method, r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":false,"user":"new-user","email":"new@example.test","groups":["ops","dev"]}`))
	}))
	defer hook.Close()

	app.cfg.AuthorizationHookURL = hook.URL
	allowed, err = app.authorizationHook(context.Background(), token, &session)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("hook should deny access")
	}
	if payload.Token["access_token"] != "hook-token" || payload.Token["id_token"] != "id.jwt" || payload.Session.User != "old-user" {
		t.Fatalf("unexpected hook payload: %#v", payload)
	}
	if session.User != "new-user" || session.Email != "new@example.test" || !reflect.DeepEqual(session.Groups, []string{"ops", "dev"}) {
		t.Fatalf("updated session = %#v", session)
	}
}

func TestAuthorizationHookErrors(t *testing.T) {
	app := newTestServer(t)
	token := &oauth2.Token{AccessToken: "hook-token", Expiry: time.Now().Add(time.Hour)}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer fail.Close()

	app.cfg.AuthorizationHookURL = fail.URL
	if _, err := app.authorizationHook(context.Background(), token, &authSession{}); err == nil {
		t.Fatal("expected non-2xx hook to fail")
	}
	app.cfg.AuthorizationHookURL = "://bad-url"
	if _, err := app.authorizationHook(context.Background(), token, &authSession{}); err == nil {
		t.Fatal("expected invalid hook URL to fail")
	}
}

func TestResourcePrerenderHookUpdatesLinksAndResource(t *testing.T) {
	app := newTestServer(t)
	object := kube.NewObject(&kube.ResourceType{Plural: "pods", Kind: "Pod", Namespaced: true}, &unstructured.Unstructured{
		Object: map[string]any{
			"kind": "Pod",
			"metadata": map[string]any{
				"name":      "nginx",
				"namespace": "default",
			},
		},
	})
	links := []config.Link{{Href: "/base", Title: "Base"}}
	gotLinks, replacement, err := app.resourcePrerenderHook(context.Background(), "test", "default", "pods", &object, links)
	if err != nil || replacement != nil || !reflect.DeepEqual(gotLinks, links) {
		t.Fatalf("empty prerender hook links=%#v replacement=%#v err=%v", gotLinks, replacement, err)
	}

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Cluster   string        `json:"cluster"`
			Namespace string        `json:"namespace"`
			Plural    string        `json:"plural"`
			Links     []config.Link `json:"links"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Cluster != "test" || payload.Namespace != "default" || payload.Plural != "pods" || len(payload.Links) != 1 {
			t.Fatalf("unexpected prerender payload: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"links":[{"href":"/extra","title":"Extra","icon":"external-link"}],"resource":{"kind":"Pod","metadata":{"name":"rewritten"}}}`))
	}))
	defer hook.Close()
	app.cfg.ResourcePrerenderHookURL = hook.URL
	gotLinks, replacement, err = app.resourcePrerenderHook(context.Background(), "test", "default", "pods", &object, links)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotLinks) != 2 || gotLinks[1].Href != "/extra" || replacement["kind"] != "Pod" {
		t.Fatalf("prerender result links=%#v replacement=%#v", gotLinks, replacement)
	}
}

func TestPostJSONErrorBranches(t *testing.T) {
	if err := postJSON(context.Background(), "://bad-url", map[string]string{"x": "y"}, nil); err == nil {
		t.Fatal("expected bad URL to fail")
	}
	if err := postJSON(context.Background(), "http://example.invalid", func() {}, nil); err == nil {
		t.Fatal("expected marshal error to fail")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer empty.Close()
	if err := postJSON(context.Background(), empty.URL, map[string]string{"x": "y"}, nil); err != nil {
		t.Fatalf("empty response should succeed: %v", err)
	}

	invalidJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer invalidJSON.Close()
	var out map[string]any
	if err := postJSON(context.Background(), invalidJSON.URL, map[string]string{"x": "y"}, &out); err == nil {
		t.Fatal("expected invalid JSON response to fail")
	}
}

func TestLoadPartialsFallbackAndPrecedence(t *testing.T) {
	if got := loadPartials(""); len(got) != 0 {
		t.Fatalf("empty root partials = %#v", got)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "partials"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extrahead.html"), []byte("root head"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "footer.html"), []byte("root footer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "partials", "extrahead.html"), []byte("partial head"), 0o644); err != nil {
		t.Fatal(err)
	}
	partials := loadPartials(root)
	if partials["partials/extrahead.html"] != "partial head" {
		t.Fatalf("partials extrahead precedence = %#v", partials)
	}
	if partials["partials/footer.html"] != "root footer" {
		t.Fatalf("footer fallback = %#v", partials)
	}
}
