package store

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/kokorhekkus/netwatch/internal/metrics"
)

// Sample is one probe result on its way to disk.
type Sample struct {
	Kind     SampleKind
	TargetID int64
	EpochID  int64
	TSMicro  int64
	ValueUS  sql.NullInt64
	Outcome  metrics.Outcome
	Load     int
}

type SampleKind uint8

const (
	SampleICMP SampleKind = iota
	SampleTCP
)

const (
	writeQueueDepth = 4096
	flushInterval   = 2 * time.Second
	flushRows       = 500
)

// Writer batches samples onto disk from a single goroutine.
//
// The queue is bounded and lossy on purpose. A probe that blocks waiting for
// the disk stops measuring the network, and a monitoring tool that distorts
// its own measurements under load is worse than one with a small hole in its
// data - so an overfull queue drops samples and counts the drops.
type Writer struct {
	db      *DB
	ch      chan Sample
	dropped atomic.Uint64
	done    chan struct{}
	log     *slog.Logger
}

func NewWriter(db *DB, log *slog.Logger) *Writer {
	return &Writer{
		db:   db,
		ch:   make(chan Sample, writeQueueDepth),
		done: make(chan struct{}),
		log:  log,
	}
}

// Submit enqueues a sample, dropping it if the queue is full.
func (w *Writer) Submit(s Sample) {
	select {
	case w.ch <- s:
	default:
		w.dropped.Add(1)
	}
}

func (w *Writer) Dropped() uint64 { return w.dropped.Load() }

// Run drains the queue until ctx is cancelled, then flushes what is left.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]Sample, 0, flushRows)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.commit(batch); err != nil {
			w.log.Error("write batch failed", "rows", len(batch), "err", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is already queued so a clean shutdown keeps its
			// last couple of seconds of data.
			for {
				select {
				case s := <-w.ch:
					batch = append(batch, s)
					if len(batch) >= flushRows {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return

		case s := <-w.ch:
			batch = append(batch, s)
			if len(batch) >= flushRows {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}

// Wait blocks until Run has finished flushing.
func (w *Writer) Wait() { <-w.done }

func (w *Writer) commit(batch []Sample) error {
	tx, err := w.db.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	icmpStmt, err := tx.Prepare(`INSERT OR REPLACE INTO icmp_raw
		(target_id, ts_us, epoch_id, rtt_us, outcome, load) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer icmpStmt.Close()

	tcpStmt, err := tx.Prepare(`INSERT OR REPLACE INTO tcp_raw
		(target_id, ts_us, epoch_id, connect_us, outcome, load) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer tcpStmt.Close()

	for _, s := range batch {
		stmt := icmpStmt
		if s.Kind == SampleTCP {
			stmt = tcpStmt
		}
		if _, err := stmt.Exec(s.TargetID, s.TSMicro, s.EpochID,
			s.ValueUS, int(s.Outcome), s.Load); err != nil {
			return err
		}
	}
	return tx.Commit()
}
