package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"

	"github.com/bropines/flaresolverr-go/internal/config"
	"github.com/bropines/flaresolverr-go/internal/solver"
)

type fakeSolver struct {
	got      *solver.Request
	res      *solver.Result
	err      error
	sessions map[string]bool
	proxy    *solver.Proxy
}

func (f *fakeSolver) Solve(ctx context.Context, r *solver.Request) (*solver.Result, error) {
	f.got = r
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

func (f *fakeSolver) CreateSession(id string, p *solver.Proxy) (string, bool, error) {
	f.proxy = p
	if id == "" {
		id = "generated"
	}
	if f.sessions[id] {
		return id, false, nil
	}
	f.sessions[id] = true
	return id, true, nil
}

func (f *fakeSolver) DestroySession(id string) bool {
	ok := f.sessions[id]
	delete(f.sessions, id)
	return ok
}

func (f *fakeSolver) SessionIDs() []string {
	ids := []string{}
	for id := range f.sessions {
		ids = append(ids, id)
	}
	return ids
}

func (f *fakeSolver) UserAgent() string { return "UA/1.0" }

func newTest(cfg *config.Config) (*fakeSolver, http.Handler) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	f := &fakeSolver{sessions: map[string]bool{}, res: &solver.Result{
		Message:   "Challenge solved!",
		URL:       "https://example.com/",
		Status:    200,
		Headers:   map[string]string{"content-type": "text/html", "content-encoding": "br"},
		HTML:      "<html>ok</html>",
		UserAgent: "UA/1.0",
		Cookies: []*network.Cookie{
			{Name: "cf_clearance", Value: "abc", Domain: ".example.com", Path: "/", Expires: 1893456000.5, Size: 15, HTTPOnly: true, Secure: true, SameSite: network.CookieSameSiteNone},
			{Name: "sess", Value: "1", Domain: "example.com", Path: "/", Expires: -1, Session: true},
		},
	}}
	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), f, "1.2.3")
	s.now = func() time.Time { return time.UnixMilli(1700000000000) }
	return f, s.Handler()
}

func post(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1", strings.NewReader(body)))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	return rec.Code, m
}

