package modclient

import (
	"container/list"
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// IntrospectAPI is what an Introspector needs from the can-core client.
type IntrospectAPI interface {
	Introspect(ctx context.Context, token string) (Identity, error)
}

const (
	// TokenPrefix is the prefix of can-core's personal access tokens.
	TokenPrefix       = "canpat_"
	maxTokenLen       = 128
	defaultTTL        = 30 * time.Second
	defaultNegativeTT = 5 * time.Second
	defaultMaxEntries = 1024
)

// TokenShaped reports whether s could be a personal access token: the prefix, URL-safe base64
// characters, bounded length. Anything else is refused locally and never sent to can-core.
func TokenShaped(s string) bool {
	if len(s) <= len(TokenPrefix) || len(s) > maxTokenLen || s[:len(TokenPrefix)] != TokenPrefix {
		return false
	}
	for i := len(TokenPrefix); i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

type entry struct {
	key     [sha256.Size]byte
	id      Identity
	expires time.Time
}

type call struct {
	done chan struct{}
	id   Identity
	err  error
}

// Introspector caches introspection answers: a bounded LRU keyed by the hash of the token (a token
// is never kept), positive answers for TTL but never beyond the token's own expiry, negative
// answers briefly so a replayed bad token does not turn into a call per request. Errors are never
// cached. Concurrent lookups of one token share one call. There is no rate limit here: a valid
// token is answered, always.
type Introspector struct {
	API         IntrospectAPI
	TTL         time.Duration // default 30 s: how long a revocation can take to reach this module
	NegativeTTL time.Duration // default 5 s
	MaxEntries  int           // default 1024
	Now         func() time.Time

	mu       sync.Mutex
	ll       *list.List
	m        map[[sha256.Size]byte]*list.Element
	inflight map[[sha256.Size]byte]*call
}

func (in *Introspector) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// Len is the number of cached answers.
func (in *Introspector) Len() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.ll == nil {
		return 0
	}
	return in.ll.Len()
}

// Lookup resolves a token. A token of the wrong shape is inactive without a call. An error means no
// answer was obtained: the caller must refuse the request.
func (in *Introspector) Lookup(ctx context.Context, token string) (Identity, error) {
	if !TokenShaped(token) {
		return Identity{}, nil
	}
	key := sha256.Sum256([]byte(token))
	now := in.now()
	in.mu.Lock()
	if in.ll == nil {
		in.ll, in.m, in.inflight = list.New(), map[[sha256.Size]byte]*list.Element{}, map[[sha256.Size]byte]*call{}
	}
	if e, ok := in.m[key]; ok {
		en := e.Value.(*entry)
		if now.Before(en.expires) {
			in.ll.MoveToFront(e)
			id := en.id
			in.mu.Unlock()
			return id, nil
		}
		in.ll.Remove(e)
		delete(in.m, key)
	}
	if c, ok := in.inflight[key]; ok {
		in.mu.Unlock()
		select {
		case <-c.done:
			return c.id, c.err
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	in.inflight[key] = c
	in.mu.Unlock()

	c.id, c.err = in.API.Introspect(ctx, token)

	in.mu.Lock()
	delete(in.inflight, key)
	if c.err == nil {
		in.put(key, c.id, now)
	}
	in.mu.Unlock()
	close(c.done)
	return c.id, c.err
}

// Fresh asks now, whatever is cached, for a check that must not be up to TTL old (the step-up before
// an irreversible action). The answer replaces the cached one; an error means no answer.
func (in *Introspector) Fresh(ctx context.Context, token string) (Identity, error) {
	if !TokenShaped(token) {
		return Identity{}, nil
	}
	key := sha256.Sum256([]byte(token))
	now := in.now()
	id, err := in.API.Introspect(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.ll == nil {
		in.ll, in.m, in.inflight = list.New(), map[[sha256.Size]byte]*list.Element{}, map[[sha256.Size]byte]*call{}
	}
	if e, ok := in.m[key]; ok { // an inactive answer with no negative TTL must not leave the old one
		in.ll.Remove(e)
		delete(in.m, key)
	}
	in.put(key, id, now)
	return id, nil
}

func (in *Introspector) put(key [sha256.Size]byte, id Identity, now time.Time) {
	ttl := in.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	exp := now.Add(ttl)
	if !id.Active {
		nt := in.NegativeTTL
		if nt <= 0 {
			nt = defaultNegativeTT
		}
		exp = now.Add(nt)
	} else if !id.ExpiresAt.IsZero() && id.ExpiresAt.Before(exp) {
		exp = id.ExpiresAt
	}
	if !now.Before(exp) {
		return
	}
	if e, ok := in.m[key]; ok {
		in.ll.Remove(e)
	}
	in.m[key] = in.ll.PushFront(&entry{key: key, id: id, expires: exp})
	max := in.MaxEntries
	if max <= 0 {
		max = defaultMaxEntries
	}
	for in.ll.Len() > max {
		last := in.ll.Back()
		in.ll.Remove(last)
		delete(in.m, last.Value.(*entry).key)
	}
}

// idSet is a bounded set of recently seen event ids (oldest evicted first).
type idSet struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[string]*list.Element
}

func newIDSet(max int) *idSet { return &idSet{max: max, ll: list.New(), m: map[string]*list.Element{}} }

func (s *idSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[id]
	return ok
}

func (s *idSet) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; ok {
		return
	}
	s.m[id] = s.ll.PushFront(id)
	for s.ll.Len() > s.max {
		last := s.ll.Back()
		s.ll.Remove(last)
		delete(s.m, last.Value.(string))
	}
}
