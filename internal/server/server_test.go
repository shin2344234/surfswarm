package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestUIPasswordGuardsPagesAndAPI(t *testing.T) {
	ts := newServer(t, Config{UIPassword: "secret"})
	client := ts.Client()

	res, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no credentials: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/lists", nil)
	req.SetBasicAuth("anyone", "wrong")
	if res, _ := client.Do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", res.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/lists", nil)
	req.SetBasicAuth("anyone", "secret")
	res, _ = client.Do(req)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("right password: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "surfswarm_ui" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("session cookie not set")
	}

	// The cookie alone is enough afterwards, which is what the websocket
	// handshake relies on.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/agents", nil)
	req.AddCookie(cookie)
	if res, _ := client.Do(req); res.StatusCode != http.StatusOK {
		t.Fatalf("cookie: %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: "surfswarm_ui", Value: "forged"})
	if res, _ := client.Do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie: %d", res.StatusCode)
	}

	// The agent endpoint is not behind the UI password.
	if res, _ := client.Get(ts.URL + "/agent"); res.StatusCode == http.StatusUnauthorized {
		t.Fatal("agent endpoint must not require the UI password")
	}
}

func TestNoPasswordLeavesUIOpen(t *testing.T) {
	ts := newServer(t, Config{})
	if res, _ := ts.Client().Get(ts.URL + "/api/lists"); res.StatusCode != http.StatusOK {
		t.Fatalf("open: %d", res.StatusCode)
	}
}

func TestAgentEndpointRequiresToken(t *testing.T) {
	ts := newServer(t, Config{Token: "tok"})
	if res, _ := ts.Client().Get(ts.URL + "/agent"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", res.StatusCode)
	}
}

func TestListAPIValidation(t *testing.T) {
	ts := newServer(t, Config{})
	client := ts.Client()

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/lists/custom", strings.NewReader(`{"text":"https://ok.example/\nbad line\n"}`))
	req.Header.Set("Content-Type", "application/json")
	res, _ := client.Do(req)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid list: %d", res.StatusCode)
	}
	var body struct {
		Problems []string `json:"problems"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if len(body.Problems) != 1 || !strings.Contains(body.Problems[0], "line 2") {
		t.Fatalf("problems: %v", body.Problems)
	}

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/lists/custom", strings.NewReader(`{"text":"https://ok.example/\n"}`))
	req.Header.Set("Content-Type", "application/json")
	if res, _ := client.Do(req); res.StatusCode != http.StatusOK {
		t.Fatalf("valid list: %d", res.StatusCode)
	}
	res, _ = client.Get(ts.URL + "/api/lists")
	var lists []ListInfo
	_ = json.NewDecoder(res.Body).Decode(&lists)
	found := false
	for _, li := range lists {
		if li.Name == "custom" && li.Count == 1 && !li.Builtin {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom list missing from %+v", lists)
	}
}

func TestCreateTestWithoutAgents(t *testing.T) {
	ts := newServer(t, Config{})
	res, err := ts.Client().Post(ts.URL+"/api/tests", "application/json", strings.NewReader(`{"threads":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 with no agents, got %d", res.StatusCode)
	}
	res, _ = ts.Client().Post(ts.URL+"/api/tests", "application/json", strings.NewReader(`{"url_list":"nope"}`))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown list: %d", res.StatusCode)
	}
}
