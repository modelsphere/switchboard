package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelsphere/switchboard/internal/gateway"
)

type fakeSource struct {
	c     *gateway.Catalog
	ready bool
}

func (f fakeSource) Catalog() *gateway.Catalog { return f.c }
func (f fakeSource) Ready() bool               { return f.ready }

var created = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func server(ready bool) http.Handler {
	alice := true
	c := &gateway.Catalog{
		Routes: []gateway.Route{
			{Name: "llm/a", Model: "Qwen/Qwen3-8B", Created: created},
			{Name: "llm/b", Model: "secret-model", Created: created},
		},
		KeyAuth: gateway.KeyAuth{
			GlobalAuth: &alice,
			Global:     &gateway.ConsumerConfig{Consumers: []string{"alice", "bob"}},
			Rules: []gateway.AuthRule{
				{Routes: map[string]struct{}{"llm/b": {}}, Allow: map[string]struct{}{"bob": {}}},
			},
		},
	}
	return (&Server{Source: fakeSource{c, ready}, OwnedBy: "modelsphere"}).Handler()
}

func do(h http.Handler, path, consumer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "api.example.com"
	if consumer != "" {
		req.Header.Set(ConsumerHeader, consumer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestListFilteredByConsumer(t *testing.T) {
	h := server(true)
	for consumer, want := range map[string][]string{
		"alice": {"Qwen/Qwen3-8B"},
		"bob":   {"Qwen/Qwen3-8B", "secret-model"},
		"eve":   {},
	} {
		rec := do(h, "/v1/models", consumer)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", consumer, rec.Code, rec.Body)
		}
		var got list
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Object != "list" || got.Data == nil || len(got.Data) != len(want) {
			t.Fatalf("%s: %s", consumer, rec.Body)
		}
		for i, m := range got.Data {
			if m.ID != want[i] || m.Object != "model" || m.OwnedBy != "modelsphere" || m.Created != created.Unix() {
				t.Errorf("%s: %+v", consumer, m)
			}
		}
	}
}

func TestEmptyListIsArray(t *testing.T) {
	rec := do(server(true), "/v1/models", "eve")
	if got := rec.Body.String(); got != "{\"object\":\"list\",\"data\":[]}\n" {
		t.Errorf("body %q", got)
	}
}

func TestUnauthenticated(t *testing.T) {
	h := server(true)
	for _, p := range []string{"/v1/models", "/v1/models/Qwen/Qwen3-8B"} {
		rec := do(h, p, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, rec.Code)
		}
		var body map[string]apiError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"].Code != "invalid_api_key" {
			t.Errorf("%s: %s", p, rec.Body)
		}
	}
}

func TestGetModel(t *testing.T) {
	h := server(true)
	rec := do(h, "/v1/models/Qwen/Qwen3-8B", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var m model
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.ID != "Qwen/Qwen3-8B" {
		t.Errorf("%+v", m)
	}

	notAllowed := do(h, "/v1/models/secret-model", "alice")
	missing := do(h, "/v1/models/no-such-model", "alice")
	for _, rec := range []*httptest.ResponseRecorder{notAllowed, missing} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	}
	// No probing: not-allowed and missing look the same apart from the id.
	var a, b map[string]apiError
	_ = json.Unmarshal(notAllowed.Body.Bytes(), &a)
	_ = json.Unmarshal(missing.Body.Bytes(), &b)
	if a["error"].Code != "model_not_found" || a["error"].Code != b["error"].Code || a["error"].Type != b["error"].Type {
		t.Errorf("%v vs %v", a, b)
	}
}

func TestNotReady(t *testing.T) {
	h := server(false)
	if rec := do(h, "/v1/models", "alice"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list: %d", rec.Code)
	}
	if rec := do(h, "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz: %d", rec.Code)
	}
	if rec := do(server(true), "/readyz", ""); rec.Code != http.StatusOK {
		t.Errorf("readyz ready: %d", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set(ConsumerHeader, "alice")
	rec := httptest.NewRecorder()
	server(true).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("%d", rec.Code)
	}
}
