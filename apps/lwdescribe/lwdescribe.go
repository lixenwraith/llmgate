// Package lwdescribe drafts LogWisp pipelines from a visitor's description:
// lw flags under a JSON Schema built from lw --schema, checked by lw --dump
// of the LogWisp the page's engine is built from, and repaired with lw's own
// words. The page then parses and validates the draft with that engine.
package lwdescribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lixenwraith/config"
	"github.com/lixenwraith/llmgate/gate"
	"github.com/lixenwraith/llmgate/model"
	"github.com/lixenwraith/toml"
)

// Config is the app's section of llmgate.toml. LW is the lw of the LogWisp
// the page's engine is built from; Assets holds prompt.md, examples.toml and
// guide.md; WorkDir is where lw runs, an empty directory, since lw reads
// ./logwisp.toml. RecordText keeps the description and the draft in the
// review log.
type Config struct {
	Enabled    bool           `toml:"enabled"`
	LW         string         `toml:"lw"`
	Assets     string         `toml:"assets"`
	WorkDir    string         `toml:"work_dir"`
	Constrain  string         `toml:"constrain"`
	MaxChars   int            `toml:"max_chars"`
	MaxLines   int            `toml:"max_lines"`
	MaxTokens  int            `toml:"max_tokens"`
	Repairs    int            `toml:"repairs"`
	RecordText bool           `toml:"record_text"`
	Limits     gate.Admission `toml:"limits"`
}

func Defaults() Config {
	return Config{Enabled: true, LW: "/usr/local/bin/lw", Assets: "/usr/local/etc/llmgate/lwdescribe",
		WorkDir: "/var/empty", Constrain: "patterns", MaxChars: 600, MaxLines: 12, MaxTokens: 320, Repairs: 1,
		RecordText: true, Limits: gate.Admission{Client: gate.Limits{Burst: 1, Refill: 10 * time.Second, Daily: 20},
			App: gate.Limits{Burst: 6, Refill: 20 * time.Second, Daily: 500}}}
}

type App struct {
	cfg      Config
	build    string // lw --version
	lang     *language
	format   json.RawMessage
	prefix   []model.Message
	guide    []byte
	examples int
}

// The answer each reader takes: the gate parses JSON, the page's paste box
// lw's flags, a line each
const (
	gateAnswer   = `JSON only, {"flags": [...]}, one flag a string`
	gateDecline  = `{"flags": []}`
	guideAnswer  = "the flags only, one a line, and nothing else"
	guideDecline = "# not a log pipeline"
)

// example is a description and the flags it should draft
type example struct {
	Text  string   `toml:"text"`
	Flags []string `toml:"flags"`
}

// New reads lw's version and schema, renders the prompt around the catalog,
// for the model here and as the guide for a visitor's own, and refuses to
// start on an example the language or lw --dump refuses: a wrong example
// teaches the model a wrong answer
func New(cfg Config) (*App, error) {
	a := &App{cfg: cfg}
	version, err := a.lw(context.Background(), "--version")
	if err != nil {
		return nil, fmt.Errorf("lw --version: %w", err)
	}
	a.build = strings.TrimSpace(version)
	data, err := a.lw(context.Background(), "--schema")
	if err != nil {
		return nil, fmt.Errorf("lw --schema: %w", err)
	}
	if a.lang, err = newLanguage([]byte(data)); err != nil {
		return nil, err
	}
	if a.format, err = a.lang.jsonSchema(cfg.Constrain); err != nil {
		return nil, err
	}
	prompt, err := asset(cfg.Assets, "prompt.md", "{{answer}}", "{{decline}}", "{{catalog}}")
	if err != nil {
		return nil, err
	}
	guide, err := asset(cfg.Assets, "guide.md", "{{prompt}}", "{{examples}}")
	if err != nil {
		return nil, err
	}
	examples, err := readExamples(filepath.Join(cfg.Assets, "examples.toml"))
	if err != nil {
		return nil, err
	}
	terms := func(answer, decline string) string {
		return strings.NewReplacer("{{answer}}", answer, "{{decline}}", decline, "{{catalog}}", a.lang.catalog).Replace(prompt)
	}
	a.prefix = []model.Message{{Role: "system", Content: terms(gateAnswer, gateDecline)}}
	var shown strings.Builder
	for i, e := range examples {
		if len(e.Flags) > 0 {
			problems := a.lang.check(e.Flags)
			if problems == "" {
				problems, err = a.dump(context.Background(), e.Flags)
			}
			if err != nil {
				return nil, fmt.Errorf("examples.toml example %d: %w", i+1, err)
			} else if problems != "" {
				return nil, fmt.Errorf("examples.toml example %d: %s", i+1, problems)
			}
		}
		answer, _ := json.Marshal(map[string][]string{"flags": append([]string{}, e.Flags...)})
		a.prefix = append(a.prefix, userTurn(e.Text), model.Message{Role: "assistant", Content: string(answer)})
		lines := strings.Join(e.Flags, "\n")
		if len(e.Flags) == 0 {
			lines = guideDecline
		}
		fmt.Fprintf(&shown, "Description: %s\n%s\n\n", e.Text, lines)
	}
	a.guide = []byte(strings.NewReplacer("{{engine}}", a.lang.version, "{{prompt}}", terms(guideAnswer, guideDecline),
		"{{examples}}", strings.TrimSpace(shown.String())).Replace(guide))
	a.examples = len(examples)
	return a, nil
}

// asset is a text of the assets that names every placeholder it must hold
func asset(dir, name string, holds ...string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	for _, h := range holds {
		if !bytes.Contains(data, []byte(h)) {
			return "", fmt.Errorf("%s: no %s", name, h)
		}
	}
	return string(data), nil
}

