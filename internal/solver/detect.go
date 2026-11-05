package solver

import (
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Titles and selectors are taken from FlareSolverr (src/flaresolverr_service.py)
// so detection behaves the same way.

var accessDeniedTitles = []string{
	// Cloudflare
	"Access denied",
	// Cloudflare http://bitturk.net/ Firefox
	"Attention Required! | Cloudflare",
}

var accessDeniedSelectors = []string{
	// Cloudflare
	"div.cf-error-title span.cf-code-label span",
	// Cloudflare http://bitturk.net/ Firefox
	"#cf-error-details div.cf-error-overview h1",
}

var challengeTitles = []string{
	// Cloudflare
	"Just a moment...",
	// DDoS-GUARD
	"DDoS-Guard",
}

var challengeSelectors = []string{
	// Cloudflare
	"#cf-challenge-running", ".ray_id", ".attack-box", "#cf-please-wait", "#challenge-spinner",
	"#trk_jschal_js", "#turnstile-wrapper", ".lds-ring",
	// Custom CloudFlare for EbookParadijs, Film-Paleis, MuziekFabriek and Puur-Hollands
	"td.info #js_info",
	// Fairlane / pararius.com
	"div.vc div.text-box h2",
}

var turnstileSelectors = []string{
	"input[name='cf-turnstile-response']",
}

// PageState is the result of inspecting a page.
type PageState int

const (
	StateClear PageState = iota
	StateChallenge
	StateAccessDenied
)

func (s PageState) String() string {
	switch s {
	case StateChallenge:
		return "challenge"
	case StateAccessDenied:
		return "access-denied"
	default:
		return "clear"
	}
}

// Detection describes what was found on the page and why.
type Detection struct {
	State     PageState
	Reason    string // title or selector that matched
	Turnstile bool   // a Turnstile response input is present
}

// Detect inspects a page title and HTML the same way FlareSolverr does:
// access denied first, then challenge titles, then challenge selectors.
func Detect(title, html string) Detection {
	var doc *goquery.Document
	if html != "" {
		doc, _ = goquery.NewDocumentFromReader(strings.NewReader(html))
	}
	has := func(sel string) bool {
		return doc != nil && doc.Find(sel).Length() > 0
	}

	d := Detection{}
	for _, sel := range turnstileSelectors {
		if has(sel) {
			d.Turnstile = true
			break
		}
	}

	for _, t := range accessDeniedTitles {
		if strings.HasPrefix(title, t) {
			d.State, d.Reason = StateAccessDenied, "title: "+t
			return d
		}
	}
	for _, sel := range accessDeniedSelectors {
		if has(sel) {
			d.State, d.Reason = StateAccessDenied, "selector: "+sel
			return d
		}
	}
	for _, t := range challengeTitles {
		if strings.EqualFold(t, title) {
			d.State, d.Reason = StateChallenge, "title: "+t
			return d
		}
	}
	for _, sel := range challengeSelectors {
		if has(sel) {
			d.State, d.Reason = StateChallenge, "selector: "+sel
			return d
		}
	}
	return d
}
