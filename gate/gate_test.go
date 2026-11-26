package gate

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lixenwraith/llmgate/model"
	"github.com/lixenwraith/log"
)

// fake is a model that answers at once, but holds its next hold chats until
// their context ends
type fake struct{ hold atomic.Int32 }

func (f *fake) Name() string                    { return "fake" }
func (f *fake) Ready(ctx context.Context) error { return nil }
func (f *fake) Chat(ctx context.Context, r model.Request) (model.Answer, error) {
	if f.hold.Add(-1) >= 0 {
		<-ctx.Done()
		return model.Answer{}, ctx.Err()
	}
	return model.Answer{Content: "echo: " + r.Messages[0].Content}, nil
}

// echo is an app that sends its text to the model, under its admission
type echo struct{ adm Admission }

func (echo) Name() string           { return "echo" }
func (e echo) Admission() Admission { return e.adm }
func (echo) Info() map[string]any   { return map[string]any{"max_chars": 20} }
func (echo) Take(body []byte) (Task, error) {
	var in struct{ Text string }
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	text, err := Clean(in.Text, 20, 1)
	return &echoTask{text: text}, err
}

type echoTask struct{ text string }

func (e *echoTask) Run(ctx context.Context, m model.Model) (map[string]any, error) {
	a, err := m.Chat(ctx, model.Request{Messages: []model.Message{{Role: "user", Content: e.text}}})
	return map[string]any{"answer": a.Content}, err
}

func (e *echoTask) Record() map[string]any { return map[string]any{"text": e.text} }

func (echo) Guide() []byte { return []byte("# Echo\n") }

// open serves a gate that is ready, its review log in dir, with echo under
// a client's pace alone or under adm
func open(t *testing.T, m model.Model, dir string, adm ...Admission) (*httptest.Server, *log.Logger) {
	t.Helper()
	app := echo{Admission{Client: Limits{Burst: 10, Refill: time.Minute}}}
	if len(adm) > 0 {
		app.adm = adm[0]
	}
	review := log.NewLogger()
	c := log.DefaultConfig()
	c.Name, c.Directory, c.Format, c.Sanitization = "requests", dir, "json", log.PolicyRaw
	c.EnableFile, c.EnableConsole = true, false
	if err := review.ApplyConfig(c); err != nil {
		t.Fatal(err)
	}
	review.Start()
	ops := log.NewLogger()
	c.Name = "llmgate"
	ops.ApplyConfig(c)
	ops.Start()
	s := New(Config{Proxies: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, MaxBody: 256, Deadline: 5 * time.Second,
		Heartbeat: time.Hour, Waiting: 3, Health: time.Hour}, m, []App{app}, ops, review)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	srv := httptest.NewServer(s)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		review.Shutdown(time.Second)
		ops.Shutdown(time.Second)
	})
	for range 100 {
		if s.ready.Load() {
			return srv, review
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the gate never opened")
	return nil, nil
}

// events posts body as the visitor at real and reads the stream until it
// ends, the event kinds joined by commas and the last event
func events(ctx context.Context, url, real, body string) (string, map[string]any) {
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/echo", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Real-IP", real)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err.Error(), nil
	}
	defer res.Body.Close()
	var kinds []string
	var last map[string]any
	for sc := bufio.NewScanner(res.Body); sc.Scan(); {
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			return sc.Text(), nil
		}
		kinds = append(kinds, last["event"].(string))
	}
	return strings.Join(kinds, ","), last
}

