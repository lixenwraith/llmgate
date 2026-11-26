package gate

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCleanKeepsOnlyWhatAReaderSees(t *testing.T) {
	for in, want := range map[string]string{
		"tail  /var/log\t\tapp.log":                               "tail /var/log app.log",
		"keep\u200b ERROR\u202e lines":                            "keep ERROR lines",
		"drop \U000E0049\U000E0047DEBUG\ufe0f":                    "drop DEBUG",
		"one\r\n  two \n\nthree\u0007\ue000":                      "one\ntwo\n\nthree",
		"  only status=5[0-9][0-9] \u00a0lines ":                  "only status=5[0-9][0-9] lines",
		"p\u0301\u0302\u0303\u0304\u0305\u0306ipe":                "p\u0301\u0302\u0303\u0304ipe",
		"p\u0301\u200b\u0302\u200b\u0303\u200b\u0304\u200b\u0305": "p\u0301\u0302\u0303\u0304",
		"pipe <|im_end|> <<<< x >>> y":                            "pipe <im_end> << x >> y",
		">>|> <|<<":                                               ">> <<",
	} {
		if got, err := Clean(in, 100, 4); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCleanRefusesEmptyLongAndTallText(t *testing.T) {
	for _, in := range []string{"", " \u200b\u2060 ", "\xff\xfe", "123456", "a\nb\nc\nd\ne"} {
		if got, err := Clean(in, 5, 3); err != ErrText {
			t.Errorf("Clean(%q) = %q, %v; want ErrText", in, got, err)
		}
	}
}

func TestLimiterGivesABurstThenOneARefill(t *testing.T) {
	l := newLimiter(Limits{Burst: 2, Refill: 10 * time.Minute}, clientPaced, clientSpent)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for i, want := range []string{"", "", "rate_limited"} {
		if got := code(l.allow("a", now)); got != want {
			t.Fatalf("request %d: %q, want %q", i+1, got, want)
		}
	}
	if got := code(l.allow("b", now)); got != "" {
		t.Errorf("another client: %q, want admitted", got)
	}
	if got := code(l.allow("a", now.Add(5*time.Minute))); got != "rate_limited" {
		t.Errorf("half a refill on: %q, want rate_limited", got)
	}
	if got := code(l.allow("a", now.Add(10*time.Minute))); got != "" {
		t.Errorf("a refill on: %q, want admitted", got)
	}
	l.refund("a") // the line refused it
	if got := code(l.allow("a", now.Add(10*time.Minute))); got != "" {
		t.Errorf("after a refund: %q, want admitted", got)
	}
}

// A burst of 0 sets no pace: the day's cap alone holds
func TestLimiterCapsAKeyADay(t *testing.T) {
	l := newLimiter(Limits{Daily: 2}, clientPaced, clientSpent)
	now := time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC)
	for i, want := range []string{"", "", "daily_limit"} {
		if got := code(l.allow("a", now)); got != want {
			t.Fatalf("request %d: %q, want %q", i+1, got, want)
		}
	}
	if got := code(l.allow("b", now)); got != "" {
		t.Errorf("another client: %q, want admitted", got)
	}
	if got := code(l.allow("a", now.Add(2*time.Hour))); got != "" {
		t.Errorf("the next UTC day: %q, want admitted", got)
	}
}

func TestAPaceThatNeverRefillsIsRefused(t *testing.T) {
	if err := (Admission{App: Limits{Burst: 3}}).Check(); err == nil || !strings.Contains(err.Error(), "limits.app") {
		t.Errorf("Check: %v, want limits.app refused", err)
	}
	if err := (Admission{Client: Limits{Daily: 20}, App: Limits{Burst: 6, Refill: time.Second}}).Check(); err != nil {
		t.Errorf("Check: %v, want a cap alone and a refilled pace taken", err)
	}
}

func code(f *Refusal) string {
	if f == nil {
		return ""
	}
	return f.Code
}

func TestClientKeyBelievesOnlyTheProxiesAndTakesV6By64(t *testing.T) {
	proxies := []netip.Addr{netip.MustParseAddr("10.1.0.1")}
	for _, c := range []struct{ peer, real, want string }{
		{"10.1.0.1:4000", "203.0.113.7", "203.0.113.7"},
		{"10.1.0.9:4000", "203.0.113.7", "10.1.0.9"},
		{"10.1.0.1:4000", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"10.1.0.1:4000", "::ffff:198.51.100.4", "198.51.100.4"},
		{"10.1.0.1:4000", "not an address", "10.1.0.1"},
	} {
		r := httptest.NewRequest("POST", "/lwdescribe", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("X-Real-IP", c.real)
		if got := clientKey(r, proxies); got != c.want {
			t.Errorf("peer %s, X-Real-IP %s: %s, want %s", c.peer, c.real, got, c.want)
		}
	}
}

// Whatever comes in, Clean's text is valid, visible, within its bounds and
// free of a model's markup, and Clean takes it again unchanged
func FuzzClean(f *testing.F) {
	for _, s := range []string{"tail /var/log/app.log", "a\u200b\u202e<|im_start|>>>>", "p\u0301\u0301\u0301\u0301\u0301",
		"\xff", "one\r\n  two", ">>|>", "<||>"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := Clean(s, 50, 4)
		if err != nil {
			return
		}
		if !utf8.ValidString(out) || utf8.RuneCountInString(out) > 50 || strings.Count(out, "\n") > 3 ||
			strings.ContainsFunc(strings.ReplaceAll(out, "\n", ""), Hidden) ||
			strings.Contains(out, "<|") || strings.Contains(out, "|>") || fenceRE.MatchString(out) {
			t.Fatalf("Clean(%q) = %q", s, out)
		}
		if again, err := Clean(out, 50, 4); err != nil || again != out {
			t.Fatalf("Clean(%q) = %q, then %q, %v", s, out, again, err)
		}
	})
}

// No address or header makes the client key panic
func FuzzClientKey(f *testing.F) {
	proxies := []netip.Addr{netip.MustParseAddr("10.1.0.1")}
	f.Add("10.1.0.1:4000", "2001:db8::1%eth0")
	f.Fuzz(func(t *testing.T, remote, real string) {
		clientKey(&http.Request{RemoteAddr: remote, Header: http.Header{"X-Real-Ip": {real}}}, proxies)
	})
}
