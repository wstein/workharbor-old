package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/domain"
)

func audit(task domain.ID, kind domain.EventKind) domain.Event {
	return domain.Event{TaskID: task, Kind: kind, Tier: domain.TierAudit, Payload: []byte(`{"n":1}`)}
}

func transcript(task domain.ID, text string) domain.Event {
	return domain.Event{TaskID: task, Kind: "message", Tier: domain.TierTranscript, Payload: []byte(text)}
}

func TestAppendAssignsMonotonicSequenceAndStampsTheTime(t *testing.T) {
	fixed := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s := openTemp(t, WithClock(func() time.Time { return fixed }))
	given := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	out, err := s.Append(bg, audit("t1", domain.EventTaskState), domain.Event{TaskID: "t1", Kind: "x", Tier: domain.TierAudit, At: given})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Seq != 1 || out[1].Seq != 2 {
		t.Fatalf("appended = %+v, want sequence numbers 1 and 2", out)
	}
	if !out[0].At.Equal(fixed) || !out[1].At.Equal(given) {
		t.Errorf("times %v and %v; want the clock for the first and the given one for the second", out[0].At, out[1].At)
	}
	more, err := s.Append(bg, audit("t2", domain.EventRunState))
	if err != nil || more[0].Seq != 3 {
		t.Fatalf("a later append = %+v, %v; want sequence 3", more, err)
	}
}

func TestAppendRefusesBadEvents(t *testing.T) {
	s := openTemp(t)
	for name, e := range map[string]domain.Event{
		"no kind":  {TaskID: "t1", Tier: domain.TierAudit},
		"no task":  {Kind: "x", Tier: domain.TierAudit},
		"no tier":  {TaskID: "t1", Kind: "x"},
		"bad tier": {TaskID: "t1", Kind: "x", Tier: "forever"},
	} {
		if _, err := s.Append(bg, e); err == nil {
			t.Errorf("%s: the event was accepted", name)
		}
	}
	// A refused event in a batch leaves the whole batch unwritten.
	if _, err := s.Append(bg, audit("t1", "ok"), domain.Event{TaskID: "t1", Kind: "x", Tier: "forever"}); err == nil {
		t.Fatal("a bad event in a batch was accepted")
	}
	if got, _ := s.EventsSince(bg, "", 0, 0); len(got) != 0 {
		t.Errorf("a refused batch left %d events behind", len(got))
	}
}

