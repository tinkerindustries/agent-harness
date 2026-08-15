package queue

import "context"

// ResultSink is where a worker publishes what happened to a request.
type ResultSink interface {
	// Final publishes the terminal result for requestID. data is an
	// already-marshalled queue.Result. Publishing twice for one
	// requestID must produce one message, not two.
	Final(ctx context.Context, requestID string, data []byte) error
	Accepted(ctx context.Context, a Accepted) error
	Progress(ctx context.Context, p Progress) error
}
