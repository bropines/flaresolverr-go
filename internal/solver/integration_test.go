package solver

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/bropines/flaresolverr-go/internal/config"
)

// TestIntegrationCloudflare opens a real Cloudflare-protected page with a real
// browser. It runs only when INTEGRATION=1:
//
//	INTEGRATION=1 [INTEGRATION_URL=...] [INTEGRATION_PROXY=socks5://host:port] go test -run Integration ./internal/solver
//
// Cloudflare answers differently per IP: from some networks the page shows no
// challenge at all, from others the IP is blocked. Use a proxy in a normal
// datacenter or residential range if the result looks odd.
func TestIntegrationCloudflare(t *testing.T) {
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("set INTEGRATION=1 to run against a real Cloudflare page")
	}
	url := os.Getenv("INTEGRATION_URL")
	if url == "" {
		url = "https://www.scrapingcourse.com/cloudflare-challenge"
	}
	cfg, err := config.Load()
	if err != nil {
		t.Skip(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(cfg, log)
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := m.Solve(ctx, &Request{
		Method: "GET",
		URL:    url,
		Proxy:  ParseProxy(os.Getenv("INTEGRATION_PROXY"), "", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s -> %s (%d), %d cookies", res.Message, res.URL, res.Status, len(res.Cookies))
	if res.Message != "Challenge solved!" {
		t.Fatalf("no challenge was solved: %s", res.Message)
	}
	for _, c := range res.Cookies {
		if c.Name == "cf_clearance" {
			return
		}
	}
	t.Fatal("no cf_clearance cookie")
}
