// Package api implements the FlareSolverr v1 HTTP API.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/network"

	"github.com/bropines/flaresolverr-go/internal/solver"
)

// flexInt accepts a JSON number or a numeric string.
type flexInt struct {
	set bool
	v   int
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid number %s", b)
	}
	f.set, f.v = true, int(n)
	return nil
}

func (f flexInt) MarshalJSON() ([]byte, error) {
	if !f.set {
		return []byte("null"), nil
	}
	return []byte(strconv.Itoa(f.v)), nil
}

// V1Request has the fields of FlareSolverr's V1RequestBase.
type V1Request struct {
	Cmd               string     `json:"cmd"`
	URL               string     `json:"url"`
	PostData          *string    `json:"postData"`
	MaxTimeout        flexInt    `json:"maxTimeout"`
	Cookies           []CookieIn `json:"cookies"`
	Proxy             *ProxyIn   `json:"proxy"`
	Session           string     `json:"session"`
	SessionTTLMinutes flexInt    `json:"session_ttl_minutes"`
	ReturnOnlyCookies bool       `json:"returnOnlyCookies"`
	ReturnScreenshot  bool       `json:"returnScreenshot"`
	WaitInSeconds     flexInt    `json:"waitInSeconds"`
	DisableMedia      *bool      `json:"disableMedia"`
	TabsTillVerify    flexInt    `json:"tabs_till_verify"`
	Headers           any        `json:"headers"`       // removed in FlareSolverr v2
	UserAgent         *string    `json:"userAgent"`     // removed in FlareSolverr v2
	Download          *bool      `json:"download"`      // removed in FlareSolverr v2
	ReturnRawHTML     *bool      `json:"returnRawHtml"` // removed in FlareSolverr v2
}

type ProxyIn struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// CookieIn accepts both Selenium ("expiry") and CDP ("expires") cookie shapes.
type CookieIn struct {
	Name     string   `json:"name"`
	Value    string   `json:"value"`
	Domain   string   `json:"domain"`
	Path     string   `json:"path"`
	Secure   bool     `json:"secure"`
	HTTPOnly bool     `json:"httpOnly"`
	SameSite string   `json:"sameSite"`
	Expiry   *float64 `json:"expiry"`
	Expires  *float64 `json:"expires"`
}

// V1Response has the fields of FlareSolverr's V1ResponseBase.
type V1Response struct {
	Status         string    `json:"status"`
	Message        string    `json:"message"`
	Session        string    `json:"session,omitempty"`
	Sessions       *[]string `json:"sessions,omitempty"`
	StartTimestamp int64     `json:"startTimestamp"`
	EndTimestamp   int64     `json:"endTimestamp"`
	Version        string    `json:"version"`
	Solution       *Solution `json:"solution,omitempty"`
}

// Solution matches ChallengeResolutionResultT. headers and response are left
// out when returnOnlyCookies is set, like FlareSolverr does.
type Solution struct {
	URL            string             `json:"url"`
	Status         int                `json:"status"`
	Headers        *map[string]string `json:"headers,omitempty"`
	Response       *string            `json:"response,omitempty"`
	Cookies        []Cookie           `json:"cookies"`
	UserAgent      string             `json:"userAgent"`
	Screenshot     *string            `json:"screenshot,omitempty"`
	TurnstileToken *string            `json:"turnstile_token"`
}

// Cookie carries both the CDP fields (expires, size, session) that
// FlareSolverrSharp and Prowlarr read, and Selenium's "expiry" that
// FlareSolverr v3 returns.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Expiry   *int64  `json:"expiry,omitempty"`
	Size     int64   `json:"size"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	Session  bool    `json:"session"`
	SameSite string  `json:"sameSite,omitempty"`
}

type IndexResponse struct {
	Msg       string `json:"msg"`
	Version   string `json:"version"`
	UserAgent string `json:"userAgent"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

// skipHeaders would describe the browser's transfer of the body, not the
// decoded HTML we return.
var skipHeaders = map[string]bool{
	"content-encoding": true, "content-length": true, "transfer-encoding": true,
}

func toCookies(in []*network.Cookie) []Cookie {
	out := make([]Cookie, 0, len(in))
	for _, c := range in {
		if c == nil {
			continue
		}
		oc := Cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: float64(c.Expires), Size: c.Size, HTTPOnly: c.HTTPOnly,
			Secure: c.Secure, Session: c.Session, SameSite: string(c.SameSite),
		}
		if !c.Session && c.Expires > 0 {
			e := int64(math.Floor(float64(c.Expires)))
			oc.Expiry = &e
		}
		if c.Session {
			oc.Expires = -1
		}
		out = append(out, oc)
	}
	return out
}

func toSolution(r *solver.Result, onlyCookies bool) *Solution {
	s := &Solution{
		URL:       r.URL,
		Status:    r.Status,
		Cookies:   toCookies(r.Cookies),
		UserAgent: r.UserAgent,
	}
	if !onlyCookies {
		h := map[string]string{}
		for k, v := range r.Headers {
			if !skipHeaders[k] {
				h[k] = v
			}
		}
		html := r.HTML
		s.Headers, s.Response = &h, &html
	}
	if r.Screenshot != "" {
		shot := r.Screenshot
		s.Screenshot = &shot
	}
	if r.TurnstileToken != "" {
		tok := r.TurnstileToken
		s.TurnstileToken = &tok
	}
	return s
}

func (c CookieIn) toParam() solver.CookieParam {
	p := solver.CookieParam{
		Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
		Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite,
	}
	switch {
	case c.Expiry != nil:
		p.Expires = *c.Expiry
	case c.Expires != nil:
		p.Expires = *c.Expires
	}
	return p
}

// redacted returns the request as JSON for logs, without proxy secrets.
func (r V1Request) redacted() string {
	if r.Proxy != nil {
		p := *r.Proxy
		if p.Password != "" {
			p.Password = "***"
		}
		r.Proxy = &p
	}
	b, _ := json.Marshal(r)
	return string(b)
}
