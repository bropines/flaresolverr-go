package solver

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		name      string
		title     string
		html      string
		want      PageState
		turnstile bool
	}{
		{"plain page", "Example Domain", `<html><body><h1>Example</h1></body></html>`, StateClear, false},
		{"cloudflare title", "Just a moment...", `<html><body></body></html>`, StateChallenge, false},
		{"title case-insensitive", "JUST A MOMENT...", ``, StateChallenge, false},
		{"ddos-guard title", "DDoS-Guard", ``, StateChallenge, false},
		{"challenge selector only", "rutracker.org", `<html><body><div id="challenge-spinner"></div></body></html>`, StateChallenge, false},
		{"ray id selector", "x", `<div class="ray_id">Ray ID: 123</div>`, StateChallenge, false},
		{"turnstile wrapper", "x", `<div id="turnstile-wrapper"><input type="hidden" name="cf-turnstile-response" value=""></div>`, StateChallenge, true},
		{"custom cloudflare", "x", `<table><tr><td class="info"><div id="js_info">wait</div></td></tr></table>`, StateChallenge, false},
		{"access denied title", "Access denied | example.com used Cloudflare to restrict access", ``, StateAccessDenied, false},
		{"attention required", "Attention Required! | Cloudflare", ``, StateAccessDenied, false},
		{"access denied selector", "x", `<div class="cf-error-title"><span class="cf-code-label">Error <span>1020</span></span></div>`, StateAccessDenied, false},
		{"turnstile on normal page", "Login", `<form><input type="hidden" name="cf-turnstile-response"></form>`, StateClear, true},
		{"title in body is not a title", "Forum", `<p>Just a moment...</p>`, StateClear, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Detect(c.title, c.html)
			if d.State != c.want {
				t.Fatalf("state = %v (%s), want %v", d.State, d.Reason, c.want)
			}
			if d.Turnstile != c.turnstile {
				t.Fatalf("turnstile = %v, want %v", d.Turnstile, c.turnstile)
			}
			if c.want != StateClear && d.Reason == "" {
				t.Fatal("no reason given")
			}
		})
	}
}

// A real "Just a moment..." page trimmed to its structure.
const cloudflarePage = `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title>
<meta http-equiv="refresh" content="390"></head><body><div class="main-wrapper" role="main">
<div class="main-content"><noscript><div class="h2"><span id="challenge-error-text">
Enable JavaScript and cookies to continue</span></div></noscript></div></div>
<script>(function(){window._cf_chl_opt={cvId: '3'};})();</script></body></html>`

func TestDetectRealPage(t *testing.T) {
	if d := Detect("Just a moment...", cloudflarePage); d.State != StateChallenge {
		t.Fatalf("got %v", d.State)
	}
}

func TestIsChallengeURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://challenges.cloudflare.com/cdn-cgi/challenge-platform/h/b/ci/x":      true,
		"https://example.com/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1": true,
		"https://example.com/logo.png":                                               false,
	} {
		if got := isChallengeURL(u); got != want {
			t.Errorf("%s: got %v", u, got)
		}
	}
}
