package solver

import "testing"

func TestBuildUserAgent(t *testing.T) {
	got := BuildUserAgent("156.0.8078.79")
	want := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestVersionRe(t *testing.T) {
	for out, want := range map[string]string{
		"Chromium 156.0.8078.79 built on Debian GNU/Linux 13 (trixie)\n": "156.0.8078.79",
		"Chromium 156.0.8078.79 Alpine Linux":                            "156.0.8078.79",
		"Thorium 138.0.7204.303":                                         "138.0.7204.303",
		"Google Chrome 152.0.7977.82 ":                                   "152.0.7977.82",
	} {
		if got := versionRe.FindString(out); got != want {
			t.Errorf("%q: got %q", out, got)
		}
	}
}

func TestBuildMetadataDropsHeadless(t *testing.T) {
	m := buildMetadata(uaData{
		Brands:          []uaBrand{{"Not:A-Brand", "8"}, {"HeadlessChrome", "156"}},
		FullVersionList: []uaBrand{{"Not:A-Brand", "8.0.0.0"}, {"HeadlessChrome", "156.0.8078.79"}, {"Chromium", "156.0.8078.79"}},
		Platform:        "Linux",
	})
	if len(m.Brands) != 2 || m.Brands[1].Brand != "Chromium" {
		t.Fatalf("brands: %+v", m.Brands)
	}
	for _, b := range m.FullVersionList {
		if b.Brand == "HeadlessChrome" {
			t.Fatal("HeadlessChrome kept in full version list")
		}
	}
	if len(m.FullVersionList) != 2 {
		t.Fatalf("full version list: %d entries", len(m.FullVersionList))
	}
	if m.Platform != "Linux" || m.Architecture != "x86" || m.Bitness != "64" {
		t.Fatalf("defaults: %+v", m)
	}
}

func TestParseProxy(t *testing.T) {
	cases := []struct {
		raw, user, pass string
		want            Proxy
	}{
		{"http://10.0.0.1:3128", "", "", Proxy{URL: "http://10.0.0.1:3128"}},
		{"10.0.0.1:3128", "", "", Proxy{URL: "http://10.0.0.1:3128"}},
		{"socks5://127.0.0.1:1080", "", "", Proxy{URL: "socks5://127.0.0.1:1080"}},
		{"http://bob:s3cret@proxy:8080", "", "", Proxy{URL: "http://proxy:8080", Username: "bob", Password: "s3cret"}},
		{"http://bob:old@proxy:8080", "alice", "new", Proxy{URL: "http://proxy:8080", Username: "alice", Password: "new"}},
	}
	for _, c := range cases {
		got := ParseProxy(c.raw, c.user, c.pass)
		if got == nil || *got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.raw, got, c.want)
		}
	}
	if ParseProxy("  ", "", "") != nil {
		t.Error("empty proxy should be nil")
	}
	if (*Proxy)(nil).hasAuth() {
		t.Error("nil proxy has no auth")
	}
}
