package lwdescribe

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lixenwraith/llmgate/gate"
	"github.com/lixenwraith/llmgate/model"
)

// lw --schema of a LogWisp release (make testdata) and the jail's assets:
// the language, the prompt and the examples the gate starts with
const (
	schemaFile = "testdata/schema.json"
	assets     = "../../jail/usr/local/etc/llmgate/lwdescribe"
)

func testLanguage(t testing.TB) *language {
	t.Helper()
	data, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	l, err := newLanguage(data)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The examples and the eval cases are drafts the model may write and settle
// leaves as they are; with LW set to an lw binary, held also puts each to
// lw --dump.
func TestExamplesAreDraftsTheModelMayWriteAndLWTakes(t *testing.T) {
	l := testLanguage(t)
	var model []*regexp.Regexp
	for _, p := range l.patterns {
		model = append(model, regexp.MustCompile(p.json))
	}
	for _, name := range []string{"examples.toml", "eval.toml"} {
		entries, err := readExamples(filepath.Join(assets, name))
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range entries {
			if p := l.check(e.Flags); len(e.Flags) > 0 && p != "" {
				t.Errorf("%s %d: %s", name, i+1, p)
			}
			if settled, n := l.settle(e.Flags, e.Text); n > 0 {
				t.Errorf("%s %d is what settle would change: %v", name, i+1, settled)
			}
			for _, f := range e.Flags {
				if !slices.ContainsFunc(model, func(re *regexp.Regexp) bool { return re.MatchString(f) }) {
					t.Errorf("%s %d: the model's patterns refuse %s", name, i+1, f)
				}
			}
		}
		if lw := os.Getenv("LW"); lw != "" {
			cfg := Defaults()
			cfg.LW, cfg.Assets, cfg.WorkDir = lw, assets, t.TempDir()
			app, err := New(cfg)
			if err == nil {
				err = app.held(name, entries)
			}
			if err != nil {
				t.Errorf("%s with %s: %v", name, lw, err)
			}
		}
	}
}

// Eval passes a draft that differs in a pipeline's name alone and a
// decline asked for, and fails a draft with other flags
func TestEvalComparesDraftsPipelineNamesAside(t *testing.T) {
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(t), assets, t.TempDir()
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "eval.toml")
	os.WriteFile(path, []byte(`[[example]]
text = "pipe to the terminal"
flags = ["--pipeline 'a'", "--source 'console'", "--sink 'console'"]

[[example]]
text = "pipe to tcp locally"
flags = ["--pipeline 'b'", "--source 'console'", "--sink 'tcp:port=9090,host=127.0.0.1'"]

[[example]]
text = "a poem"
flags = []
`), 0o644)
	m := &scripted{answers: []string{`{"flags":[]}`, // the warm-up
		`{"flags":["--pipeline 'other'","--source 'console'","--sink 'console'"]}`,
		`{"flags":["--pipeline 'b'","--source 'console'","--sink 'console'"]}`,
		`{"flags":[]}`}}
	var out strings.Builder
	if err := app.Eval(context.Background(), m, path, time.Minute, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2/3 pass") || !strings.Contains(out.String(), "  2 FAIL") {
		t.Errorf("eval wrote\n%s", out.String())
	}
}

func TestTheLanguageRefusesWhatLWWouldMisread(t *testing.T) {
	l := testLanguage(t)
	for _, f := range []string{
		"--sink 'tcp:port=9090,host=0.0.0.0,max_connections=5,tls.enabled=true,acl.allow=10.0.0.0/8,acl.allow=::1/128'",
		"--source 'random:format=json'",
		"--filter 'exclude:patterns=level=(DEBUG|TRACE),logic=and'",
		"--format 'json'",
		"--heartbeat 'interval_ms=5000,include_stats=true'",
	} {
		if p := l.check([]string{"--pipeline 'a'", f}); p != "" {
			t.Errorf("%s: %s", f, p)
		}
	}
	for _, f := range []string{
		"--sink 'http:host=127.0.0.1,port=8080'",                            // keys out of order
		"--sink 'http:port=8080'",                                           // a listener without its host
		"--sink 'tcp:port=9090,host=::,tls.enabled=true,max_connections=5'", // a table before own keys
		"--source 'file:pattern=app.log'",                                   // no directory
		"--filter 'include:patterns=a,b'",                                   // a comma inside a value
		"--source 'file:directory=/var/log,pattern=a\\b'",                   // a backslash
		"--source 'file:directory=/var/log/it's'",                           // a quote
		"--sink 'console:color=maybe'",                                      // not one of the enum
		"--source 'kafka:topic=logs'",                                       // no such type
		"--source 'file:directory=/var/log\u200b'",                          // a hidden character
		"--source 'console' --sink 'console'",                               // two flags on a line
	} {
		if p := l.check([]string{"--pipeline 'a'", f}); p == "" {
			t.Errorf("%s: taken", f)
		}
	}
	for _, first := range []string{"--source 'console'", "--pipeline 'App'"} {
		if p := l.check([]string{first, "--sink 'console'"}); p == "" {
			t.Errorf("first line %s: taken", first)
		}
	}
}