func TestEventsSinceReplaysWhatTheClientMissed(t *testing.T) {
	s := openTemp(t)
	for i := range 5 {
		task := domain.ID("t1")
		if i%2 == 1 {
			task = "t2"
		}
		if _, err := s.Append(bg, audit(task, domain.EventRunState)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.EventsSince(bg, "", 0, 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("all events = %d, %v", len(all), err)
	}
	for i, e := range all {
		if e.Seq != int64(i+1) {
			t.Errorf("event %d has Seq %d", i, e.Seq)
		}
	}
	after, _ := s.EventsSince(bg, "", 3, 0)
	if len(after) != 2 || after[0].Seq != 4 {
		t.Errorf("after 3 = %+v, want 4 and 5", after)
	}
	t1, _ := s.EventsSince(bg, "t1", 0, 0)
	if len(t1) != 3 {
		t.Errorf("task t1 has %d events, want 3", len(t1))
	}
	limited, _ := s.EventsSince(bg, "", 0, 2)
	if len(limited) != 2 || limited[1].Seq != 2 {
		t.Errorf("limit 2 = %+v", limited)
	}
	if none, _ := s.EventsSince(bg, "", 5, 0); len(none) != 0 {
		t.Errorf("after the last event there is %d", len(none))
	}
	if got := all[0]; string(got.Payload) != `{"n":1}` || got.Tier != domain.TierAudit || got.Kind != domain.EventRunState || got.TaskID != "t1" {
		t.Errorf("a stored event came back as %+v", got)
	}
}

func TestAuditEventsCannotBeChangedOrDeleted(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Append(bg, audit("t1", domain.EventTaskState), transcript("t1", "hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(bg, `UPDATE events SET kind = 'forged' WHERE seq = 1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("updating an audit event = %v, want the append-only trigger", err)
	}
	if _, err := s.db.ExecContext(bg, `UPDATE events SET payload = x'00' WHERE seq = 2`); err == nil {
		t.Error("updating a transcript event must be refused too")
	}
	if _, err := s.db.ExecContext(bg, `DELETE FROM events WHERE seq = 1`); err == nil || !strings.Contains(err.Error(), "never deleted") {
		t.Errorf("deleting an audit event = %v, want the trigger", err)
	}
	if _, err := s.db.ExecContext(bg, `DELETE FROM events WHERE task_id = 't1'`); err == nil {
		t.Error("a bulk delete that includes audit rows must be refused")
	}
	if got, _ := s.EventsSince(bg, "", 0, 0); len(got) != 2 {
		t.Errorf("%d events left, want 2", len(got))
	}
}

func digest(rows ...struct {
	seq     int64
	payload string
},
) string {
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%d:%d:", r.seq, len(r.payload))
		h.Write([]byte(r.payload))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestPurgeDeletesTranscriptAndRecordsItself(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s := openTemp(t, WithClock(func() time.Time { return now }))
	if _, err := s.Append(bg,
		audit("t1", domain.EventTaskState),                    // 1
		transcript("t1", "first message"),                     // 2
		transcript("t1", "second message"),                    // 3
		transcript("t2", "another task"),                      // 4
		audit("t1", domain.EventDecisionRaised)); err != nil { // 5
		t.Fatal(err)
	}

	res, err := s.Purge(bg, PurgeSpec{TaskID: "t1", Actor: "werner", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 2 || res.Bytes != int64(len("first message")+len("second message")) {
		t.Errorf("purged %d events and %d bytes", res.Events, res.Bytes)
	}
	want := digest(
		struct {
			seq     int64
			payload string
		}{2, "first message"},
		struct {
			seq     int64
			payload string
		}{3, "second message"})
	if res.Digest != want {
		t.Errorf("digest %s, want %s", res.Digest, want)
	}

	left, _ := s.EventsSince(bg, "", 0, 0)
	var kinds []domain.EventKind
	for _, e := range left {
		kinds = append(kinds, e.Kind)
	}
	// The audit entries of t1 stay, the other task's transcript stays, and the purge is recorded.
	if len(left) != 4 || kinds[0] != domain.EventTaskState || kinds[1] != "message" || kinds[2] != domain.EventDecisionRaised || kinds[3] != domain.EventPurged {
		t.Fatalf("events after the purge = %v", kinds)
	}
	if left[1].TaskID != "t2" {
		t.Errorf("the other task's transcript was purged")
	}
	entry := left[3]
	if entry.Tier != domain.TierAudit || entry.Seq != 6 || res.Audit.Seq != 6 {
		t.Errorf("audit entry = %+v; its sequence must continue after the gap, never reuse numbers", entry)
	}
	var p PurgePayload
	if err := json.Unmarshal(entry.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Actor != "werner" || p.TaskID != "t1" || p.Events != 2 || p.Bytes != res.Bytes || p.Digest != want || !p.At.Equal(now) {
		t.Errorf("purge payload = %+v", p)
	}
}

func TestPurgeByAgeAndBySize(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := openTemp(t)
	for i, text := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		e := transcript("t1", text)
		e.At = base.Add(time.Duration(i) * time.Hour)
		if _, err := s.Append(bg, e); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.Purge(bg, PurgeSpec{TaskID: "t1", Actor: "retention", Before: base.Add(90 * time.Minute)})
	if err != nil || res.Events != 2 || res.Bytes != 8 {
		t.Fatalf("by age: %+v, %v; want the two events before 01:30", res, err)
	}
	// 8 bytes remain; keep at most 5: the older of the two goes.
	res, err = s.Purge(bg, PurgeSpec{TaskID: "t1", Actor: "retention", MaxBytes: 5})
	if err != nil || res.Events != 1 || res.Bytes != 4 {
		t.Fatalf("by size: %+v, %v; want the oldest of the rest", res, err)
	}
	rest, _ := s.EventsSince(bg, "t1", 0, 0)
	var texts []string
	for _, e := range rest {
		if e.Tier == domain.TierTranscript {
			texts = append(texts, string(e.Payload))
		}
	}
	if len(texts) != 1 || texts[0] != "dddd" {
		t.Errorf("transcript left = %v, want only the newest", texts)
	}

	// Both limits at once.
	s2 := openTemp(t)
	for i, text := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		e := transcript("t1", text)
		e.At = base.Add(time.Duration(i) * time.Hour)
		if _, err := s2.Append(bg, e); err != nil {
			t.Fatal(err)
		}
	}
	res, err = s2.Purge(bg, PurgeSpec{TaskID: "t1", Actor: "retention", Before: base.Add(30 * time.Minute), MaxBytes: 8})
	if err != nil || res.Events != 2 || res.Bytes != 8 {
		t.Errorf("age then size: %+v, %v; want one by age and one by size", res, err)
	}
}

func TestPurgeNeedsACriterionATaskAndAnActor(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Append(bg, transcript("t1", "keep me")); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]PurgeSpec{
		"no criterion": {TaskID: "t1", Actor: "werner"},
		"no actor":     {TaskID: "t1", All: true},
		"no task":      {Actor: "werner", All: true},
	} {
		if _, err := s.Purge(bg, spec); err == nil {
			t.Errorf("%s: the purge ran", name)
		}
	}
	if got, _ := s.EventsSince(bg, "", 0, 0); len(got) != 1 {
		t.Errorf("a refused purge changed the log: %d events", len(got))
	}
}

func TestPurgeOfNothingStillRecordsItself(t *testing.T) {
	s := openTemp(t)
	res, err := s.Purge(bg, PurgeSpec{TaskID: "t1", Actor: "werner", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 0 || res.Bytes != 0 || res.Audit.Kind != domain.EventPurged {
		t.Errorf("an empty purge = %+v, want zero counts and an audit entry", res)
	}
}

func TestSequenceNumbersAreNeverReused(t *testing.T) {
	s := openTemp(t)
	first, _ := s.Append(bg, transcript("t1", "x"))
	if _, err := s.Purge(bg, PurgeSpec{TaskID: "t1", Actor: "a", All: true}); err != nil {
		t.Fatal(err)
	}
	next, _ := s.Append(bg, transcript("t1", "y"))
	if next[0].Seq <= first[0].Seq+1 {
		t.Errorf("seq %d after purging %d and writing its audit entry; numbers must not be reused", next[0].Seq, first[0].Seq)
	}
}

func TestATransactionThatFailsWritesNoEvents(t *testing.T) {
	s := openTemp(t)
	err := s.Update(context.Background(), func(tx *Tx) error {
		if _, err := tx.Append(bg, audit("t1", "a")); err != nil {
			return err
		}
		return context.Canceled // anything after the append fails
	})
	if err == nil {
		t.Fatal("the update should have failed")
	}
	if got, _ := s.EventsSince(bg, "", 0, 0); len(got) != 0 {
		t.Errorf("a rolled-back transaction left %d events", len(got))
	}
}

func TestLatestEventSeqIsTaskScopedAndEmptyIsZero(t *testing.T) {
	s := openTemp(t)
	seq, err := s.LatestEventSeq(bg, "t1")
	if err != nil || seq != 0 {
		t.Fatalf("empty watermark = %d, %v", seq, err)
	}
	saved, err := s.Append(bg, audit("t1", domain.EventRunState), audit("t2", domain.EventRunState))
	if err != nil {
		t.Fatal(err)
	}
	seq, err = s.LatestEventSeq(bg, "t1")
	if err != nil || seq != saved[0].Seq {
		t.Fatalf("task watermark = %d, %v", seq, err)
	}
	more, err := s.Append(bg, audit("t1", domain.EventTaskState))
	if err != nil {
		t.Fatal(err)
	}
	seq, err = s.LatestEventSeq(bg, "t1")
	if err != nil || seq != more[0].Seq {
		t.Fatalf("advanced watermark = %d, %v", seq, err)
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	if _, err := s.LatestEventSeq(cancelled, "t1"); err == nil {
		t.Fatal("cancelled watermark read succeeded")
	}
}
