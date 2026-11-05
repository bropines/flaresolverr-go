package solver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Request is one request.get / request.post command.
type Request struct {
	Method            string // GET or POST
	URL               string
	PostData          string // application/x-www-form-urlencoded body
	Cookies           []CookieParam
	Proxy             *Proxy
	Session           string
	SessionTTL        time.Duration
	ReturnOnlyCookies bool
	ReturnScreenshot  bool
	WaitInSeconds     int
	BlockMedia        *bool // overrides BLOCK_MEDIA
	TabsTillVerify    *int  // Turnstile token mode (FlareSolverr's tabs_till_verify)
}

// CookieParam is a cookie sent with the request.
type CookieParam struct {
	Name     string
	Value    string
	Domain   string
	Path     string
	Secure   bool
	HTTPOnly bool
	Expires  float64
	SameSite string
}

// Result is what the browser ended up with.
type Result struct {
	Message        string
	URL            string
	Status         int
	Headers        map[string]string
	HTML           string
	Cookies        []*network.Cookie
	UserAgent      string
	Screenshot     string
	TurnstileToken string
}

var (
	// ErrTimeout means maxTimeout passed before the page was solved.
	ErrTimeout = errTimeout
	// ErrAccessDenied means Cloudflare blocked the request outright.
	ErrAccessDenied = errors.New("Cloudflare has blocked this request. Probably your IP is banned for this site, check in your web browser.")
)

const pollInterval = 500 * time.Millisecond

// Solve opens the URL in a fresh incognito context (or the named session),
// waits until any challenge is gone and returns the page.
func (m *Manager) Solve(ctx context.Context, req *Request) (*Result, error) {
	res, err := m.solve(ctx, req)
	if err != nil && ctx.Err() != nil {
		return nil, ErrTimeout
	}
	return res, err
}

func (m *Manager) solve(ctx context.Context, req *Request) (*Result, error) {
	inst, release, err := m.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var tabCtx context.Context
	if req.Session != "" {
		s, err := m.getSession(req.Session, req.Proxy, req.SessionTTL)
		if err != nil {
			return nil, err
		}
		s.Lock()
		defer s.Unlock()
		s.touch()
		defer s.touch()
		tabCtx = s.ctx
	} else {
		c, cancel := chromedp.NewContext(inst.ctx, chromedp.WithNewBrowserContext(req.Proxy.contextOption()))
		defer cancel()
		// create the tab with a context that has no deadline; the tab must
		// outlive the request context only until cancel above.
		if err := chromedp.Do(c); err != nil {
			return nil, fmt.Errorf("cannot open tab: %w", err)
		}
		tabCtx = c
	}

	// rctx keeps the chromedp values of the tab but ends with the request.
	rctx, rcancel := context.WithCancel(tabCtx)
	defer rcancel()
	stop := context.AfterFunc(ctx, rcancel)
	defer stop()

	r := &run{m: m, inst: inst, req: req, t: chromedp.FromContext(tabCtx).Target}
	defer r.cleanup(tabCtx)
	return r.do(rctx)
}

// run holds the state of one request on one tab.
type run struct {
	m    *Manager
	inst *instance
	req  *Request
	t    *chromedp.Target

	mu       sync.Mutex
	status   int
	headers  map[string]string
	navs     int
	postDone bool
	clicks   int
	worldID  runtime.ExecutionContextID
	worldNav int
	fetchOn  bool
}

func (r *run) navCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.navs
}

func (r *run) blockMedia() bool {
	if r.req.BlockMedia != nil {
		return *r.req.BlockMedia
	}
	return r.m.cfg.BlockMedia
}