func readExamples(path string) ([]example, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := toml.NewParser(data).Parse()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var v struct {
		Example []example `toml:"example"`
	}
	if err := config.ScanMap(root, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(v.Example) == 0 {
		return nil, fmt.Errorf("%s: no [[example]]", path)
	}
	return v.Example, nil
}

// userTurn is the user turn around a description: fenced, and called data
// by the system message, the same for the examples and the visitor
func userTurn(text string) model.Message {
	return model.Message{Role: "user", Content: "Description:\n<<<\n" + text + "\n>>>"}
}

func (a *App) Name() string              { return "lwdescribe" }
func (a *App) Guide() []byte             { return a.guide }
func (a *App) Admission() gate.Admission { return a.cfg.Limits }

// Info is GET's answer: the engine's version, which the page compares its
// own schema's to, and the longest description
func (a *App) Info() map[string]any {
	return map[string]any{"engine": a.lang.version, "max_chars": a.cfg.MaxChars, "max_lines": a.cfg.MaxLines}
}

// Summary is what -check prints and the operations log keeps at start
func (a *App) Summary() string {
	return fmt.Sprintf("lwdescribe: engine %s, %d examples checked, catalog %d characters, constrain %s",
		a.build, a.examples, len(a.lang.catalog), a.cfg.Constrain)
}

func (a *App) Take(body []byte) (gate.Task, error) {
	var in struct {
		Text string `json:"text"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, &gate.Refusal{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	text, err := gate.Clean(in.Text, a.cfg.MaxChars, a.cfg.MaxLines)
	if err != nil {
		return nil, err
	}
	return &task{app: a, text: text}, nil
}

// Warm sends the prefix with a short description, cut after a few tokens:
// the prompt cache keeps the prefix for every request after it
func (a *App) Warm(ctx context.Context, m model.Model) error {
	_, err := m.Chat(ctx, model.Request{Messages: append(slices.Clone(a.prefix), userTurn("warm up")), Format: a.format, MaxTokens: 8})
	if errors.Is(err, model.ErrCut) {
		return nil
	}
	return err
}

type task struct {
	app      *App
	text     string
	flags    []string
	problems string
	refused  []map[string]string // drafts before a repair, with lw's words
	declined bool
	calls    int
	prompt   int
	answer   int
}

// Run asks for flags, holds them to the language and lw --dump, and repairs
// up to Repairs times; flags lw still refuses go to the page with lw's words
func (t *task) Run(ctx context.Context, m model.Model) (map[string]any, error) {
	msgs := append(slices.Clone(t.app.prefix), userTurn(t.text))
	for try := 0; ; try++ {
		a, err := m.Chat(ctx, model.Request{Messages: msgs, Format: t.app.format, MaxTokens: t.app.cfg.MaxTokens})
		t.calls, t.prompt, t.answer = t.calls+1, t.prompt+a.PromptTokens, t.answer+a.AnswerTokens
		if err != nil {
			return nil, err
		}
		var out struct {
			Flags []string `json:"flags"`
		}
		if err := json.Unmarshal([]byte(answerJSON(a.Content)), &out); err != nil {
			return nil, fmt.Errorf("answer: %w", err)
		}
		if len(out.Flags) == 0 {
			t.declined = true
			return map[string]any{"declined": true}, nil
		}
		t.flags = out.Flags
		if t.problems = t.app.lang.check(out.Flags); t.problems == "" {
			if t.problems, err = t.app.dump(ctx, out.Flags); err != nil {
				return nil, err
			}
		}
		if t.problems == "" || try >= t.app.cfg.Repairs {
			break
		}
		t.refused = append(t.refused, map[string]string{"flags": strings.Join(t.flags, "\n"), "problems": t.problems})
		msgs = append(msgs, model.Message{Role: "assistant", Content: a.Content},
			model.Message{Role: "user", Content: "lw refused it:\n" + t.problems + "\nWrite the corrected flags."})
	}
	return map[string]any{"flags": strings.Join(t.flags, "\n"), "problems": t.problems, "engine": t.app.lang.version}, nil
}

// answerJSON is the answer's JSON object; without a format the model may
// wrap it in prose or a code fence
func answerJSON(s string) string {
	if i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

func (t *task) Record() map[string]any {
	r := map[string]any{"calls": t.calls, "prompt_tokens": t.prompt, "answer_tokens": t.answer,
		"declined": t.declined, "refused": t.problems != ""}
	if t.app.cfg.RecordText {
		r["text"], r["flags"], r["problems"] = t.text, strings.Join(t.flags, "\n"), t.problems
		if len(t.refused) > 0 {
			r["refused_drafts"] = t.refused
		}
	}
	return r
}

// lw runs lw with args, in WorkDir with no environment: no LOGWISP_
// variable or file reaches it
func (a *App) lw(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.cfg.LW, args...)
	cmd.Dir, cmd.Env = a.cfg.WorkDir, []string{}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if _, exit := errors.AsType[*exec.ExitError](err); exit {
		return stdout.String(), &lwError{strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), err
}

type lwError struct{ text string }

func (e *lwError) Error() string { return e.text }

// dump is lw --dump on the flags: "" when lw takes them, else its problems,
// a line each, without lw's own preamble
func (a *App) dump(ctx context.Context, flags []string) (string, error) {
	var args []string
	for _, f := range flags {
		name, value, _ := strings.Cut(f, " ")
		args = append(args, name, strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'"))
	}
	_, err := a.lw(ctx, append([]string{"--dump"}, args...)...)
	if e, ok := errors.AsType[*lwError](err); ok {
		text := strings.TrimPrefix(e.text, "Error: Failed to load config: ")
		return strings.TrimPrefix(text, "invalid configuration:\n"), nil
	}
	return "", err
}
