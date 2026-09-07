package geministdio

import (
	"sync"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
)

// pump stands between the hub and the pipe. The hub drops — and closes — a
// subscriber whose 256-frame buffer fills, so a parent that stops reading
// stdout for a moment would otherwise cost the run its whole event stream
// (internal/hub, publish). This drains the hub as fast as it produces and
// holds the backlog here instead, where growing costs memory rather than the
// stream.
//
// There is no ceiling on the backlog on purpose. A parent that has stopped
// reading stdout has stopped hosting the session, and the session's own
// lifecycle — stdin closing, or the parent exiting — is what ends it.
type pump struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []hub.Frame
	closed bool
}

func newPump() *pump {
	p := &pump{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *pump) push(f hub.Frame) {
	p.mu.Lock()
	p.queue = append(p.queue, f)
	p.mu.Unlock()
	p.cond.Signal()
}

// close marks the source exhausted. next then drains whatever is left and
// reports false only once the backlog is empty, so nothing already produced
// is lost when the run ends.
func (p *pump) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *pump) next() (hub.Frame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.queue) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.queue) == 0 {
		return hub.Frame{}, false
	}
	f := p.queue[0]
	p.queue = p.queue[1:]
	return f, true
}

// drain reads frames from the hub subscription into the pump until the
// subscription closes.
func (p *pump) drain(frames <-chan hub.Frame) {
	for f := range frames {
		p.push(f)
	}
	p.close()
}