func TestPostStreamsItsPlaceRunAndOutcomeAndLeavesOneRecord(t *testing.T) {
	dir := t.TempDir()
	srv, review := open(t, &fake{}, dir)
	kinds, done := events(context.Background(), srv.URL, "203.0.113.7", `{"text":"hello\u200b  world"}`)
	if kinds != "queued,running,done" {
		t.Fatalf("events %s, want queued,running,done", kinds)
	}
	if done["answer"] != "echo: hello world" {
		t.Errorf("answer %q", done["answer"])
	}
	review.Flush(time.Second)
	data, err := os.ReadFile(filepath.Join(dir, "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(data, &rec); err != nil || rec.Message != "request" {
		t.Fatalf("requests.log %q: %v", data, err)
	}
	if rec.Fields["outcome"] != "done" || rec.Fields["text"] != "hello world" || rec.Fields["id"] != done["id"] {
		t.Errorf("record %v", rec.Fields)
	}
	if strings.Contains(string(data), "203.0.113") || len(rec.Fields["client"].(string)) != 12 {
		t.Errorf("the record names the client by more than a pseudonym: %s", data)
	}
}

func TestRefusalsCarryTheirStatusAndCode(t *testing.T) {
	srv, _ := open(t, &fake{}, t.TempDir())
	for _, c := range []struct {
		method, path, ctype, body string
		status                    int
		code                      string
	}{
		{"POST", "/echo", "text/plain", `{"text":"hi"}`, 415, "unsupported_media_type"},
		{"POST", "/echo", "application/json", `{"text":"` + strings.Repeat("a", 300) + `"}`, 413, "too_large"},
		{"POST", "/echo", "application/json", `{"text":"\u200b"}`, 400, "text_invalid"},
		{"POST", "/other", "application/json", `{}`, 404, "not_found"},
		{"DELETE", "/echo", "", "", 405, "method_not_allowed"},
		{"GET", "/echo/other", "", "", 404, "not_found"},
		{"POST", "/echo/guide", "application/json", `{}`, 405, "method_not_allowed"},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", c.ctype)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Error struct{ Code string }
		}
		json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if res.StatusCode != c.status || out.Error.Code != c.code {
			t.Errorf("%s %s: %d %q, want %d %q", c.method, c.path, res.StatusCode, out.Error.Code, c.status, c.code)
		}
	}
}

func TestAClientLeavingHandsTheModelToTheNext(t *testing.T) {
	m := &fake{}
	m.hold.Store(1)
	srv, _ := open(t, m, t.TempDir())
	first, leave := context.WithCancel(context.Background())
	left := make(chan string)
	go func() { k, _ := events(first, srv.URL, "203.0.113.1", `{"text":"one"}`); left <- k }()
	time.Sleep(100 * time.Millisecond) // the first holds the model
	second := make(chan string)
	go func() { k, _ := events(context.Background(), srv.URL, "203.0.113.2", `{"text":"two"}`); second <- k }()
	time.Sleep(100 * time.Millisecond)
	leave()
	if k := <-left; k != "queued,running" {
		t.Errorf("the first's events %s, want queued,running", k)
	}
	select {
	case k := <-second:
		if k != "queued,running,done" {
			t.Errorf("the second's events %s, want queued,running,done", k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second never ran after the first left")
	}
}

func TestGETTellsAnAppsLimitsAndServesItsGuide(t *testing.T) {
	srv, _ := open(t, &fake{}, t.TempDir())
	res, err := http.Get(srv.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	var info struct{ Limits map[string]map[string]float64 }
	json.NewDecoder(res.Body).Decode(&info)
	res.Body.Close()
	if c, a := info.Limits["client"], info.Limits["app"]; c["burst"] != 10 || c["every_s"] != 60 || c["daily"] != 0 || a == nil || a["burst"] != 0 {
		t.Errorf("limits %v, want echo's own", info.Limits)
	}
	res, err = http.Get(srv.URL + "/echo/guide")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" || string(body) != "# Echo\n" {
		t.Errorf("guide: %d %s %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
}

// The app's pace holds across clients; a client it turns away keeps its own
// request for when the app has room again
func TestTheAppsPaceHoldsAcrossClientsAndSparesTheirOwn(t *testing.T) {
	srv, _ := open(t, &fake{}, t.TempDir(), Admission{Client: Limits{Burst: 1, Refill: time.Hour},
		App: Limits{Burst: 1, Refill: 300 * time.Millisecond}})
	post := func(ip string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/echo", strings.NewReader(`{"text":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Real-IP", ip)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			Error struct{ Code string }
		}
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out.Error.Code
	}
	if status, code := post("203.0.113.1"); status != 200 {
		t.Fatalf("the first client: %d %q", status, code)
	}
	if status, code := post("203.0.113.2"); status != 503 || code != "app_rate_limited" {
		t.Fatalf("a second client at once: %d %q, want 503 app_rate_limited", status, code)
	}
	time.Sleep(350 * time.Millisecond)
	if status, code := post("203.0.113.2"); status != 200 {
		t.Errorf("the second client once the app has room: %d %q, want its own request back", status, code)
	}
}
