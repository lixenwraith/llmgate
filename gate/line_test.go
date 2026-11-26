package gate

import (
	"context"
	"testing"
	"time"
)

// started is whether p has the model, within a moment
func started(p *place) bool {
	select {
	case <-p.start:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// position is p's latest news
func position(t *testing.T, p *place) int {
	t.Helper()
	select {
	case n := <-p.news:
		return n
	case <-time.After(time.Second):
		t.Fatal("no news")
		return 0
	}
}

func TestLineRunsOneAtATimeInArrivalOrder(t *testing.T) {
	l := newLine(5, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _ := l.join("a")
	b, _ := l.join("b")
	c, _ := l.join("c")
	go l.serve(ctx)
	if !started(a) {
		t.Fatal("a never started")
	}
	if position(t, b) != 1 || position(t, c) != 2 {
		t.Fatal("b and c are not 1 and 2 behind a")
	}
	l.leave(b) // b gives up waiting: c moves up
	if position(t, c) != 1 {
		t.Fatal("c did not move up when b left")
	}
	select {
	case <-c.start:
		t.Fatal("c started while a held the model")
	case <-time.After(50 * time.Millisecond):
	}
	l.leave(a)
	if !started(c) {
		t.Fatal("c never started after a left")
	}
}

func TestLineHoldsAClientOnceAndRefusesWhenFull(t *testing.T) {
	l := newLine(2, time.Second)
	a, _ := l.join("a")
	if _, f := l.join("a"); code(f) != "already_queued" {
		t.Errorf("a second place for a: %q, want already_queued", code(f))
	}
	l.leave(a)
	if _, f := l.join("a"); f != nil {
		t.Errorf("a again after leaving: %q, want admitted", code(f))
	}
	l.join("b")
	if _, f := l.join("c"); code(f) != "busy" {
		t.Errorf("a third place in a line of 2: %q, want busy", code(f))
	}
	if _, f := l.join(""); f != nil {
		t.Errorf("the gate's own place: %q, want admitted", code(f))
	}
}