func (r *run) do(ctx context.Context) (*Result, error) {
	log := r.m.log
	mainFrame := cdp.FrameID(r.t.TargetID)

	if err := chromedp.Listen(ctx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			if e.Type == network.ResourceTypeDocument && e.FrameID == mainFrame && e.Response != nil {
				r.mu.Lock()
				r.status = int(e.Response.Status)
				r.headers = flattenHeaders(e.Response.Headers)
				r.mu.Unlock()
			}
		case *network.EventLoadingFailed:
			if e.BlockedReason == "" {
				log.Debug("request failed", "type", e.Type, "error", e.ErrorText)
			}
		case *page.EventFrameNavigated:
			if e.Frame != nil && e.Frame.ParentID == "" {
				r.mu.Lock()
				r.navs++
				r.mu.Unlock()
			}
		}
	}); err != nil {
		return nil, err
	}

	if log.Enabled(ctx, slog.LevelDebug) {
		go func() {
			for msg, err := range chromedp.Console(ctx) {
				if err != nil {
					return
				}
				log.Debug("page console", "type", msg.Type, "text", msg.Text, "url", msg.URL)
			}
		}()
	}

	if err := r.setup(ctx); err != nil {
		return nil, err
	}

	log.Debug("navigating", "url", r.req.URL, "method", r.req.Method)
	nav, err := cdp.Call(ctx, r.t, page.Navigate, page.NavigateParams{URL: r.req.URL})
	if err != nil {
		return nil, err
	}
	if nav.ErrorText != "" && nav.ErrorText != "net::ERR_HTTP_RESPONSE_CODE_FAILURE" && nav.ErrorText != "net::ERR_ABORTED" {
		return nil, errors.New(nav.ErrorText)
	}

	res := &Result{}
	var (
		challengeSeen bool
		challengeAt   time.Time
		navsAtChall   int
		lastClick     time.Time
		clearSince    time.Time
		snap          snapshot
		det           Detection
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, err := r.snapshot(ctx)
		if err != nil {
			// usually "execution context was destroyed" during a navigation
			if serr := sleep(ctx, pollInterval); serr != nil {
				return nil, serr
			}
			continue
		}
		now := time.Now()
		snap, det = s, Detect(s.Title, s.HTML)
		switch det.State {
		case StateAccessDenied:
			return nil, ErrAccessDenied
		case StateChallenge:
			clearSince = time.Time{}
			navsAtChall = r.navCount()
			if !challengeSeen {
				challengeSeen, challengeAt = true, now
				log.Info("challenge detected", "reason", det.Reason, "url", s.URL)
			}
			// give the automatic (non-interactive) check a few seconds,
			// then try the checkbox the way FlareSolverr does
			if now.Sub(challengeAt) > 4*time.Second && now.Sub(lastClick) > 8*time.Second {
				r.clickVerify(ctx, 1)
				lastClick = time.Now()
			}
		default:
			if clearSince.IsZero() {
				clearSince = now
			}
			if r.isLoaded(s, now.Sub(clearSince)) &&
				// after a challenge, wait for Cloudflare's redirect to land
				(!challengeSeen || r.navCount() > navsAtChall || now.Sub(clearSince) > 3*time.Second) {
				goto solved
			}
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return nil, err
		}
	}

solved:
	if challengeSeen {
		log.Info("challenge solved", "took", time.Since(challengeAt).Round(time.Millisecond))
		res.Message = "Challenge solved!"
	} else {
		log.Info("challenge not detected")
		res.Message = "Challenge not detected!"
	}

	if r.req.TabsTillVerify != nil && det.Turnstile {
		tok, err := r.turnstileToken(ctx, *r.req.TabsTillVerify)
		if err != nil {
			return nil, err
		}
		res.TurnstileToken = tok
	}

	if !r.req.ReturnOnlyCookies && r.req.WaitInSeconds > 0 {
		log.Info("waiting before returning the response", "seconds", r.req.WaitInSeconds)
		if err := sleep(ctx, time.Duration(r.req.WaitInSeconds)*time.Second); err != nil {
			return nil, err
		}
	}

	if s, err := r.snapshot(ctx); err == nil {
		snap = s
	}
	res.URL = snap.URL
	res.UserAgent = r.inst.ua
	r.mu.Lock()
	res.Status, res.Headers = r.status, r.headers
	r.mu.Unlock()
	if res.Status == 0 {
		res.Status = 200
	}
	if !r.req.ReturnOnlyCookies {
		res.HTML = snap.Doctype + snap.HTML
		if r.m.cfg.LogHTML {
			log.Debug("response HTML", "html", res.HTML)
		}
	}
	if r.req.ReturnScreenshot {
		shot, err := cdp.Call(ctx, r.t, page.CaptureScreenshot, page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng})
		if err == nil {
			res.Screenshot = base64.StdEncoding.EncodeToString(shot.Data)
		}
	}
	cookies, err := cdp.Call(ctx, r.t, network.GetCookies, network.GetCookiesParams{URLs: []string{snap.URL}})
	if err != nil {
		return nil, fmt.Errorf("cannot read cookies: %w", err)
	}
	res.Cookies = cookies.Cookies
	return res, nil
}

