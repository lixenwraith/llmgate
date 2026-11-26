package gate

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// line is the one queue to the model, shared by every app: one place runs
// at a time, in arrival order, and a client holds at most one place, waiting
// or running. A place is left once, by its holder, however its request ends.
type line struct {
	mu      sync.Mutex
	waiting []*place
	held    map[string]bool
	took    time.Duration // recent run time, an average, for the ETA
	limit   int
	arrived chan struct{}
}

// place is a request's turn: news carries its latest position, a mailbox of
// one; start closes when it reaches the model, done when its run is over
type place struct {
	client  string
	told    int
	news    chan int
	start   chan struct{}
	done    chan struct{}
	started bool
}

func newLine(limit int, took time.Duration) *line {
	return &line{held: map[string]bool{}, took: took, limit: limit, arrived: make(chan struct{}, 1)}
}

// join adds a place at the end, or refuses: the client already holds one,
// or the line is full. A client of "" is the gate's own (warm-up).
func (l *line) join(client string) (*place, *Refusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case client != "" && l.held[client]:
		return nil, &Refusal{http.StatusServiceUnavailable, "already_queued"}
	case client != "" && len(l.waiting) >= l.limit:
		return nil, &Refusal{http.StatusServiceUnavailable, "busy"}
	}
	p := &place{client: client, news: make(chan int, 1), start: make(chan struct{}), done: make(chan struct{})}
	if client != "" {
		l.held[client] = true
	}
	l.waiting = append(l.waiting, p)
	l.tell()
	select {
	case l.arrived <- struct{}{}:
	default:
	}
	return p, nil
}

// leave ends a place: out of the line if it is still waiting, else its run
// is over and the next may start
func (l *line) leave(p *place) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.started {
		close(p.done)
	} else {
		for i, w := range l.waiting {
			if w == p {
				l.waiting = append(l.waiting[:i], l.waiting[i+1:]...)
				break
			}
		}
		l.tell()
	}
	if p.client != "" {
		delete(l.held, p.client)
	}
}

// ran folds a run's time into the average the ETA uses
func (l *line) ran(d time.Duration) {
	l.mu.Lock()
	l.took = (3*l.took + d) / 4
	l.mu.Unlock()
}

// eta is the wait a place at position n can expect
func (l *line) eta(n int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Duration(n) * l.took
}

func (l *line) length() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.waiting)
}

// tell gives each waiting place a position it has not been told; called with
// mu held
func (l *line) tell() {
	for i, p := range l.waiting {
		if p.told == i+1 {
			continue
		}
		p.told = i + 1
		select {
		case <-p.news:
		default:
		}
		p.news <- i + 1
	}
}

// serve hands the model to the head of the line, one place at a time, until
// ctx ends
func (l *line) serve(ctx context.Context) {
	for {
		l.mu.Lock()
		var p *place
		if len(l.waiting) > 0 {
			p, l.waiting = l.waiting[0], l.waiting[1:]
			p.started = true
			l.tell()
		}
		l.mu.Unlock()
		if p == nil {
			select {
			case <-l.arrived:
				continue
			case <-ctx.Done():
				return
			}
		}
		close(p.start)
		select {
		case <-p.done:
		case <-ctx.Done():
			return
		}
	}
}