func TestRequestGetResponseShape(t *testing.T) {
	f, h := newTest(nil)
	code, m := post(t, h, `{"cmd":"request.get","url":"https://example.com/","maxTimeout":60000}`)
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, k := range []string{"status", "message", "startTimestamp", "endTimestamp", "version", "solution"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
	if _, ok := m["session"]; ok {
		t.Error("request.get must not carry 'session'")
	}
	if m["status"] != "ok" || m["message"] != "Challenge solved!" || m["version"] != "1.2.3" {
		t.Fatalf("%v", m)
	}
	sol := m["solution"].(map[string]any)
	for _, k := range []string{"url", "status", "headers", "response", "cookies", "userAgent", "turnstile_token"} {
		if _, ok := sol[k]; !ok {
			t.Errorf("solution missing %q", k)
		}
	}
	if sol["status"].(float64) != 200 || sol["response"] != "<html>ok</html>" || sol["turnstile_token"] != nil {
		t.Fatalf("%v", sol)
	}
	hdr := sol["headers"].(map[string]any)
	if hdr["content-type"] != "text/html" || hdr["content-encoding"] != nil {
		t.Fatalf("headers %v", hdr)
	}
	cookies := sol["cookies"].([]any)
	c0 := cookies[0].(map[string]any)
	if c0["name"] != "cf_clearance" || c0["expiry"].(float64) != 1893456000 || c0["httpOnly"] != true || c0["sameSite"] != "None" || c0["session"] != false {
		t.Fatalf("cookie %v", c0)
	}
	c1 := cookies[1].(map[string]any)
	if _, ok := c1["expiry"]; ok || c1["session"] != true || c1["expires"].(float64) != -1 {
		t.Fatalf("session cookie %v", c1)
	}
	if f.got.Method != "GET" || f.got.URL != "https://example.com/" {
		t.Fatalf("solver got %+v", f.got)
	}
}

// FlareSolverrSharp (Jackett) decodes the response with these types; make
// sure our JSON fits them, including the numeric status into a string field.
func TestFlareSolverrSharpDecode(t *testing.T) {
	_, h := newTest(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1", strings.NewReader(`{"cmd":"request.get","url":"https://example.com/"}`)))
	var res struct {
		Status         string
		Message        string
		StartTimestamp int64
		EndTimestamp   int64
		Version        string
		Solution       struct {
			URL      string
			Status   json.Number
			Response string
			Cookies  []struct {
				Name, Value, Domain, Path string
				Expires                   float64
				Size                      int
				HttpOnly, Secure, Session bool
				SameSite                  string
			}
			UserAgent string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.Solution.UserAgent != "UA/1.0" || len(res.Solution.Cookies) != 2 || res.Solution.Cookies[0].Value != "abc" {
		t.Fatalf("%+v", res)
	}
}

func TestReturnOnlyCookies(t *testing.T) {
	_, h := newTest(nil)
	_, m := post(t, h, `{"cmd":"request.get","url":"https://example.com/","returnOnlyCookies":true}`)
	sol := m["solution"].(map[string]any)
	if _, ok := sol["response"]; ok {
		t.Error("response must be left out")
	}
	if _, ok := sol["headers"]; ok {
		t.Error("headers must be left out")
	}
	if len(sol["cookies"].([]any)) != 2 {
		t.Error("cookies missing")
	}
}

func TestRequestMapping(t *testing.T) {
	f, h := newTest(nil)
	post(t, h, `{"cmd":"request.post","url":"https://example.com/login","postData":"a=1&b=2",
		"maxTimeout":"30000","session":"s1","session_ttl_minutes":5,"waitInSeconds":2,"disableMedia":false,
		"returnScreenshot":true,"tabs_till_verify":3,
		"cookies":[{"name":"x","value":"y","domain":".example.com","expiry":1893456000},{"name":"z","value":"w"}],
		"proxy":{"url":"http://u:p@proxy:3128"}}`)
	g := f.got
	if g.Method != "POST" || g.PostData != "a=1&b=2" || g.Session != "s1" || g.SessionTTL != 5*time.Minute || g.WaitInSeconds != 2 || !g.ReturnScreenshot {
		t.Fatalf("%+v", g)
	}
	if g.BlockMedia == nil || *g.BlockMedia || g.TabsTillVerify == nil || *g.TabsTillVerify != 3 {
		t.Fatalf("flags %+v", g)
	}
	if len(g.Cookies) != 2 || g.Cookies[0].Expires != 1893456000 || g.Cookies[0].Domain != ".example.com" || g.Cookies[1].Name != "z" {
		t.Fatalf("cookies %+v", g.Cookies)
	}
	if g.Proxy == nil || g.Proxy.URL != "http://proxy:3128" || g.Proxy.Username != "u" || g.Proxy.Password != "p" {
		t.Fatalf("proxy %+v", g.Proxy)
	}
}

func TestEnvProxy(t *testing.T) {
	f, h := newTest(&config.Config{ProxyURL: "socks5://10.0.0.1:1080"})
	post(t, h, `{"cmd":"request.get","url":"https://example.com/"}`)
	if f.got.Proxy == nil || f.got.Proxy.URL != "socks5://10.0.0.1:1080" {
		t.Fatalf("env proxy not applied: %+v", f.got.Proxy)
	}
	post(t, h, `{"cmd":"request.get","url":"https://example.com/","proxy":{"url":"http://other:1"}}`)
	if f.got.Proxy.URL != "http://other:1" {
		t.Fatalf("request proxy must win: %+v", f.got.Proxy)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]string{
		`{}`:                    "Error: Request parameter 'cmd' is mandatory.",
		`{"cmd":"nope"}`:        "Error: Request parameter 'cmd' = 'nope' is invalid.",
		`{"cmd":"request.get"}`: "Error: Request parameter 'url' is mandatory in 'request.get' command.",
		`{"cmd":"request.get","url":"x","postData":"a=1"}`: "Error: Cannot use 'postBody' when sending a GET request.",
		`{"cmd":"request.post","url":"x"}`:                 "Error: Request parameter 'postData' is mandatory in 'request.post' command.",
		`{"cmd":"sessions.destroy","session":"x"}`:         "Error: The session doesn't exist.",
	}
	_, h := newTest(nil)
	for body, want := range cases {
		code, m := post(t, h, body)
		if code != 500 || m["status"] != "error" || m["message"] != want {
			t.Errorf("%s: %d %v", body, code, m)
		}
	}
}

func TestTimeoutMessage(t *testing.T) {
	f, h := newTest(nil)
	f.err = solver.ErrTimeout
	_, m := post(t, h, `{"cmd":"request.get","url":"https://example.com/","maxTimeout":45500}`)
	if m["message"] != "Error: Error solving the challenge. Timeout after 45.5 seconds." {
		t.Fatal(m["message"])
	}
	_, m = post(t, h, `{"cmd":"request.get","url":"https://example.com/"}`)
	if m["message"] != "Error: Error solving the challenge. Timeout after 60.0 seconds." {
		t.Fatal(m["message"])
	}
}

func TestSessions(t *testing.T) {
	f, h := newTest(nil)
	_, m := post(t, h, `{"cmd":"sessions.create","session":"a","proxy":{"url":"http://p:1"}}`)
	if m["session"] != "a" || m["message"] != "Session created successfully." || f.proxy.URL != "http://p:1" {
		t.Fatal(m)
	}
	_, m = post(t, h, `{"cmd":"sessions.create","session":"a"}`)
	if m["message"] != "Session already exists." {
		t.Fatal(m)
	}
	_, m = post(t, h, `{"cmd":"sessions.list"}`)
	if s := m["sessions"].([]any); len(s) != 1 || s[0] != "a" {
		t.Fatal(m)
	}
	_, m = post(t, h, `{"cmd":"sessions.destroy","session":"a"}`)
	if m["message"] != "The session has been removed." {
		t.Fatal(m)
	}
	_, m = post(t, h, `{"cmd":"sessions.list"}`)
	if s, ok := m["sessions"].([]any); !ok || len(s) != 0 {
		t.Fatalf("empty list must be [] not missing: %v", m)
	}
}

func TestIndexHealthNotFound(t *testing.T) {
	_, h := newTest(nil)
	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	if c, m := get("/"); c != 200 || m["msg"] != "FlareSolverr is ready!" || m["userAgent"] != "UA/1.0" || m["version"] != "1.2.3" {
		t.Fatal(c, m)
	}
	if c, m := get("/health"); c != 200 || m["status"] != "ok" {
		t.Fatal(c, m)
	}
	if c, _ := get("/missing"); c != 404 {
		t.Fatal(c)
	}
}

func TestPySeconds(t *testing.T) {
	for ms, want := range map[int]string{60000: "60.0", 45500: "45.5", 12345: "12.345", 1: "0.001"} {
		if got := pySeconds(ms); got != want {
			t.Errorf("%d: %s", ms, got)
		}
	}
}