// isLoaded decides that a challenge-free page is ready to be returned.
func (r *run) isLoaded(s snapshot, clearFor time.Duration) bool {
	if r.navCount() == 0 || s.URL == "about:blank" {
		return false // our navigation has not committed yet
	}
	switch s.Ready {
	case "complete":
		return true
	case "interactive":
		// some pages never finish loading (long polls, slow trackers)
		return clearFor > 5*time.Second
	}
	return false
}

// setup prepares the tab before the first navigation.
func (r *run) setup(ctx context.Context) error {
	// chromedp enables the Runtime domain when it attaches. An enabled
	// Runtime is a known automation tell (console argument serialisation
	// leaks it), so turn it off; Runtime.evaluate works without it.
	if _, err := cdp.Call(ctx, r.t, runtime.Disable, cdp.Empty{}); err != nil {
		return err
	}
	if _, err := cdp.Call(ctx, r.t, network.Enable, network.EnableParams{}); err != nil {
		return err
	}
	if r.inst.ua != "" || r.inst.meta != nil {
		if _, err := cdp.Call(ctx, r.t, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{
			UserAgent:         r.inst.ua,
			Platform:          "Linux x86_64",
			UserAgentMetadata: r.inst.meta,
		}); err != nil {
			return err
		}
	}
	if len(r.req.Cookies) > 0 {
		params := network.SetCookiesParams{}
		for _, c := range r.req.Cookies {
			p := &network.CookieParam{
				Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
				Secure: c.Secure, HTTPOnly: c.HTTPOnly,
				SameSite: network.CookieSameSite(c.SameSite),
			}
			if p.Domain == "" {
				p.URL = r.req.URL
			}
			if c.Expires > 0 {
				p.Expires = cdp.TimeSinceEpoch(c.Expires)
			}
			params.Cookies = append(params.Cookies, p)
		}
		if _, err := cdp.Call(ctx, r.t, network.SetCookies, params); err != nil {
			return fmt.Errorf("cannot set cookies: %w", err)
		}
	}

	var patterns []*fetch.RequestPattern
	auth := r.req.Proxy.hasAuth()
	if auth {
		// proxy auth needs every request to pass through Fetch
		patterns = []*fetch.RequestPattern{{URLPattern: "*"}}
	} else {
		if r.blockMedia() {
			for _, rt := range []network.ResourceType{network.ResourceTypeImage, network.ResourceTypeFont, network.ResourceTypeMedia} {
				patterns = append(patterns, &fetch.RequestPattern{URLPattern: "*", ResourceType: rt})
			}
		}
		if r.req.Method == "POST" {
			patterns = append(patterns, &fetch.RequestPattern{URLPattern: "*", ResourceType: network.ResourceTypeDocument})
		}
	}
	if len(patterns) == 0 {
		return nil
	}

	go func() {
		for ev, err := range chromedp.Events(ctx, fetch.RequestPaused) {
			if err != nil {
				return
			}
			go r.onPaused(ctx, ev)
		}
	}()
	if auth {
		go func() {
			for ev, err := range chromedp.Events(ctx, fetch.AuthRequired) {
				if err != nil {
					return
				}
				go r.onAuth(ctx, ev)
			}
		}()
	}
	if _, err := cdp.Call(ctx, r.t, fetch.Enable, fetch.EnableParams{Patterns: patterns, HandleAuthRequests: &auth}); err != nil {
		return err
	}
	r.fetchOn = true
	return nil
}

