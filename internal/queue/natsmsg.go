package queue

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// natsMsg adapts a jetstream.Msg to the queue.Msg seam, so the worker pool
// can be written against one narrow interface while NATS is still behind it.
// It is the phase-1 implementation only: the SQLite queue (the plan's phase
// 4/5) implements Msg over a claimed work_queue row instead.
type natsMsg struct {
	msg jetstream.Msg
}

// WrapNATS adapts a JetStream message for the worker pool. The pool's
// Consumer callback is the only place a jetstream.Msg crosses into queue.Msg.
func WrapNATS(msg jetstream.Msg) Msg {
	return natsMsg{msg: msg}
}

func (m natsMsg) Data() []byte {
	return m.msg.Data()
}

// DeliveryCount reports how many times the message has been delivered, 1 on
// the first. A Metadata read failure falls back to 1: the delivery ceiling
// only fails a request once its count is exhausted, so an unknown count must
// not look like an exhausted one — a message whose metadata cannot be read
// should be treated as a fresh attempt, not Term'd on arrival.
func (m natsMsg) DeliveryCount() uint64 {
	meta, err := m.msg.Metadata()
	if err != nil || meta == nil {
		return 1
	}
	return meta.NumDelivered
}

func (m natsMsg) Ack() error {
	return m.msg.Ack()
}

func (m natsMsg) Nak(delay time.Duration) error {
	return m.msg.NakWithDelay(delay)
}

func (m natsMsg) Term() error {
	return m.msg.Term()
}

func (m natsMsg) InProgress() error {
	return m.msg.InProgress()
}
