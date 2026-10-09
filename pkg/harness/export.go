package harness

import (
	"context"
	"log/slog"
	"time"
)

// Putter stores one object (pkg/s3.Client in production).
type Putter interface {
	Put(ctx context.Context, key, contentType string, body []byte) error
}

// Exporter uploads session traces off the request path. One worker drains
// the queue in order, so a session's later turn never lands before an
// earlier one.
type Exporter struct {
	Store  Putter
	Logger *slog.Logger

	queue chan export
	done  chan struct{}
}

type export struct {
	key  string
	body []byte
}

func NewExporter(store Putter, logger *slog.Logger) *Exporter {
	e := &Exporter{Store: store, Logger: logger, queue: make(chan export, 256), done: make(chan struct{})}
	go e.run()
	return e
}

// Enqueue never blocks a turn: when the queue is full the upload is dropped
// (the next turn of that session uploads the whole trace again).
func (e *Exporter) Enqueue(key string, body []byte) {
	select {
	case e.queue <- export{key, body}:
	default:
		e.Logger.Warn("session export dropped: queue full", "key", key)
	}
}

// Close stops accepting uploads and waits for the queue to drain, or ctx.
func (e *Exporter) Close(ctx context.Context) {
	close(e.queue)
	select {
	case <-e.done:
	case <-ctx.Done():
		e.Logger.Warn("session export: shutdown before queue drained", "pending", len(e.queue))
	}
}

func (e *Exporter) run() {
	defer close(e.done)
	for x := range e.queue {
		var err error
		for attempt := range 3 {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*attempt) * time.Second)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err = e.Store.Put(ctx, x.key, "application/json", x.body)
			cancel()
			if err == nil {
				break
			}
		}
		if err != nil {
			e.Logger.Error("session export failed", "key", x.key, "err", err)
		}
	}
}