func (r *run) onPaused(ctx context.Context, ev fetch.EventRequestPaused) {
	rt := ev.ResourceType
	if r.blockMedia() && isMedia(rt) && (ev.Request == nil || !isChallengeURL(ev.Request.URL)) {
		_, _ = cdp.Call(ctx, r.t, fetch.FailRequest, fetch.FailRequestParams{RequestID: ev.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
		return
	}
	params := fetch.ContinueRequestParams{RequestID: ev.RequestID}
	if r.req.Method == "POST" && rt == network.ResourceTypeDocument && ev.FrameID == cdp.FrameID(r.t.TargetID) {
		r.mu.Lock()
		first := !r.postDone
		r.postDone = true
		r.mu.Unlock()
		if first {
			params.Method = "POST"
			params.PostData = []byte(strings.TrimPrefix(r.req.PostData, "?"))
			params.Headers = []*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/x-www-form-urlencoded"}}
			if ev.Request != nil {
				for k, v := range flattenHeaders(ev.Request.Headers) {
					if !strings.EqualFold(k, "content-type") && !strings.EqualFold(k, "content-length") {
						params.Headers = append(params.Headers, &fetch.HeaderEntry{Name: k, Value: v})
					}
				}
			}
		}
	}
	_, _ = cdp.Call(ctx, r.t, fetch.ContinueRequest, params)
}

func (r *run) onAuth(ctx context.Context, ev fetch.EventAuthRequired) {
	resp := &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseCancelAuth}
	if ev.AuthChallenge != nil && ev.AuthChallenge.Source == fetch.AuthChallengeSourceProxy {
		resp = &fetch.AuthChallengeResponse{
			Response: fetch.AuthChallengeResponseResponseProvideCredentials,
			Username: r.req.Proxy.Username,
			Password: r.req.Proxy.Password,
		}
	}
	_, _ = cdp.Call(ctx, r.t, fetch.ContinueWithAuth, fetch.ContinueWithAuthParams{RequestID: ev.RequestID, AuthChallengeResponse: resp})
}

// cleanup leaves a reused (session) tab in a clean state.
func (r *run) cleanup(tabCtx context.Context) {
	if r.req.Session == "" || !r.fetchOn || tabCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(tabCtx, 3*time.Second)
	defer cancel()
	_, _ = cdp.Call(ctx, r.t, fetch.Disable, cdp.Empty{})
}

type snapshot struct {
	Title   string `json:"title"`
	HTML    string `json:"html"`
	Doctype string `json:"doctype"`
	Ready   string `json:"ready"`
	URL     string `json:"url"`
}

const snapshotJS = `(() => {
  const de = document.documentElement;
  const dt = document.doctype ? new XMLSerializer().serializeToString(document.doctype) : "";
  return {title: document.title, html: de ? de.outerHTML : "", doctype: dt, ready: document.readyState, url: location.href};
})()`

func (r *run) snapshot(ctx context.Context) (snapshot, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return evalJS[snapshot](c, r, snapshotJS)
}

// dump saves a screenshot and the HTML to DEBUG_DUMP_DIR (debugging aid).
func (r *run) dump(ctx context.Context) {
	dir := os.Getenv("DEBUG_DUMP_DIR")
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	name := filepath.Join(dir, time.Now().Format("150405.000"))
	if shot, err := cdp.Call(ctx, r.t, page.CaptureScreenshot, page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng}); err == nil {
		_ = os.WriteFile(name+".png", shot.Data, 0o644)
	}
	if s, err := r.snapshot(ctx); err == nil {
		_ = os.WriteFile(name+".html", []byte(s.HTML), 0o644)
	}
}

