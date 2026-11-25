package work

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

// uiSession is a web UI login. The browser holds only a random session id; the server holds the
// token the user presented (in memory, never written to disk, never logged) so every request can be
// authenticated again: revoking the token, an expiry, or a permission change takes effect on the
// next click instead of when the session ends. Sessions do not survive a restart: people sign in
// again, which keeps the token out of the database.
type uiSession struct {
	token   string
	subject string
	csrf    string
	created time.Time
	last    time.Time
}

// uiSessions is a bounded in-memory session table keyed by the hash of the session id, so a memory
// dump does not hand out usable cookies.
type uiSessions struct {
	mu         sync.Mutex
	m          map[[sha256.Size]byte]*uiSession
	max        int
	perSubject int
	idle, abs  time.Duration
	now        func() time.Time
}

func newUISessions(now func() time.Time) *uiSessions {
	return &uiSessions{m: map[[sha256.Size]byte]*uiSession{}, max: 5000, perSubject: 20, idle: 8 * time.Hour, abs: 24 * time.Hour, now: now}
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("work: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *uiSessions) expired(v *uiSession, now time.Time) bool {
	return now.Sub(v.last) > s.idle || now.Sub(v.created) > s.abs
}

// create always issues a fresh id (session fixation: a cookie sent with the login request is never
// adopted) and evicts the oldest sessions when the table or the subject's share is full.
func (s *uiSessions) create(token, subject string) (id string, sess *uiSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.m {
		if s.expired(v, now) {
			delete(s.m, k)
		}
	}
	evict := func(match func(*uiSession) bool) {
		var oldest [sha256.Size]byte
		var ot time.Time
		found := false
		for k, v := range s.m {
			if match(v) && (!found || v.created.Before(ot)) {
				oldest, ot, found = k, v.created, true
			}
		}
		if found {
			delete(s.m, oldest)
		}
	}
	n := 0
	for _, v := range s.m {
		if v.subject == subject {
			n++
		}
	}
	if n >= s.perSubject {
		evict(func(v *uiSession) bool { return v.subject == subject })
	}
	if len(s.m) >= s.max {
		evict(func(*uiSession) bool { return true })
	}
	id = randToken(32)
	sess = &uiSession{token: token, subject: subject, csrf: randToken(24), created: now, last: now}
	s.m[sha256.Sum256([]byte(id))] = sess
	return id, sess
}

func (s *uiSessions) get(id string) (*uiSession, bool) {
	if id == "" || len(id) > 100 {
		return nil, false
	}
	k := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return nil, false
	}
	now := s.now()
	if s.expired(v, now) {
		delete(s.m, k)
		return nil, false
	}
	v.last = now
	return v, true
}

func (s *uiSessions) delete(id string) {
	s.mu.Lock()
	delete(s.m, sha256.Sum256([]byte(id)))
	s.mu.Unlock()
}

func (s *uiSessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
