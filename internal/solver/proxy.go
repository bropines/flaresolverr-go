package solver

import (
	"net/url"
	"strings"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Proxy is a per-request proxy. Chromium applies it to the incognito browser
// context, so one browser can serve requests through different proxies.
type Proxy struct {
	URL      string
	Username string
	Password string
}

// ParseProxy normalises a proxy URL. Credentials inside the URL
// (http://user:pass@host:port) are moved to Username/Password, because
// Chromium does not accept them in --proxy-server.
func ParseProxy(raw, user, pass string) *Proxy {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	p := &Proxy{URL: raw, Username: user, Password: pass}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		if u.User != nil {
			if p.Username == "" {
				p.Username = u.User.Username()
			}
			if pw, ok := u.User.Password(); ok && p.Password == "" {
				p.Password = pw
			}
		}
		p.URL = u.Scheme + "://" + u.Host
	}
	return p
}

func (p *Proxy) hasAuth() bool {
	return p != nil && (p.Username != "" || p.Password != "")
}

func (p *Proxy) contextOption() chromedp.CreateBrowserContextOption {
	return func(params *target.CreateBrowserContextParams) {
		if p != nil && p.URL != "" {
			params.ProxyServer = p.URL
		}
	}
}