type point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// turnstileBoxJS finds the Turnstile widget and returns the checkbox position.
const turnstileBoxJS = `(() => {
  let el = null;
  const inp = document.querySelector("input[name='cf-turnstile-response']");
  if (inp) {
    el = inp.parentElement;
    while (el && el.getBoundingClientRect().width < 100) el = el.parentElement;
  }
  el = el || document.querySelector("iframe[src*='challenges.cloudflare.com']") || document.querySelector(".cf-turnstile, #turnstile-wrapper");
  if (!el) return null;
  el.scrollIntoView({block: "center"});
  const r = el.getBoundingClientRect();
  if (r.width < 20 || r.height < 20) return null;
  return {x: r.left + Math.min(28, r.width / 2), y: r.top + r.height / 2};
})()`

const verifyButtonJS = `(() => {
  const b = document.querySelector("input[type='button'][value='Verify you are human']");
  if (!b) return null;
  const r = b.getBoundingClientRect();
  return {x: r.left + 5, y: r.top + 7};
})()`

// clickVerify tries to tick the Turnstile checkbox. It clicks the widget with
// a real (trusted) mouse event when it can find it, and falls back to the
// Tab + Space trick that FlareSolverr uses.
func (r *run) clickVerify(ctx context.Context, tabs int) {
	log := r.m.log
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	r.dump(c)
	r.clicks++
	// A mouse click on the checkbox is what passes in tests, so it goes
	// first. The keyboard (FlareSolverr's Tab + Space) is the fallback for
	// pages where the widget box cannot be located or the click misses. The
	// widget often focuses its own checkbox; a Tab would then jump past it to
	// the logo link, so press only Space in that case.
	p, err := evalJS[*point](c, r, turnstileBoxJS)
	switch {
	case err == nil && p != nil && r.clicks%3 != 0:
		log.Debug("clicking Turnstile checkbox", "x", p.X, "y", p.Y)
		r.humanClick(c, p.X, p.Y)
	default:
		if focused, _ := evalJS[bool](c, r, focusInsideJS); focused {
			log.Debug("widget has focus, pressing Space")
			r.keyboardVerify(c, 0)
		} else {
			log.Debug("pressing Tab + Space", "tabs", tabs)
			r.keyboardVerify(c, tabs)
		}
	}
	if p, err := evalJS[*point](c, r, verifyButtonJS); err == nil && p != nil {
		log.Debug("clicking 'Verify you are human' button")
		r.humanClick(c, p.X, p.Y)
	}
}

func (r *run) humanClick(ctx context.Context, x, y float64) {
	x += rand.Float64()*4 - 2
	y += rand.Float64()*4 - 2
	sx, sy := x-80-rand.Float64()*120, y+40+rand.Float64()*80
	for i := 1; i <= 6; i++ {
		f := float64(i) / 6
		_ = chromedp.Do(ctx, chromedp.MouseEvent(chromedp.MouseMoved, sx+(x-sx)*f, sy+(y-sy)*f))
		_ = sleep(ctx, time.Duration(15+rand.IntN(30))*time.Millisecond)
	}
	_ = chromedp.Do(ctx, chromedp.MouseEvent(chromedp.MousePressed, x, y, chromedp.ButtonLeft, chromedp.ClickCount(1)))
	_ = sleep(ctx, time.Duration(60+rand.IntN(80))*time.Millisecond)
	_ = chromedp.Do(ctx, chromedp.MouseEvent(chromedp.MouseReleased, x, y, chromedp.ButtonLeft, chromedp.ClickCount(1)))
}

