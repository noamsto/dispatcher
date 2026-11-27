package stub

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/messages", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "not-a-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestScriptedStatusesThenRepeatLast(t *testing.T) {
	s, err := Start(DefaultScript())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	r, body := post(t, s.URL)
	if r.StatusCode != 429 || r.Header.Get("retry-after") != "1" ||
		r.Header.Get("anthropic-ratelimit-unified-status") != "rejected" || !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("first: %d %v %s", r.StatusCode, r.Header, body)
	}
	if r, body = post(t, s.URL); r.StatusCode != 529 || !strings.Contains(body, "overloaded_error") {
		t.Fatalf("second: %d %s", r.StatusCode, body)
	}
	for range 2 {
		if r, body = post(t, s.URL); r.StatusCode != 400 || !strings.Contains(body, "credit") {
			t.Fatalf("third+: %d %s", r.StatusCode, body)
		}
	}
	if s.Hits() != 4 {
		t.Fatalf("hits = %d", s.Hits())
	}
}

func TestEmptyScriptIs500(t *testing.T) {
	s, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if r, _ := post(t, s.URL); r.StatusCode != 500 {
		t.Fatalf("status = %d", r.StatusCode)
	}
}
