# flaresolverr-go

A small drop-in replacement for [FlareSolverr](https://github.com/FlareSolverr/FlareSolverr),
written in Go. It speaks the same v1 API, so Jackett, Prowlarr, Sonarr and Radarr
work with it unchanged.

It drives Chromium directly over the DevTools protocol (no Selenium, no
chromedriver, no Python). The browser starts on the first request and is
killed after it has been idle for a while, so an idle container uses a few MB
instead of ~100 MB.

## Run

```sh
docker run -d --name flaresolverr -p 8191:8191 ghcr.io/bropines/flaresolverr-go:latest
```

```yaml
services:
  flaresolverr:
    image: ghcr.io/bropines/flaresolverr-go:latest
    ports: ["8191:8191"]
    environment:
      BROWSER_IDLE_TIMEOUT: 5m
    restart: unless-stopped
```

### Jackett / Prowlarr

- **Jackett**: Dashboard → *FlareSolverr API URL* → `http://flaresolverr:8191`
  (*FlareSolverr Max Timeout* 60000 is fine).
- **Prowlarr**: Settings → Indexers → add *FlareSolverr* proxy with host
  `http://flaresolverr:8191/` and tag the indexers that need it.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8191` | HTTP port |
| `HOST` | `0.0.0.0` | Listen address |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TZ` | `UTC` | Time zone (the browser reports it to sites) |
| `BROWSER_PATH` | `/usr/bin/chromium` | Browser binary. Without it, `chromium`, `google-chrome`, `thorium-browser` are looked up in `PATH` |
| `BROWSER_IDLE_TIMEOUT` | `5m` | Kill the browser after this much idle time (`90s`, `10m`, or minutes as a number) |
| `MAX_CONCURRENCY` | `1` | Requests solved at the same time; the rest wait in a queue |
| `BLOCK_MEDIA` | `true` | Block images, fonts and media (Cloudflare's own challenge files are never blocked). The request field `disableMedia` overrides it |
| `SESSION_TTL` | `30m` | Sessions unused for this long are closed |
| `HEADLESS` | `true` | `false` runs a visible window (needs `DISPLAY`, e.g. Xvfb) |
| `BROWSER_EXTRA_ARGS` | | Extra Chromium switches, space separated. `--name=false` removes a default switch |
| `PROXY_URL`, `PROXY_USERNAME`, `PROXY_PASSWORD` | | Default proxy for requests that have none (same as FlareSolverr) |
| `LANG` | | Browser language, e.g. `ru_RU.UTF-8` (default `en-US`) |
| `LOG_HTML` | `false` | Log the returned HTML at debug level |

## API

`POST /v1` with `cmd` = `request.get`, `request.post`, `sessions.create`,
`sessions.list`, `sessions.destroy`, and the FlareSolverr fields `url`,
`maxTimeout`, `cookies`, `postData`, `session`, `session_ttl_minutes`,
`returnOnlyCookies`, `returnScreenshot`, `waitInSeconds`, `disableMedia`,
`tabs_till_verify`, `proxy` (`{"url", "username", "password"}`). Responses have
the same shape (`status`, `message`, `startTimestamp`, `endTimestamp`,
`version`, `solution{url,status,headers,response,cookies,userAgent}`). Unlike
FlareSolverr, `solution.status` and `solution.headers` are the real values of
the final page. `GET /` and `GET /health` work as in FlareSolverr.

Each request gets its own incognito browser context, closed afterwards. A
per-request proxy is set on that context, so one browser serves many proxies.

## Memory

Measured on the same machine with `docker stats`, solving the managed challenge
at `scrapingcourse.com/cloudflare-challenge` through the same proxy:

| | flaresolverr-go 0.1 | FlareSolverr 3.5.2 |
|---|---|---|
| Idle, browser not running | **2.5 MiB** (Go process RSS ~15 MB) | 53 MiB |
| Peak while solving | 513 MiB | 723 MiB |
| Right after a request | 217 MiB (browser kept for `BROWSER_IDLE_TIMEOUT`) | 110 MiB (Python + Xvfb) |
| After the idle timeout | 9 MiB | 110 MiB |
| Time to solve | 17–19 s | 16 s (and one timeout in three runs) |
| Image size (uncompressed) | 1.15 GB | 1.07 GB |

The browser (Chromium 156) is most of the image and all of the peak. A browser
start takes about 0.5 s, so the idle timeout costs little.

## What matters for Cloudflare

Found by testing against a live challenge:

- **Site isolation must stay on.** With `--disable-site-isolation-trials` the
  Turnstile iframe shares the page process and Cloudflare fails every attempt.
  Other memory switches (`--renderer-process-limit=1`, `--no-zygote`,
  `--disable-gpu`) are harmless.
- `--headless=new` passes; no Xvfb is needed. The user agent and client hints
  are rewritten so they say `Chrome`/`Chromium`, never `HeadlessChrome`.
- The image uses Debian's Chromium because Alpine's build has no SwiftShader,
  so WebGL is missing there; challenges fingerprint WebGL.
- The checkbox is clicked with the mouse first; in tests this is what passes.
  Tab + Space (FlareSolverr's way) is the fallback.
- From a Russian home connection the challenge page loaded blank: the
  challenge scripts never arrived. The same browser through a Dutch proxy
  passed. If pages stay blank, try a proxy.
- QUIC is disabled: it breaks behind small-MTU overlays (Docker, k8s CNIs).

## Thorium

`Dockerfile.thorium` builds the same server on top of
[bropines/thorium-docker](https://github.com/bropines/thorium-docker). It is not
the default, for two reasons:

- Thorium lags Chrome stable (M138 against Chrome 156 at the time of writing).
  Cloudflare compares the version in the user agent with what the engine can
  do, and an old version is a common bot signal.
- The default `AVX2` build crashes on CPUs without AVX2 (some old or cheap
  virtual CPUs). Use the `AVX`/`SSE` tags there.

Stay on the default Chromium image unless you have a reason to switch.

## Limits

- Proxy authentication (`username`/`password`) covers the page itself;
  cross-site iframes run in their own process and may not get the credentials.
  Prefer proxies without auth, or put the credentials in a local forwarder.
- A challenge that asks for a real CAPTCHA is not solved.
- `DEBUG_DUMP_DIR` (debugging only) saves a screenshot and the HTML each time
  the checkbox is tried.

## Development

```sh
go test ./...                          # unit tests
INTEGRATION=1 go test -run Integration ./internal/solver   # real Cloudflare page, needs Chromium
INTEGRATION=1 INTEGRATION_PROXY=socks5://127.0.0.1:1080 go test -run Integration ./internal/solver
```

MIT license.
