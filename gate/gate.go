// Package gate is llmgate's core, the one way from web pages to the model. It
// serves apps at /NAME: GET says whether an app is ready, POST admits a
// request (clean input, a client's rate) into the one line the model is
// shared through, and answers with an NDJSON stream of the request's place
// and outcome. Each request leaves one record in the review log.
package gate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/lixenwraith/llmgate/model"
	"github.com/lixenwraith/log"
)

// App is one use of the model, served at /NAME. Take checks a request body
// before the line and returns its task, or a *Refusal the visitor is told.
type App interface {
	Name() string
	Admission() Admission
	Info() map[string]any
	Take(body []byte) (Task, error)
}

// Task is an admitted request. Run holds the model and returns the fields
// of the done event; Record is what the review log keeps, read after Run.
// A *Refusal from Run is reported by its code; any other error is
// model_error.
type Task interface {
	Run(ctx context.Context, m model.Model) (map[string]any, error)
	Record() map[string]any
}

// Warmer is an app that fills the model's prompt cache with its prefix,
// before the gate answers and whenever the model server comes back
type Warmer interface {
	Warm(ctx context.Context, m model.Model) error
}

// Guide is an app that can hand its task to a visitor's own model: GET
// /NAME/guide serves the text, whether or not the model here is up
type Guide interface {
	Guide() []byte
}

// Config is the gate's own: the proxies whose X-Real-IP is believed, the
// largest body, a request's whole life (queueing included), the beat that
// keeps idle timers along the way off, the line's length and how often the
// model server is asked whether it is up
type Config struct {
	Proxies   []netip.Addr
	MaxBody   int64
	Deadline  time.Duration
	Heartbeat time.Duration
	Waiting   int
	Health    time.Duration
}

type Server struct {
	cfg    Config
	model  model.Model
	apps   map[string]*served
	line   *line
	ops    *log.Logger
	review *log.Logger
	key    [32]byte
	ready  atomic.Bool
}

// served is an app with its limiters, its clients' and its own, and counts
type served struct {
	App
	client, app *limiter
	mu          sync.Mutex
	counts      map[string]int
}

func (a *served) count(outcome string) {
	a.mu.Lock()
	a.counts[outcome]++
	a.mu.Unlock()
}

func New(cfg Config, m model.Model, apps []App, ops, review *log.Logger) *Server {
	s := &Server{cfg: cfg, model: m, apps: map[string]*served{}, line: newLine(cfg.Waiting, 15*time.Second), ops: ops, review: review}
	rand.Read(s.key[:])
	for _, a := range apps {
		adm := a.Admission()
		s.apps[a.Name()] = &served{App: a, client: newLimiter(adm.Client, clientPaced, clientSpent),
			app: newLimiter(adm.App, appPaced, appSpent), counts: map[string]int{}}
	}
	return s
}

