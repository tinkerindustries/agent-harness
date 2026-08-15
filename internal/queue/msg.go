package queue

import "time"

// Msg is one claimed unit of work and the four ways of disposing of it.
type Msg interface {
	Data() []byte
	DeliveryCount() uint64         // 1 on the first delivery
	Ack() error                    // done; the message goes away
	Nak(delay time.Duration) error // try again after delay
	Term() error                   // never again; the message goes away
	InProgress() error             // extend the lease
}