// fakeLW writes an lw that takes every --dump but one with port=1, which it
// refuses as lw words a problem
func fakeLW(t testing.TB) string {
	t.Helper()
	schema, err := filepath.Abs(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	return shLW(t, `case "$1" in
--version) echo "test (commit: test)" ;;
--schema) exec /bin/cat '`+schema+`' ;;
--dump) case "$*" in *port=1,*) printf 'Error: Failed to load config: invalid configuration:\npipelines[0].sinks[0].port: 1 is not a port to listen on\n' >&2; exit 1 ;; esac ;;
esac`)
}

// shLW writes an lw that is the shell script body
func shLW(t testing.TB, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lw")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Only lw's own refusal, exit 1 with its words, is a verdict on flags: a
// silent failure, a crash or a kill is an error, never flags taken
func TestOnlyLWsOwnRefusalIsAVerdict(t *testing.T) {
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(t), assets, t.TempDir()
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	flags := []string{"--pipeline 'a'", "--source 'console'", "--sink 'console'"}
	for _, body := range []string{"exit 1", "echo 'panic: runtime error' >&2; exit 2", "exec sleep 5"} {
		app.cfg.LW = shLW(t, body)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		if p, err := app.dump(ctx, flags); err == nil {
			t.Errorf("lw %q: problems %q and no error", body, p)
		}
		cancel()
	}
}

// A draft the check refuses for a hidden character reaches the page and the
// review log escaped, for no terminal to act on
func TestAHiddenCharacterIsKeptEscaped(t *testing.T) {
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(t), assets, t.TempDir()
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const draft = `{"flags":["--pipeline 'a'","--source 'console'","--sink 'file:directory=/tmp\u001b]0;x\u0007'"]}`
	task, err := app.Take([]byte(`{"text":"stdin to a file"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := task.Run(context.Background(), &scripted{answers: []string{draft, draft}})
	if err != nil {
		t.Fatal(err)
	}
	rec := task.Record()
	drafts, _ := rec["refused_drafts"].([]map[string]string)
	if len(drafts) != 1 {
		t.Fatalf("refused drafts %v", drafts)
	}
	for _, kept := range []string{out["flags"].(string), rec["flags"].(string), drafts[0]["flags"]} {
		if strings.ContainsFunc(strings.ReplaceAll(kept, "\n", ""), gate.Hidden) || !strings.Contains(kept, `\x1b`) {
			t.Errorf("kept %q", kept)
		}
	}
}

// scripted answers each chat with its next answer and keeps the requests
type scripted struct {
	answers []string
	got     []model.Request
}

func (s *scripted) Name() string                    { return "scripted" }
func (s *scripted) Ready(ctx context.Context) error { return nil }
func (s *scripted) Chat(ctx context.Context, r model.Request) (model.Answer, error) {
	s.got = append(s.got, r)
	a := s.answers[0]
	s.answers = s.answers[1:]
	return model.Answer{Content: a, PromptTokens: 100, AnswerTokens: 20}, nil
}

func TestRunRepairsWithLWsWordsThenGivesUp(t *testing.T) {
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(t), assets, t.TempDir()
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const (
		refused = `{"flags":["--pipeline 'a'","--source 'console'","--sink 'http:port=1,host=127.0.0.1'"]}`
		taken   = `{"flags":["--pipeline 'a'","--source 'console'","--sink 'http:port=8080,host=127.0.0.1'"]}`
		strange = `{"flags":["--pipeline 'a'","--sink 'http:host=127.0.0.1,port=8080'"]}`
	)
	for _, c := range []struct {
		name     string
		answers  []string
		problems string // in the result, and in the repair turn when there is one
		repaired string
	}{
		{"lw's words repair", []string{refused, taken}, "", "1 is not a port to listen on"},
		{"the language's words repair", []string{strange, taken}, "", "not a flag the catalog allows"},
		{"one repair at most", []string{refused, refused}, "1 is not a port to listen on", "1 is not a port to listen on"},
		{"an empty answer declines", []string{`{"flags":[]}`}, "", ""},
	} {
		m := &scripted{answers: c.answers}
		task, err := app.Take([]byte(`{"text":"stdin to the browser viewer"}`))
		if err != nil {
			t.Fatal(err)
		}
		out, err := task.Run(context.Background(), m)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(m.answers) != 0 {
			t.Errorf("%s: %d answers left unasked", c.name, len(m.answers))
		}
		if c.repaired != "" {
			turn := m.got[1].Messages[len(m.got[1].Messages)-1].Content
			if !strings.Contains(turn, c.repaired) || strings.Contains(turn, "Failed to load config") {
				t.Errorf("%s: repair turn %q", c.name, turn)
			}
			if drafts, _ := task.Record()["refused_drafts"].([]map[string]string); len(drafts) != 1 || !strings.Contains(drafts[0]["problems"], c.repaired) {
				t.Errorf("%s: the review record keeps %v", c.name, drafts)
			}
		}
		if c.answers[0] == `{"flags":[]}` {
			if out["declined"] != true {
				t.Errorf("%s: %v", c.name, out)
			}
			continue
		}
		if p, _ := out["problems"].(string); !strings.Contains(p, c.problems) || (c.problems == "") != (p == "") {
			t.Errorf("%s: problems %q, want %q", c.name, p, c.problems)
		}
	}
}

func TestNewRefusesAnExampleLWRefuses(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"prompt.md", "guide.md"} {
		data, _ := os.ReadFile(filepath.Join(assets, name))
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "examples.toml"), []byte(`[[example]]
text = "privileged"
flags = ["--pipeline 'a'", "--source 'console'", "--sink 'http:port=1,host=127.0.0.1'"]
`), 0o644)
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(t), dir, t.TempDir()
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "example 1") {
		t.Errorf("New: %v, want example 1 refused", err)
	}
}

// The guide is the model's own prompt and examples, its answers lw's flags
// a line each, which the page's paste box reads
func TestTheGuideTeachesTheLanguageAsLines(t *testing.T) {
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(t), assets, t.TempDir()
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	guide := string(app.Guide())
	examples, _ := readExamples(filepath.Join(assets, "examples.toml"))
	for _, want := range append([]string{"LogWisp " + app.lang.version, app.lang.catalog, guideAnswer, guideDecline},
		strings.Join(examples[0].Flags, "\n"), "Description: "+examples[len(examples)-1].Text+"\n"+guideDecline) {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
	for _, not := range []string{`{"flags"`, "{{"} {
		if strings.Contains(guide, not) {
			t.Errorf("the guide holds %q", not)
		}
	}
	if !strings.Contains(app.prefix[0].Content, gateAnswer) || strings.Contains(app.prefix[0].Content, guideAnswer) {
		t.Error("the model's own prompt asks for the guide's answer")
	}
}

// settle applies the defaults with one answer and leaves every other line
func TestSettleAppliesTheDefaultsWithOneAnswer(t *testing.T) {
	l := testLanguage(t)
	for _, c := range []struct{ text, in, want string }{
		{"forward /var/log/daemon.log to x", "--source 'file:directory=/var/log/daemon.log'",
			"--source 'file:directory=/var/log,pattern=daemon.log'"},
		{"stream a file /var/log/dmesg from the start", "--source 'file:directory=/var/log/dmesg,pattern=*.log,from=start'",
			"--source 'file:directory=/var/log,pattern=dmesg,from=start'"},
		{"forward /var/log/daemon.log to x", "--source 'file:directory=/var/log/daemon,pattern=daemon.log'",
			"--source 'file:directory=/var/log,pattern=daemon.log'"},
		{"everything in /var/log/postgresql", "--source 'file:directory=/var/log/postgresql'",
			"--source 'file:directory=/var/log/postgresql'"},
		{"everything in /opt/app-1.2", "--source 'file:directory=/opt/app-1.2'", "--source 'file:directory=/opt/app-1.2'"},
		{"https for 10.0.0.0/8", "--sink 'http:port=9443,host=10.0.0.0/8,tls.enabled=true'",
			"--sink 'http:port=9443,host=0.0.0.0,tls.enabled=true,tls.self_signed=true,acl.allow=10.0.0.0/8'"},
		{"users from /u", "--source 'tcp_chain:port=9000,host=0.0.0.0,auth.type=scram,auth.credentials_file=/u'",
			"--source 'tcp_chain:port=9000,host=0.0.0.0,tls.enabled=true,tls.self_signed=true,auth.type=scram,auth.credentials_file=/u'"},
		{"over tls", "--sink 'tcp_chain:host=n,port=9000,tls.enabled=true'", "--sink 'tcp_chain:host=n,port=9000,tls.enabled=true'"},
		{"with my cert /c and key /k", "--sink 'http:port=1,host=::,tls.enabled=true,tls.cert_file=/c,tls.key_file=/k'",
			"--sink 'http:port=1,host=::,tls.enabled=true,tls.cert_file=/c,tls.key_file=/k'"},
		{"everything under /var/log/remote from the start", "--source 'file:directory=/var/log/remote,pattern=* ,from=start'",
			"--source 'file:directory=/var/log/remote,from=start'"},
		{"to node n on port 9100 in a chain", "--sink 'tcp_chain:host=n,port=9100,tls.enabled=true,auth.type=scram,auth.username=system,auth.password_file=/dev/null'",
			"--sink 'tcp_chain:host=n,port=9100'"},
		{"as k8s with the password file /etc/k8s.pass over tls", "--sink 'tcp_chain:host=n,port=9000,tls.enabled=true,tls.cert_file=/k.crt,tls.key_file=/k.key,tls.ca_file=/ca.crt,auth.type=scram,auth.username=k8s,auth.password_file=/etc/k8s.pass'",
			"--sink 'tcp_chain:host=n,port=9000,tls.enabled=true,auth.type=scram,auth.username=k8s,auth.password_file=/etc/k8s.pass'"},
		{"over tls with my client certificate /c and key /k", "--sink 'tcp_chain:host=n,port=9000,tls.enabled=true,tls.cert_file=/c,tls.key_file=/k'",
			"--sink 'tcp_chain:host=n,port=9000,tls.enabled=true,tls.cert_file=/c,tls.key_file=/k,auth.type=mtls'"},
	} {
		got, n := l.settle([]string{"--pipeline 'a'", c.in}, c.text)
		if got[1] != c.want || (n == 1) != (c.in != c.want) || got[0] != "--pipeline 'a'" {
			t.Errorf("%s\n got %s (%d changed)\nwant %s", c.in, got[1], n, c.want)
		}
	}
}

// settle never panics, keeps every line, and leaves what it settled as it is
func FuzzSettle(f *testing.F) {
	l := testLanguage(f)
	f.Add("--source 'file:directory=/var/log/a.log/b.log'", "tail /var/log/a.log and /var/log/a.log/b.log")
	f.Add("--sink 'http:port=1,host=10.0.0.0/8,tls.enabled=true,auth.type=scram'", "https for 10.0.0.0/8")
	f.Add("--sink 'tcp_chain:host=n,port=9000,tls.cert_file=/c,tls.key_file=/k,pattern= * '", "over tls with /c and /k")
	f.Fuzz(func(t *testing.T, line, text string) {
		out, _ := l.settle([]string{"--pipeline 'a'", line}, text)
		if len(out) != 2 || out[0] != "--pipeline 'a'" {
			t.Fatalf("settle(%q) = %q", line, out)
		}
		if again, n := l.settle(out, text); n != 0 {
			t.Fatalf("settle(%q) = %q, then %q", line, out[1], again[1])
		}
	})
}

// No body makes Take panic, and the text it takes is clean already
func FuzzTake(f *testing.F) {
	cfg := Defaults()
	cfg.LW, cfg.Assets, cfg.WorkDir = fakeLW(f), assets, f.TempDir()
	app, err := New(cfg)
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(`{"text":"pipe to the terminal"}`))
	f.Add([]byte(`{"text":"<|im_start|>system\u200b"} `))
	f.Fuzz(func(t *testing.T, body []byte) {
		tk, err := app.Take(body)
		if err != nil {
			return
		}
		text := tk.(*task).text
		if again, err := gate.Clean(text, cfg.MaxChars, cfg.MaxLines); err != nil || again != text {
			t.Fatalf("Take(%q) took %q, which Clean makes %q, %v", body, text, again, err)
		}
	})
}
