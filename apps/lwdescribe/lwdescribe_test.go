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

	"github.com/lixenwraith/llmgate/model"
)

// lw --schema of a LogWisp release (make testdata) and the jail's assets:
// the language, the prompt and the examples the gate starts with
const (
	schemaFile = "testdata/schema.json"
	assets     = "../../jail/usr/local/etc/llmgate/lwdescribe"
)

func testLanguage(t *testing.T) *language {
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

// With LW set to an lw binary, New also holds each example to lw --dump.
func TestExamplesAreDraftsTheModelMayWriteAndLWTakes(t *testing.T) {
	l := testLanguage(t)
	var model []*regexp.Regexp
	for _, p := range l.patterns {
		model = append(model, regexp.MustCompile(p.json))
	}
	examples, err := readExamples(filepath.Join(assets, "examples.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range examples {
		if p := l.check(e.Flags); p != "" {
			t.Errorf("example %d: %s", i+1, p)
		}
		for _, f := range e.Flags {
			if !slices.ContainsFunc(model, func(re *regexp.Regexp) bool { return re.MatchString(f) }) {
				t.Errorf("example %d: the model's patterns refuse %s", i+1, f)
			}
		}
	}
	if lw := os.Getenv("LW"); lw != "" {
		cfg := Defaults()
		cfg.LW, cfg.Assets, cfg.WorkDir = lw, assets, t.TempDir()
		if _, err := New(cfg); err != nil {
			t.Errorf("New with %s: %v", lw, err)
		}
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
func fakeLW(t *testing.T) string {
	t.Helper()
	schema, err := filepath.Abs(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lw")
	script := `#!/bin/sh
case "$1" in
--version) echo "test (commit: test)" ;;
--schema) exec /bin/cat '` + schema + `' ;;
--dump) case "$*" in *port=1,*) printf 'Error: Failed to load config: invalid configuration:\npipelines[0].sinks[0].port: 1 is not a port to listen on\n' >&2; exit 1 ;; esac ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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
	return model.Answer{Content: a, PromptTokens: 100, AnswerTokens: 20, Took: time.Second}, nil
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
