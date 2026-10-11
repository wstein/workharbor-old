package notify

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wstein/workharbor/internal/domain"
)

func TestWatchdogQuotaAndFinishedPushesShareThrottleAndGenericPayload(t *testing.T) {
	events := []domain.Event{
		{TaskID: "task", Kind: "tool.hung", Payload: []byte(`{"run_id":"run","generation":"PRIVATE-GENERATION","tool_call_id":"PRIVATE-TOOL","episode":1}`)},
		{TaskID: "task", Kind: "tool.aborted", Payload: []byte(`{"run_id":"run","generation":"PRIVATE-GENERATION","tool_call_id":"PRIVATE-TOOL","episode":1}`)},
		{TaskID: "task", Kind: domain.EventDecisionRaised, Payload: []byte(`{"id":"decision","kind":"question","blocking":true,"cause":"quota_exhausted","subject":"PRIVATE-QUOTA"}`)},
		{TaskID: "task", Kind: domain.EventRunState, Payload: []byte(`{"object":"run","id":"run","from":"running","to":"stopped"}`)},
	}
	msgs := FromEvents(events)
	want := []Kind{"tool_hung", "tool_aborted", KindQuotaExhausted, KindRunEnded}
	if len(msgs) != len(want) {
		t.Fatalf("messages=%+v", msgs)
	}
	srv, requests := server(t, http.StatusOK)
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	channel := Throttled{Next: Ntfy{Server: srv.URL, BaseURL: "https://host", Secrets: secrets{SecretTopic: topic}}, Throttle: &Throttle{Now: func() time.Time { return clock }}}
	for i, m := range msgs {
		if m.Kind != want[i] {
			t.Fatalf("kind=%s want=%s", m.Kind, want[i])
		}
		for range 2 {
			if err := channel.Notify(context.Background(), m); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(*requests) != len(want) {
		t.Fatalf("duplicate delivery: %d requests", len(*requests))
	}
	for i, r := range *requests {
		if r.body != string(want[i])+": task task" {
			t.Fatalf("nongeneric body=%q", r.body)
		}
		if !strings.HasPrefix(r.header.Get("Click"), "https://host/tasks/task") {
			t.Fatal("missing task link")
		}
		for name, values := range r.header {
			if strings.Contains(name+strings.Join(values, ""), "PRIVATE-") {
				t.Fatal("private tool/quota data reached notification headers")
			}
		}
	}
	// Four kinds consumed the same task's hourly quota; only one more fits.
	if err := channel.Notify(context.Background(), Message{TaskID: "task", Kind: KindApproval, DecisionID: "last"}); err != nil {
		t.Fatal(err)
	}
	if err := channel.Notify(context.Background(), Message{TaskID: "task", Kind: KindQuestion, DecisionID: "over"}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 5 {
		t.Fatalf("watchdog kinds bypassed shared task quota: %d", len(*requests))
	}
}

func TestMalformedWatchdogNoticesDoNotNotify(t *testing.T) {
	for _, payload := range []string{`not json`, `{}`, `{"run_id":""}`} {
		if msgs := FromEvents([]domain.Event{{TaskID: "task", Kind: "tool.hung", Payload: []byte(payload)}}); len(msgs) != 0 {
			t.Fatal("malformed watchdog notice notified")
		}
	}
}
