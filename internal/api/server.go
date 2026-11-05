package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bropines/flaresolverr-go/internal/config"
	"github.com/bropines/flaresolverr-go/internal/solver"
)

const defaultMaxTimeout = 60000

// Solver is what the API needs from the browser side (mocked in tests).
type Solver interface {
	Solve(ctx context.Context, req *solver.Request) (*solver.Result, error)
	CreateSession(id string, proxy *solver.Proxy) (string, bool, error)
	DestroySession(id string) bool
	SessionIDs() []string
	UserAgent() string
}

type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	solver  Solver
	version string
	now     func() time.Time
}

func NewServer(cfg *config.Config, log *slog.Logger, s Solver, version string) *Server {
	return &Server{cfg: cfg, log: log, solver: s, version: version, now: time.Now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1", s.v1)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Not found: '" + r.URL.Path + "'", "status_code": 404})
	})
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, IndexResponse{Msg: "FlareSolverr is ready!", Version: s.version, UserAgent: s.solver.UserAgent()})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// apiError becomes {"status":"error","message":"Error: ..."} with HTTP 500.
type apiError struct{ msg string }

func (e apiError) Error() string { return e.msg }

func errorf(format string, a ...any) error { return apiError{fmt.Sprintf(format, a...)} }

func (s *Server) v1(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	var req V1Request
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err == nil && len(strings.TrimSpace(string(body))) > 0 {
		err = json.Unmarshal(body, &req)
	}

	var res *V1Response
	if err != nil {
		err = errorf("Invalid JSON body: %v", err)
	} else {
		s.applyEnvProxy(&req)
		s.log.Info("incoming request", "body", req.redacted())
		res, err = s.handle(r.Context(), &req)
	}

	code := http.StatusOK
	if err != nil {
		code = http.StatusInternalServerError
		res = &V1Response{Status: "error", Message: "Error: " + err.Error()}
		s.log.Error(res.Message)
	}
	res.StartTimestamp = start.UnixMilli()
	res.EndTimestamp = s.now().UnixMilli()
	res.Version = s.version
	s.log.Info("response", "status", res.Status, "took", time.Duration(res.EndTimestamp-res.StartTimestamp)*time.Millisecond)
	writeJSON(w, code, res)
}

// applyEnvProxy uses PROXY_URL/PROXY_USERNAME/PROXY_PASSWORD when the request
// has no proxy, as FlareSolverr does.
func (s *Server) applyEnvProxy(req *V1Request) {
	if (req.Proxy == nil || req.Proxy.URL == "") && s.cfg.ProxyURL != "" {
		req.Proxy = &ProxyIn{URL: s.cfg.ProxyURL, Username: s.cfg.ProxyUsername, Password: s.cfg.ProxyPassword}
	}
}

func (s *Server) handle(ctx context.Context, req *V1Request) (*V1Response, error) {
	if req.Cmd == "" {
		return nil, errorf("Request parameter 'cmd' is mandatory.")
	}
	if req.Headers != nil {
		s.log.Warn("Request parameter 'headers' was removed in FlareSolverr v2.")
	}
	if req.UserAgent != nil {
		s.log.Warn("Request parameter 'userAgent' was removed in FlareSolverr v2.")
	}
	maxTimeout := defaultMaxTimeout
	if req.MaxTimeout.set && req.MaxTimeout.v >= 1 {
		maxTimeout = req.MaxTimeout.v
	}

	switch req.Cmd {
	case "sessions.create":
		id, fresh, err := s.solver.CreateSession(req.Session, s.proxy(req))
		if err != nil {
			return nil, err
		}
		msg := "Session created successfully."
		if !fresh {
			msg = "Session already exists."
		}
		return &V1Response{Status: "ok", Message: msg, Session: id}, nil
	case "sessions.list":
		ids := s.solver.SessionIDs()
		return &V1Response{Status: "ok", Message: "", Sessions: &ids}, nil
	case "sessions.destroy":
		if !s.solver.DestroySession(req.Session) {
			return nil, errorf("The session doesn't exist.")
		}
		return &V1Response{Status: "ok", Message: "The session has been removed."}, nil
	case "request.get":
		if req.URL == "" {
			return nil, errorf("Request parameter 'url' is mandatory in 'request.get' command.")
		}
		if req.PostData != nil {
			return nil, errorf("Cannot use 'postBody' when sending a GET request.")
		}
		return s.request(ctx, req, "GET", maxTimeout)
	case "request.post":
		if req.PostData == nil {
			return nil, errorf("Request parameter 'postData' is mandatory in 'request.post' command.")
		}
		if req.URL == "" {
			return nil, errorf("Request parameter 'url' is mandatory in 'request.post' command.")
		}
		return s.request(ctx, req, "POST", maxTimeout)
	}
	return nil, errorf("Request parameter 'cmd' = '%s' is invalid.", req.Cmd)
}

func (s *Server) proxy(req *V1Request) *solver.Proxy {
	if req.Proxy == nil {
		return nil
	}
	return solver.ParseProxy(req.Proxy.URL, req.Proxy.Username, req.Proxy.Password)
}

// BuildRequest maps a v1 request to a solver request.
func (s *Server) BuildRequest(req *V1Request, method string) *solver.Request {
	sr := &solver.Request{
		Method:            method,
		URL:               req.URL,
		Proxy:             s.proxy(req),
		Session:           req.Session,
		ReturnOnlyCookies: req.ReturnOnlyCookies,
		ReturnScreenshot:  req.ReturnScreenshot,
		WaitInSeconds:     req.WaitInSeconds.v,
		BlockMedia:        req.DisableMedia,
	}
	if req.PostData != nil {
		sr.PostData = *req.PostData
	}
	if req.SessionTTLMinutes.set && req.SessionTTLMinutes.v > 0 {
		sr.SessionTTL = time.Duration(req.SessionTTLMinutes.v) * time.Minute
	}
	if req.TabsTillVerify.set {
		n := req.TabsTillVerify.v
		sr.TabsTillVerify = &n
	}
	for _, c := range req.Cookies {
		sr.Cookies = append(sr.Cookies, c.toParam())
	}
	return sr
}

func (s *Server) request(ctx context.Context, req *V1Request, method string, maxTimeout int) (*V1Response, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxTimeout)*time.Millisecond)
	defer cancel()
	res, err := s.solver.Solve(ctx, s.BuildRequest(req, method))
	if err != nil {
		if errors.Is(err, solver.ErrTimeout) {
			return nil, errorf("Error solving the challenge. Timeout after %s seconds.", pySeconds(maxTimeout))
		}
		return nil, errorf("Error solving the challenge. %s", strings.ReplaceAll(err.Error(), "\n", "\\n"))
	}
	return &V1Response{Status: "ok", Message: res.Message, Solution: toSolution(res, req.ReturnOnlyCookies)}, nil
}

// pySeconds formats ms/1000 the way Python prints a float (60000 -> "60.0").
func pySeconds(ms int) string {
	s := strconv.FormatFloat(float64(ms)/1000, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
