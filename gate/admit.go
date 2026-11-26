package gate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Refusal is a request turned away before it reaches the model, as the
// visitor is told: its HTTP status and a code the page words
type Refusal struct {
	Status int
	Code   string
}

func (r *Refusal) Error() string { return r.Code }

// ErrText is the refusal of a text Clean will not take
var ErrText = &Refusal{http.StatusBadRequest, "text_invalid"}

// Hidden is what hides text from a reader but not from a model: format
// characters (zero-width, bidi, the tag block), variation selectors, private
// use and controls
func Hidden(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Co, unicode.Variation_Selector) || unicode.IsControl(r)
}

// Clean keeps letters, marks (4 in a row at most), numbers, punctuation,
// symbols, single spaces and at most maxLines lines, drops what is Hidden,
// and breaks the markup a model reads as its own: the <| |> of a chat
// template's special tokens, then <<< and >>>, which fence a prompt's text
func Clean(s string, maxRunes, maxLines int) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrText
	}
	var b []byte
	lines, marks := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.M, r) {
			if marks++; marks > 4 {
				continue
			}
		} else if !Hidden(r) {
			marks = 0
		}
		switch {
		case r == '\n':
			if lines++; lines >= maxLines {
				return "", ErrText
			}
			b = append(bytes.TrimRight(b, " "), '\n')
		case unicode.IsSpace(r):
			if len(b) > 0 && b[len(b)-1] != ' ' && b[len(b)-1] != '\n' {
				b = append(b, ' ')
			}
		case Hidden(r):
		case unicode.IsPrint(r):
			b = utf8.AppendRune(b, r)
		}
	}
	out := strings.TrimSpace(string(b))
	for strings.Contains(out, "<|") || strings.Contains(out, "|>") { // <|| is <| again after one pass
		out = strings.NewReplacer("<|", "<", "|>", ">").Replace(out)
	}
	out = fenceRE.ReplaceAllStringFunc(out, func(m string) string { return m[:2] })
	if n := utf8.RuneCountInString(out); n == 0 || n > maxRunes {
		return "", ErrText
	}
	return out, nil
}

var fenceRE = regexp.MustCompile(`<{3,}|>{3,}`)

// Limits are a pace and a day's cap: a burst of requests and the time one
// more comes back, no pace at a burst of 0, and requests a UTC day, no cap
// at 0
type Limits struct {
	Burst  int           `toml:"burst"`
	Refill time.Duration `toml:"refill"`
	Daily  int           `toml:"daily"`
}

// Admission is an app's limits: each client's own, and the app's across its
// clients, so that a crowd of first requests cannot hold the model
type Admission struct {
	Client Limits `toml:"client"`
	App    Limits `toml:"app"`
}

// Check refuses a pace that never refills
func (a Admission) Check() error {
	for name, l := range map[string]Limits{"client": a.Client, "app": a.App} {
		if l.Burst > 0 && l.Refill <= 0 {
			return fmt.Errorf("limits.%s: a burst needs a refill above 0", name)
		}
	}
	return nil
}

// The limits' refusals: a client's own are 429, the app's 503
var (
	clientPaced = &Refusal{http.StatusTooManyRequests, "rate_limited"}
	clientSpent = &Refusal{http.StatusTooManyRequests, "daily_limit"}
	appPaced    = &Refusal{http.StatusServiceUnavailable, "app_rate_limited"}
	appSpent    = &Refusal{http.StatusServiceUnavailable, "app_daily_limit"}
)

// limiter keeps a token bucket and the day's count a key, a client's address
// or "" for the app as a whole; paced and spent are its refusals
type limiter struct {
	Limits
	paced, spent *Refusal
	mu           sync.Mutex
	buckets      map[string]*bucket
	day          string
}

type bucket struct {
	tokens float64
	last   time.Time
	today  int
}

func newLimiter(l Limits, paced, spent *Refusal) *limiter {
	return &limiter{Limits: l, paced: paced, spent: spent, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string, now time.Time) *Refusal {
	l.mu.Lock()
	defer l.mu.Unlock()
	// a new day starts every count again: the map holds the day's keys
	if day := now.UTC().Format(time.DateOnly); day != l.day {
		l.day = day
		clear(l.buckets)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(l.Burst), last: now}
		l.buckets[key] = b
	}
	if l.Daily > 0 && b.today >= l.Daily {
		return l.spent
	}
	if l.Burst > 0 {
		b.tokens = min(float64(l.Burst), b.tokens+now.Sub(b.last).Seconds()/l.Refill.Seconds())
		b.last = now
		if b.tokens < 1 {
			return l.paced
		}
		b.tokens--
	}
	b.today++
	return nil
}

// refund gives back a request refused after this limiter took it: it never
// reached the model
func (l *limiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[key]; b != nil {
		b.tokens = min(float64(l.Burst), b.tokens+1)
		if b.today = max(0, b.today-1); b.today == 0 {
			delete(l.buckets, key) // as if never seen: the map holds only keys that spent
		}
	}
}

// clientKey is who a request counts against: the address a trusted proxy
// sent in X-Real-IP, else the peer's; an IPv6 one by its /64, which
// one host may roam
func clientKey(r *http.Request, proxies []netip.Addr) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	addr := peer
	if real, err := netip.ParseAddr(r.Header.Get("X-Real-IP")); err == nil && slices.Contains(proxies, peer) {
		addr = real.Unmap()
	}
	if addr.Is6() {
		p, _ := addr.Prefix(64)
		return p.String()
	}
	return addr.String()
}

// pseudonym names a client in the review log: stable within a UTC day and
// unlinkable to its address without the key, which never leaves memory
func pseudonym(key []byte, client string, now time.Time) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(now.UTC().Format(time.DateOnly) + "|" + client))
	return hex.EncodeToString(m.Sum(nil)[:6])
}