// Run serves the line and keeps the gate ready while the model server is:
// warm, then open; closed while it is down, warmed again when it is back
func (s *Server) Run(ctx context.Context) {
	go s.line.serve(ctx)
	tick := time.NewTicker(s.cfg.Health)
	defer tick.Stop()
	for {
		// a server that takes connections but never answers is down too
		rctx, cancel := context.WithTimeout(ctx, s.cfg.Health)
		err := s.model.Ready(rctx)
		cancel()
		switch {
		case err != nil && s.ready.Swap(false):
			s.ops.Warn("model server down; apps closed", "model", s.model.Name(), "error", err.Error())
		case err != nil:
		case !s.ready.Load():
			if err := s.warm(ctx); err != nil {
				s.ops.Warn("warm-up failed", "error", err.Error())
				break
			}
			s.ready.Store(true)
			s.ops.Info("apps open", "model", s.model.Name())
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// warm runs each warmer in the line, as the gate's own place, so a request
// already waiting never races it for the model
func (s *Server) warm(ctx context.Context) error {
	p, _ := s.line.join("")
	defer s.line.leave(p)
	select {
	case <-p.start:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, a := range s.apps {
		if w, ok := a.App.(Warmer); ok {
			start := time.Now()
			if err := w.Warm(ctx, s.model); err != nil {
				return errors.Join(errors.New(a.Name()), err)
			}
			s.ops.Info("warmed", "app", a.Name(), "ms", time.Since(start).Milliseconds())
		}
	}
	return nil
}

// event is one line of the stream; fields hold an outcome's own
type event map[string]any

// A body has bodyTime to arrive, and each write writeTime to leave, so a
// slow or stalled peer holds a connection no longer
const bodyTime, writeTime = 10 * time.Second, 15 * time.Second

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeTime))
	name, sub, _ := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
	a := s.apps[name]
	if sub != "" {
		s.serveGuide(w, r, a, sub)
		return
	}
	switch {
	case a == nil:
		refuse(w, &Refusal{http.StatusNotFound, "not_found"})
	case r.Method == http.MethodGet && !s.ready.Load():
		refuse(w, &Refusal{http.StatusServiceUnavailable, "unavailable"})
	case r.Method == http.MethodGet:
		limits := func(l Limits) map[string]any {
			return map[string]any{"burst": l.Burst, "every_s": l.Refill.Seconds(), "daily": l.Daily}
		}
		info := map[string]any{}
		maps.Copy(info, a.Info())
		info["model"], info["waiting"] = s.model.Name(), s.line.length()
		info["limits"] = map[string]any{"client": limits(a.client.Limits), "app": limits(a.app.Limits)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", "GET, POST")
		refuse(w, &Refusal{http.StatusMethodNotAllowed, "method_not_allowed"})
	default:
		s.post(w, r, a)
	}
}

// serveGuide is GET /NAME/guide, an app's task as a text for another model
func (s *Server) serveGuide(w http.ResponseWriter, r *http.Request, a *served, sub string) {
	var g Guide
	if a != nil && sub == "guide" {
		g, _ = a.App.(Guide)
	}
	switch {
	case g == nil:
		refuse(w, &Refusal{http.StatusNotFound, "not_found"})
	case r.Method != http.MethodGet:
		w.Header().Set("Allow", "GET")
		refuse(w, &Refusal{http.StatusMethodNotAllowed, "method_not_allowed"})
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(g.Guide())
	}
}

// post admits a request, keeps its place in line on the stream, runs it and
// streams its outcome; one review record whatever happens
func (s *Server) post(w http.ResponseWriter, r *http.Request, a *served) {
	arrived := time.Now()
	client := clientKey(r, s.cfg.Proxies)
	rec := map[string]any{"id": newID(), "app": a.Name(), "client": pseudonym(s.key[:], client, arrived)}
	outcome := "refused"
	var task Task
	defer func() {
		rec["outcome"] = outcome
		if task != nil {
			maps.Copy(rec, task.Record())
		}
		a.count(outcome)
		s.review.LogStructured(log.LevelInfo, "request", rec)
	}()
	ref := func(f *Refusal) {
		rec["code"] = f.Code
		refuse(w, f)
	}
	if !s.ready.Load() {
		ref(&Refusal{http.StatusServiceUnavailable, "unavailable"})
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		ref(&Refusal{http.StatusUnsupportedMediaType, "unsupported_media_type"})
		return
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyTime))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBody))
	rc.SetReadDeadline(time.Time{})
	if _, large := errors.AsType[*http.MaxBytesError](err); large {
		ref(&Refusal{http.StatusRequestEntityTooLarge, "too_large"})
		return
	} else if err != nil || !utf8.Valid(body) {
		// a JSON decoder would make broken UTF-8 into U+FFFD, past an app's check
		ref(&Refusal{http.StatusBadRequest, "invalid_request"})
		return
	}
	t, err := a.Take(body)
	if f, ok := errors.AsType[*Refusal](err); ok {
		ref(f)
		return
	} else if err != nil {
		ref(&Refusal{http.StatusBadRequest, "invalid_request"})
		return
	}
	// the client's own limits, then the app's: a request the app or the line
	// turns away costs the client nothing
	if f := a.client.allow(client, arrived); f != nil {
		ref(f)
		return
	}
	if f := a.app.allow("", arrived); f != nil {
		a.client.refund(client)
		ref(f)
		return
	}
	p, f := s.line.join(client)
	if f != nil {
		a.client.refund(client)
		a.app.refund("")
		ref(f)
		return
	}
	defer s.line.leave(p)
	task = t
	ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(s.cfg.Deadline))
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	send := func(e event) {
		data, _ := json.Marshal(e)
		rc.SetWriteDeadline(time.Now().Add(writeTime))
		w.Write(append(data, '\n'))
		rc.Flush()
	}
	// stop is a request ended early: its client left, or its deadline came
	// and the client, still there, is told
	stop := func() {
		if outcome = "left"; errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome, rec["code"] = "timeout", "timeout"
			send(event{"event": "error", "code": "timeout", "id": rec["id"]})
		}
	}
	n := <-p.news // join told the place its position
	send(event{"event": "queued", "position": n, "eta_s": int(s.line.eta(n).Seconds())})
	beat := time.NewTicker(s.cfg.Heartbeat)
	defer beat.Stop()
	for waiting := true; waiting; {
		select {
		case n := <-p.news:
			send(event{"event": "queued", "position": n, "eta_s": int(s.line.eta(n).Seconds())})
		case <-beat.C:
			send(event{"event": "waiting"})
		case <-p.start:
			waiting = false
		case <-ctx.Done():
			stop()
			return
		}
	}
	queued := time.Since(arrived)
	rec["queue_ms"] = queued.Milliseconds()
	send(event{"event": "running", "model": s.model.Name()})
	type result struct {
		fields map[string]any
		err    error
	}
	done := make(chan result, 1)
	go func() {
		// an app that panics fails its request, not the gate and every other
		defer func() {
			if p := recover(); p != nil {
				s.ops.Error("app panicked", "app", a.Name(), "panic", fmt.Sprint(p))
				done <- result{nil, &Refusal{http.StatusInternalServerError, "app_error"}}
			}
		}()
		fields, err := t.Run(ctx, s.model)
		done <- result{fields, err}
	}()
	for {
		select {
		case <-beat.C:
			send(event{"event": "running"})
		case <-ctx.Done():
			<-done // the model call ends with its context
			rec["run_ms"] = (time.Since(arrived) - queued).Milliseconds()
			stop()
			return
		case res := <-done:
			took := time.Since(arrived) - queued
			rec["run_ms"] = took.Milliseconds()
			f, refused := errors.AsType[*Refusal](res.err)
			switch {
			case res.err == nil:
				s.line.ran(took)
				outcome = "done"
				e := event{"event": "done", "id": rec["id"], "ms": time.Since(arrived).Milliseconds()}
				maps.Copy(e, res.fields)
				send(e)
			case !refused && ctx.Err() != nil:
				stop()
			default:
				code := "model_error"
				if refused {
					code = f.Code
				}
				outcome, rec["code"], rec["error"] = "error", code, res.err.Error()
				send(event{"event": "error", "code": code, "id": rec["id"]})
			}
			return
		}
	}
}

func refuse(w http.ResponseWriter, f *Refusal) {
	w.Header().Set("Content-Type", "application/json")
	if f.Status == http.StatusTooManyRequests || f.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "60")
	}
	w.WriteHeader(f.Status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": f.Code}})
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Status is the gate's state for the operator, on a listener only the jail
// reaches: whether it is open, the line, and each app's outcomes so far
func (s *Server) Status() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apps := map[string]any{}
		for name, a := range s.apps {
			a.mu.Lock()
			apps[name] = maps.Clone(a.counts)
			a.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ready": s.ready.Load(), "model": s.model.Name(),
			"waiting": s.line.length(), "eta_s": int(s.line.eta(1).Seconds()), "apps": apps})
	})
}
