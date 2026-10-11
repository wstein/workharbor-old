package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wstein/workharbor/internal/domain"
)

// Append writes events to the log in this transaction and returns them with
// their sequence numbers. An event without a time is stamped with the
// store's clock.
func (tx *Tx) Append(ctx context.Context, events ...domain.Event) ([]domain.Event, error) {
	out := make([]domain.Event, 0, len(events))
	for _, e := range events {
		if e.Kind == "" || e.TaskID == "" {
			return nil, errors.New("store: an event needs a kind and a task")
		}
		if e.Tier != domain.TierAudit && e.Tier != domain.TierTranscript {
			return nil, fmt.Errorf("store: event %s has unknown tier %q", e.Kind, e.Tier)
		}
		if e.At.IsZero() {
			e.At = tx.s.now()
		}
		payload := e.Payload
		if payload == nil {
			payload = []byte("{}")
		}
		payload = tx.s.redactor.Bytes(payload) // audit entries are never purged: redact before writing
		res, err := tx.tx.ExecContext(ctx, `INSERT INTO events (task_id, kind, tier, payload, at) VALUES (?, ?, ?, ?, ?)`,
			string(e.TaskID), string(e.Kind), string(e.Tier), payload, e.At.UnixNano())
		if err != nil {
			return nil, fmt.Errorf("store: append %s: %w", e.Kind, err)
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("store: append %s: %w", e.Kind, err)
		}
		e.Seq, e.Payload = seq, payload
		if e.Tier == domain.TierAudit {
			if err := tx.chain(ctx, seq, e, payload); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Redact applies the store's redactor to bytes that are never written, such as
// an ephemeral event's payload, so a client sees no more than the store keeps.
func (s *Store) Redact(b []byte) []byte { return s.redactor.Bytes(b) }

// Append writes events in their own transaction.
func (s *Store) Append(ctx context.Context, events ...domain.Event) ([]domain.Event, error) {
	var out []domain.Event
	err := s.Update(ctx, func(tx *Tx) error {
		var err error
		out, err = tx.Append(ctx, events...)
		return err
	})
	return out, err
}

// EventsSince returns the events after sequence number since, oldest first, at
// most limit of them (1000 if limit is not positive). An empty task returns
// events of every task. A client resumes a stream with the last Seq it saw.
func (s *Store) EventsSince(ctx context.Context, task domain.ID, since int64, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := `SELECT seq, task_id, kind, tier, payload, at FROM events WHERE seq > ?`
	args := []any{since}
	if task != "" {
		query += ` AND task_id = ?`
		args = append(args, string(task))
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var taskID, kind, tier string
		var at int64
		if err := rows.Scan(&e.Seq, &taskID, &kind, &tier, &e.Payload, &at); err != nil {
			return nil, fmt.Errorf("store: events: %w", err)
		}
		e.TaskID, e.Kind, e.Tier, e.At = domain.ID(taskID), domain.EventKind(kind), domain.Tier(tier), time.Unix(0, at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeSpec says which transcript content of a task to delete. At least one of
// All, Before and MaxBytes must be set; Before and MaxBytes together delete
// what is older than Before and then the oldest of the rest until the
// transcript is no larger than MaxBytes.
type PurgeSpec struct {
	TaskID   domain.ID
	Actor    string    // who purges, recorded in the audit entry
	All      bool      // delete all transcript content
	Before   time.Time // delete transcript content older than this
	MaxBytes int64     // keep at most this many bytes of transcript content
}

// PurgeResult says what a purge removed.
type PurgeResult struct {
	Events int
	Bytes  int64
	// Digest is a SHA-256 over the removed events, in order: "seq:length:"
	// followed by the payload of each. The audit entry holds it, so a purge leaves a
	// verifiable gap and never silently rewrites history.
	Digest string
	Audit  domain.Event
}

// PurgePayload is the payload of the audit entry a purge writes.
type PurgePayload struct {
	Actor  string    `json:"actor"`
	TaskID string    `json:"task_id"`
	Events int       `json:"events"`
	Bytes  int64     `json:"bytes"`
	Digest string    `json:"digest"`
	At     time.Time `json:"at"`
}

// Purge deletes transcript content and records itself: in the same
// transaction it appends one audit entry with who, when, how many events and
// bytes, and a digest of what was removed. Audit entries are never touched,
// and sequence numbers are never reused.
func (s *Store) Purge(ctx context.Context, spec PurgeSpec) (PurgeResult, error) {
	if spec.TaskID == "" || spec.Actor == "" {
		return PurgeResult{}, errors.New("store: a purge needs a task and an actor")
	}
	if !spec.All && spec.Before.IsZero() && spec.MaxBytes <= 0 {
		return PurgeResult{}, errors.New("store: a purge needs All, Before or MaxBytes")
	}
	var res PurgeResult
	err := s.Update(ctx, func(tx *Tx) error {
		type row struct {
			seq     int64
			payload []byte
			at      int64
		}
		rows, err := tx.tx.QueryContext(ctx, `SELECT seq, payload, at FROM events WHERE task_id = ? AND tier = 'transcript' ORDER BY seq`, string(spec.TaskID))
		if err != nil {
			return fmt.Errorf("store: purge: %w", err)
		}
		var all []row
		var total int64
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.seq, &r.payload, &r.at); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: purge: %w", err)
			}
			all = append(all, r)
			total += int64(len(r.payload))
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: purge: %w", err)
		}

		doomed := make([]bool, len(all))
		remaining := total
		for i, r := range all {
			if spec.All || (!spec.Before.IsZero() && r.at < spec.Before.UnixNano()) {
				doomed[i] = true
				remaining -= int64(len(r.payload))
			}
		}
		if spec.MaxBytes > 0 {
			for i, r := range all {
				if remaining <= spec.MaxBytes {
					break
				}
				if !doomed[i] {
					doomed[i] = true
					remaining -= int64(len(r.payload))
				}
			}
		}

		h := sha256.New()
		for i, r := range all {
			if !doomed[i] {
				continue
			}
			if _, err := tx.tx.ExecContext(ctx, `DELETE FROM events WHERE seq = ?`, r.seq); err != nil {
				return fmt.Errorf("store: purge: %w", err)
			}
			fmt.Fprintf(h, "%d:%d:", r.seq, len(r.payload))
			h.Write(r.payload)
			res.Events++
			res.Bytes += int64(len(r.payload))
		}
		res.Digest = hex.EncodeToString(h.Sum(nil))

		at := s.now()
		payload, err := json.Marshal(PurgePayload{Actor: spec.Actor, TaskID: string(spec.TaskID), Events: res.Events, Bytes: res.Bytes, Digest: res.Digest, At: at})
		if err != nil {
			return err
		}
		appended, err := tx.Append(ctx, domain.Event{TaskID: spec.TaskID, Kind: domain.EventPurged, Tier: domain.TierAudit, Payload: payload, At: at})
		if err != nil {
			return err
		}
		res.Audit = appended[0]
		return nil
	})
	if err != nil {
		return PurgeResult{}, err
	}
	return res, nil
}

// TranscriptSize returns how many transcript events a task has and their bytes:
// what a purge of all of them would delete.
func (s *Store) TranscriptSize(ctx context.Context, task domain.ID) (events int, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(payload)), 0) FROM events WHERE task_id = ? AND tier = 'transcript'`, string(task)).Scan(&events, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("store: transcript size: %w", err)
	}
	return events, bytes, nil
}

// EventsOfKind returns a task's events of one kind, oldest first.
func (s *Store) EventsOfKind(ctx context.Context, task domain.ID, kind domain.EventKind) ([]domain.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, task_id, kind, tier, payload, at FROM events
		WHERE task_id = ? AND kind = ? ORDER BY seq`, string(task), string(kind))
	if err != nil {
		return nil, fmt.Errorf("store: events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var taskID, k, tier string
		var at int64
		if err := rows.Scan(&e.Seq, &taskID, &k, &tier, &e.Payload, &at); err != nil {
			return nil, fmt.Errorf("store: events: %w", err)
		}
		e.TaskID, e.Kind, e.Tier, e.At = domain.ID(taskID), domain.EventKind(k), domain.Tier(tier), time.Unix(0, at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestEventSeq returns the latest durable sequence for one task, or zero.
// It lets live observers distinguish history from events after attachment.
func (s *Store) LatestEventSeq(ctx context.Context, task domain.ID) (int64, error) {
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE task_id = ?`, string(task)).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: latest event sequence: %w", err)
	}
	return seq, nil
}
