// Package solver drives a headless Chromium over the DevTools protocol to get
// past Cloudflare / DDoS-Guard challenges.
package solver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"

	"github.com/bropines/flaresolverr-go/internal/config"
)

// Manager owns the browser process. The browser starts on the first request,
// is shared by all requests (each one gets its own incognito context) and is
// killed after it has been idle for cfg.IdleTimeout.
type Manager struct {
	cfg *config.Config
	log *slog.Logger
	sem chan struct{}

	mu       sync.Mutex
	inst     *instance
	active   int // requests holding a slot or using a session
	lastUse  time.Time
	sessions map[string]*Session

	versionOnce sync.Once
	version     string // full browser version from --version, e.g. 142.0.7444.59
	ua          string // user agent built from version
}

// instance is one running browser process.
type instance struct {
	ctx         context.Context // first chromedp context: owns the browser
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
	ua          string
	meta        *emulation.UserAgentMetadata
	started     time.Time
}

func NewManager(cfg *config.Config, log *slog.Logger) *Manager {
	m := &Manager{
		cfg:      cfg,
		log:      log,
		sem:      make(chan struct{}, cfg.MaxConcurrency),
		sessions: map[string]*Session{},
		lastUse:  time.Now(),
	}
	go m.reaper()
	return m
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)\.(\d+)`)

// detectVersion runs "<browser> --version" once. It does not start a full
// browser, so GET / can report the user agent without paying for a launch.
func (m *Manager) detectVersion() {
	m.versionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, m.cfg.BrowserPath, "--version").Output()
		if err != nil {
			m.log.Warn("cannot read browser version", "path", m.cfg.BrowserPath, "err", err)
			return
		}
		v := versionRe.FindString(string(out))
		if v == "" {
			m.log.Warn("cannot parse browser version", "output", strings.TrimSpace(string(out)))
			return
		}
		m.version = v
		m.ua = BuildUserAgent(v)
		m.log.Info("browser detected", "path", m.cfg.BrowserPath, "version", strings.TrimSpace(string(out)))
	})
}

// BuildUserAgent returns the user agent a desktop Chrome on Linux sends.
// Chrome reduces the version to MAJOR.0.0.0 since v110.
func BuildUserAgent(version string) string {
	major, _, _ := strings.Cut(version, ".")
	return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", major)
}

// UserAgent is the user agent the browser uses (best effort before launch).
func (m *Manager) UserAgent() string {
	m.mu.Lock()
	inst := m.inst
	m.mu.Unlock()
	if inst != nil && inst.ua != "" {
		return inst.ua
	}
	m.detectVersion()
	return m.ua
}

func (m *Manager) allocatorOptions() []chromedp.ExecAllocatorOption {
	lang := "en-US"
	if l := m.cfg.Lang; l != "" && l != "C" && l != "POSIX" && !strings.HasPrefix(l, "C.") {
		lang = strings.ReplaceAll(strings.SplitN(l, ".", 2)[0], "_", "-")
	}
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(m.cfg.BrowserPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		// containers usually have no user namespaces for the sandbox
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("no-zygote", true),
		chromedp.Flag("disable-gpu", true),
		// software WebGL (SwiftShader); challenges fingerprint WebGL
		chromedp.Flag("enable-unsafe-swiftshader", true),
		// HTTP/3 breaks behind small-MTU overlays (docker, k8s CNI); H2 is fine
		chromedp.Flag("disable-quic", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("renderer-process-limit", "1"),
		// Keep site isolation: Turnstile fails when its iframe shares the
		// page process (tested), so no --disable-site-isolation-trials.
		// no navigator.webdriver; we also never pass --enable-automation
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-features", strings.Join([]string{
			"Translate", "MediaRouter", "DialMediaRouteProvider", "OptimizationHints",
			"OptimizationGuideModelDownloading", "OptimizationGuideOnDeviceModel",
			"AutofillServerCommunication", "CertificateTransparencyComponentUpdater",
			"InterestFeedContentSuggestions", "LocalNetworkAccessChecks", "BackForwardCache",
			"HttpsUpgrades",
			// no pre-started spare renderer: one process less while solving
			"SpareRendererForSitePerProcess",
		}, ",")),
		// network service inside the browser process: one process less
		chromedp.Flag("enable-features", "NetworkServiceInProcess"),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-search-engine-choice-screen", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("window-size", "1920,1080"),
		chromedp.Flag("lang", lang),
		chromedp.Flag("accept-lang", lang+","+strings.SplitN(lang, "-", 2)[0]),
	}
	if m.cfg.Headless {
		opts = append(opts, chromedp.Flag("headless", "new"), chromedp.Flag("hide-scrollbars", true))
	}
	if m.ua != "" {
		// also covers workers, which Emulation.setUserAgentOverride does not reach
		opts = append(opts, chromedp.UserAgent(m.ua))
	}
	for _, a := range m.cfg.BrowserExtraArgs {
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case hasVal && val == "false": // --disable-gpu=false drops a default flag
			opts = append(opts, chromedp.Flag(name, false))
		case hasVal:
			opts = append(opts, chromedp.Flag(name, val))
		default:
			opts = append(opts, chromedp.Flag(name, true))
		}
	}
	return opts
}

type uaBrand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

type uaData struct {
	UserAgent       string    `json:"userAgent"`
	Brands          []uaBrand `json:"brands"`
	FullVersionList []uaBrand `json:"fullVersionList"`
	Platform        string    `json:"platform"`
	PlatformVersion string    `json:"platformVersion"`
	Architecture    string    `json:"architecture"`
	Bitness         string    `json:"bitness"`
	Model           string    `json:"model"`
	Mobile          bool      `json:"mobile"`
}

const uaDataJS = `(async () => {
  const d = navigator.userAgentData;
  const h = d ? await d.getHighEntropyValues(["architecture","bitness","fullVersionList","model","platformVersion"]) : {};
  return Object.assign({userAgent: navigator.userAgent, brands: d ? d.brands : [], mobile: d ? d.mobile : false, platform: d ? d.platform : "Linux"}, h);
})()`

// launch starts a new browser. Caller holds m.mu.
func (m *Manager) launch() (*instance, error) {
	m.detectVersion()
	start := time.Now()
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), m.allocatorOptions()...)
	bctx, bcancel := chromedp.NewContext(allocCtx,
		chromedp.WithErrorf(func(f string, a ...any) { m.log.Debug("cdp: " + fmt.Sprintf(f, a...)) }),
	)

	// The first Do starts the browser. It must not carry a timeout (that would
	// stop the browser later), so guard it with a timer instead.
	timer := time.AfterFunc(45*time.Second, bcancel)
	err := chromedp.Do(bctx)
	timer.Stop()
	if err != nil {
		bcancel()
		allocCancel()
		return nil, fmt.Errorf("cannot start browser: %w", err)
	}

	inst := &instance{ctx: bctx, cancel: bcancel, allocCancel: allocCancel, started: time.Now(), ua: m.ua}

	ectx, ecancel := context.WithTimeout(bctx, 10*time.Second)
	data, err := chromedp.Run(ectx, chromedp.Evaluate[uaData](uaDataJS, chromedp.EvalAwaitPromise))
	ecancel()
	if err != nil {
		m.log.Warn("cannot read navigator.userAgentData", "err", err)
	} else {
		if inst.ua == "" {
			inst.ua = strings.ReplaceAll(data.UserAgent, "HeadlessChrome", "Chrome")
		}
		inst.meta = buildMetadata(data)
	}
	m.log.Info("browser started", "took", time.Since(start).Round(time.Millisecond), "userAgent", inst.ua)
	return inst, nil
}

// buildMetadata turns navigator.userAgentData into client hints without the
// "HeadlessChrome" brand that headless mode reports.
func buildMetadata(d uaData) *emulation.UserAgentMetadata {
	fix := func(in []uaBrand) []*emulation.UserAgentBrandVersion {
		hasChromium := false
		for _, b := range in {
			if b.Brand == "Chromium" {
				hasChromium = true
			}
		}
		var out []*emulation.UserAgentBrandVersion
		for _, b := range in {
			name := b.Brand
			if name == "HeadlessChrome" {
				if hasChromium {
					continue
				}
				name = "Chromium"
			}
			out = append(out, &emulation.UserAgentBrandVersion{Brand: name, Version: b.Version})
		}
		return out
	}
	platform := d.Platform
	if platform == "" {
		platform = "Linux"
	}
	arch := d.Architecture
	if arch == "" {
		arch = "x86"
	}
	bitness := d.Bitness
	if bitness == "" {
		bitness = "64"
	}
	return &emulation.UserAgentMetadata{
		Brands:          fix(d.Brands),
		FullVersionList: fix(d.FullVersionList),
		Platform:        platform,
		PlatformVersion: d.PlatformVersion,
		Architecture:    arch,
		Bitness:         bitness,
		Model:           d.Model,
		Mobile:          d.Mobile,
	}
}

// alive reports whether the browser still answers.
func (inst *instance) alive() bool {
	if inst.ctx.Err() != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(inst.ctx, 5*time.Second)
	defer cancel()
	_, err := chromedp.Targets(ctx)
	return err == nil
}

func (inst *instance) stop() {
	inst.cancel()
	inst.allocCancel()
}

// ensure returns a running browser, starting one if needed. Caller holds m.mu.
func (m *Manager) ensure() (*instance, error) {
	if m.inst != nil && !m.inst.alive() {
		m.log.Warn("browser is gone, restarting")
		m.dropLocked()
	}
	if m.inst == nil {
		inst, err := m.launch()
		if err != nil {
			return nil, err
		}
		m.inst = inst
	}
	return m.inst, nil
}

// dropLocked stops the browser and forgets all sessions. Caller holds m.mu.
func (m *Manager) dropLocked() {
	if m.inst == nil {
		return
	}
	for id, s := range m.sessions {
		s.cancel()
		delete(m.sessions, id)
	}
	inst := m.inst
	m.inst = nil
	// stopping can take a moment (tabs close, process exits); do not block callers
	go inst.stop()
}

// acquire waits for a free slot (MAX_CONCURRENCY) and returns a running browser.
func (m *Manager) acquire(ctx context.Context) (*instance, func(), error) {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	m.mu.Lock()
	inst, err := m.ensure()
	if err != nil {
		m.mu.Unlock()
		<-m.sem
		return nil, nil, err
	}
	m.active++
	m.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			m.active--
			m.lastUse = time.Now()
			m.mu.Unlock()
			<-m.sem
		})
	}
	return inst, release, nil
}

// reaper stops the browser after it has been idle long enough and expires
// sessions that were not used for SESSION_TTL.
func (m *Manager) reaper() {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for range tick.C {
		m.mu.Lock()
		now := time.Now()
		for id, s := range m.sessions {
			if s.idleSince(now) > m.cfg.SessionTTL && s.TryLock() {
				m.log.Info("session expired", "session", id)
				s.cancel()
				delete(m.sessions, id)
				s.Unlock()
			}
		}
		if m.inst != nil && m.active == 0 && len(m.sessions) == 0 && now.Sub(m.lastUse) >= m.cfg.IdleTimeout {
			m.log.Info("browser idle, stopping", "idle", now.Sub(m.lastUse).Round(time.Second))
			m.dropLocked()
		}
		m.mu.Unlock()
	}
}

// Close stops the browser (used on shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	inst := m.inst
	for id, s := range m.sessions {
		s.cancel()
		delete(m.sessions, id)
	}
	m.inst = nil
	m.mu.Unlock()
	if inst != nil {
		inst.stop()
	}
}

// Running reports whether a browser process is up (for logs and tests).
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inst != nil
}

var errTimeout = errors.New("timeout")