func (r *run) keyboardVerify(ctx context.Context, tabs int) {
	for range tabs {
		_ = chromedp.Do(ctx, chromedp.KeyEvent("\t"))
		_ = sleep(ctx, time.Duration(100+rand.IntN(100))*time.Millisecond)
	}
	_ = sleep(ctx, time.Duration(300+rand.IntN(300))*time.Millisecond)
	_ = chromedp.Do(ctx, chromedp.KeyEvent(" "))
}

const turnstileValueJS = `(() => { const i = document.querySelector("input[name='cf-turnstile-response']"); return i ? i.value : ""; })()`

// focusInsideJS: is the focus inside an element (the widget), not the page?
const focusInsideJS = `(() => { const a = document.activeElement; return !!a && a !== document.body && a !== document.documentElement; })()`

const focusResetJS = `(() => {
  let old = document.getElementById('__focus_helper'); if (old) old.remove();
  let el = document.createElement('button'); el.id = '__focus_helper';
  el.style.cssText = 'position:fixed;top:0;left:0;opacity:0.01;pointer-events:none';
  document.body.prepend(el); el.focus();
})()`

// turnstileToken implements FlareSolverr's tabs_till_verify mode: tick the
// widget until the hidden input gets a token.
func (r *run) turnstileToken(ctx context.Context, tabs int) (string, error) {
	initial, _ := evalJS[string](ctx, r, turnstileValueJS)
	for {
		r.clickVerify(ctx, tabs)
		tok, err := evalJS[string](ctx, r, turnstileValueJS)
		if err == nil && tok != "" && tok != initial {
			r.m.log.Info("Turnstile token received")
			return tok, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, _ = evalJS[any](ctx, r, focusResetJS)
		if err := sleep(ctx, time.Second); err != nil {
			return "", err
		}
	}
}

// evalJS runs an expression in an isolated world of the main frame. The
// isolated world shares the DOM with the page but not its JavaScript, so
// page scripts that hook DOM methods cannot see our calls.
func evalJS[T any](ctx context.Context, r *run, expr string) (T, error) {
	var v T
	for attempt := 0; ; attempt++ {
		world, err := r.world(ctx)
		if err != nil {
			return v, err
		}
		res, err := cdp.Call(ctx, r.t, runtime.Evaluate, runtime.EvaluateParams{
			Expression:    expr,
			ContextID:     world,
			ReturnByValue: new(true),
			AwaitPromise:  new(true),
		})
		if err != nil {
			// the world dies with its document; make a new one once
			r.mu.Lock()
			r.worldID = 0
			r.mu.Unlock()
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return v, err
		}
		if res.ExceptionDetails != nil {
			return v, fmt.Errorf("javascript error: %s", res.ExceptionDetails.Text)
		}
		if res.Result == nil || len(res.Result.Value) == 0 {
			return v, nil
		}
		err = json.Unmarshal(res.Result.Value, &v)
		return v, err
	}
}

// world returns the isolated world of the current main-frame document.
func (r *run) world(ctx context.Context) (runtime.ExecutionContextID, error) {
	r.mu.Lock()
	if r.worldID != 0 && r.worldNav == r.navs {
		id := r.worldID
		r.mu.Unlock()
		return id, nil
	}
	nav := r.navs
	r.mu.Unlock()
	res, err := cdp.Call(ctx, r.t, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{
		FrameID:   cdp.FrameID(r.t.TargetID),
		WorldName: "fsg",
	})
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.worldID, r.worldNav = res.ExecutionContextID, nav
	r.mu.Unlock()
	return res.ExecutionContextID, nil
}

func isMedia(rt network.ResourceType) bool {
	return rt == network.ResourceTypeImage || rt == network.ResourceTypeFont || rt == network.ResourceTypeMedia
}

// isChallengeURL marks Cloudflare challenge resources. They are never blocked:
// the challenge loads beacon images and fonts and fails without them.
func isChallengeURL(u string) bool {
	return strings.Contains(u, "challenges.cloudflare.com") || strings.Contains(u, "/cdn-cgi/challenge-platform/")
}

func flattenHeaders(h network.Headers) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = fmt.Sprint(v)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
