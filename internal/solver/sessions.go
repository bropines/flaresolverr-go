package solver

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// Session is a persistent browser context with one tab. Requests that name a
// session reuse its cookies; they run one at a time.
type Session struct {
	sync.Mutex
	ID      string
	created time.Time
	lastUse atomic.Int64 // unix nanos
	ctx     context.Context
	cancel  context.CancelFunc
	inst    *instance
	proxy   *Proxy
}

func (s *Session) touch() { s.lastUse.Store(time.Now().UnixNano()) }

func (s *Session) idleSince(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, s.lastUse.Load()))
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// CreateSession creates a session (idempotent). It returns false when the
// session already existed.
func (m *Manager) CreateSession(id string, proxy *Proxy) (string, bool, error) {
	if id == "" {
		id = newUUID()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok && s.ctx.Err() == nil {
		return id, false, nil
	}
	if _, err := m.createSessionLocked(id, proxy); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (m *Manager) createSessionLocked(id string, proxy *Proxy) (*Session, error) {
	inst, err := m.ensure()
	if err != nil {
		return nil, err
	}
	ctx, cancel := chromedp.NewContext(inst.ctx, chromedp.WithNewBrowserContext(proxy.contextOption()))
	if err := chromedp.Do(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("cannot create browser context: %w", err)
	}
	s := &Session{ID: id, created: time.Now(), ctx: ctx, cancel: cancel, inst: inst, proxy: proxy}
	s.touch()
	m.sessions[id] = s
	m.log.Debug("session created", "session", id)
	return s, nil
}

// getSession returns the session, creating it when missing or when it is
// older than ttl (ttl 0 means no limit), the same as FlareSolverr.
func (m *Manager) getSession(id string, proxy *Proxy, ttl time.Duration) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		if s.ctx.Err() == nil && s.inst == m.inst && (ttl <= 0 || time.Since(s.created) <= ttl) {
			return s, nil
		}
		m.log.Debug("session recreated", "session", id)
		s.cancel()
		delete(m.sessions, id)
	}
	return m.createSessionLocked(id, proxy)
}

// DestroySession closes a session. It returns false when it did not exist.
func (m *Manager) DestroySession(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		m.lastUse = time.Now()
	}
	m.mu.Unlock()
	if ok {
		s.cancel()
	}
	return ok
}

// SessionIDs lists the current sessions.
func (m *Manager) SessionIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
