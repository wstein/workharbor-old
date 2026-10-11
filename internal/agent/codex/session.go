package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wstein/workharbor/internal/agent"
)

type session struct {
	client         *client
	state          *state
	spec           agent.StartSpec
	events         chan agent.Event
	done           chan struct{}
	reaped         <-chan struct{}
	commandOutput  *commandOutputTranslator
	generation     string
	approvals      sync.WaitGroup
	open           chan struct{}
	approvalCtx    context.Context
	approvalCancel context.CancelFunc
	order          sync.Mutex
	mu             sync.Mutex
	result         agent.Result
	err            error
	stopped        bool
	parent         context.Context
}

func newSession(parent context.Context, client *client, state *state, spec agent.StartSpec, reaped <-chan struct{}) (*session, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	generation := hex.EncodeToString(nonce[:])
	commandOutput, err := newCommandOutputTranslator(state, generation)
	if err != nil {
		return nil, err
	}
	approvalCtx, approvalCancel := context.WithCancel(client.ctx)
	result := &session{parent: parent, client: client, state: state, spec: spec, events: make(chan agent.Event, 256), done: make(chan struct{}), reaped: reaped, generation: generation, commandOutput: commandOutput, open: make(chan struct{}, 8), approvalCtx: approvalCtx, approvalCancel: approvalCancel}
	go result.run()
	return result, nil
}

func (session *session) emit(event agent.Event) bool {
	session.order.Lock()
	defer session.order.Unlock()
	return session.emitLocked(event)
}

func (session *session) emitLocked(event agent.Event) bool {
	select {
	case session.events <- event:
		return true
	default:
		session.client.fail(errProtocol)
		return false
	}
}

func (session *session) run() {
	defer close(session.done)
	defer close(session.events)
	session.emit(session.state.event(agent.EventSession))
	for value := range session.client.incoming {
		if errors.Is(session.client.failure(), errProtocol) {
			break
		}
		session.order.Lock()
		if value.ID != nil {
			ok := session.approve(value)
			session.order.Unlock()
			if !ok {
				break
			}
			continue
		}
		if value.Method == "thread/started" {
			var notification struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if decodeExact(value.Params, &notification) != nil || notification.Thread.ID != session.state.thread {
				session.client.fail(errProtocol)
				session.order.Unlock()
				break
			}
			session.order.Unlock()
			continue
		}
		events, err := session.state.observe(value)
		if err != nil {
			session.client.fail(err)
			session.order.Unlock()
			break
		}
		for _, event := range events {
			session.emitLocked(event)
		}
		terminal := session.state.result != nil
		session.order.Unlock()
		if terminal {
			break
		}
	}
	session.approvalCancel()
	session.client.close()
	session.approvals.Wait()
	<-session.reaped
	session.mu.Lock()
	defer session.mu.Unlock()
	switch {
	case session.state.result != nil:
		session.result = *session.state.result
	case session.stopped || session.parent.Err() != nil:
		session.result = agent.Result{Status: agent.ResultStopped, SessionID: session.state.thread}
	default:
		session.result = agent.Result{Status: agent.ResultFailed, SessionID: session.state.thread}
		session.err = session.client.failure()
	}
}

func (session *session) approve(value message) bool {
	req, err := session.state.approval(value)
	if err != nil {
		ctx, cancel := context.WithTimeout(session.client.ctx, time.Second)
		defer cancel()
		_ = session.client.write(ctx, message{ID: value.ID, Error: &rpcError{Code: -32602, Message: "unsupported or stale approval request"}})
		session.client.fail(errProtocol)
		return false
	}
	req.ID = session.generation + ":" + session.state.thread + ":" + session.state.turn + ":" + req.ID
	var identity struct {
		ItemID string `json:"itemId"`
	}
	if decodeExact(value.Params, &identity) != nil {
		session.client.fail(errProtocol)
		return false
	}
	select {
	case session.open <- struct{}{}:
	default:
		session.client.fail(errProtocol)
		return false
	}
	session.approvals.Add(1)
	go func() {
		defer session.approvals.Done()
		defer func() { <-session.open }()
		answer := agent.Ask(session.approvalCtx, session.spec.Approver, session.spec.ApprovalTimeout, req)
		session.order.Lock()
		defer session.order.Unlock()
		if session.state.result != nil || session.state.completed[identity.ItemID] {
			answer = agent.Approval{Reason: "approval no longer pending: denied"}
		}
		decision := "decline"
		if answer.Allow {
			decision = "accept"
		}
		ctx, cancel := context.WithTimeout(session.approvalCtx, time.Second)
		defer cancel()
		encoded, _ := json.Marshal(map[string]string{"decision": decision})
		if session.approvalCtx.Err() != nil || session.client.write(ctx, message{ID: value.ID, Result: encoded}) != nil {
			answer = agent.Approval{Reason: "approval channel lost: denied"}
			session.client.close()
		}
		event := session.state.event(agent.EventApproval)
		event.Tool = req.Tool
		event.Input = boundedText(req.Input)
		event.Approval = &agent.ApprovalRecord{ID: req.ID, Allow: answer.Allow, Reason: answer.Reason}
		session.emitLocked(event)
	}()
	return true
}

func (session *session) ID() string                 { return session.state.thread }
func (session *session) Events() <-chan agent.Event { return session.events }

func (session *session) Instruct(ctx context.Context, text string) (agent.Delivery, error) {
	select {
	case <-session.done:
		return "", agent.ErrNotRunning
	default:
	}
	response, err := session.client.call(ctx, "turn/steer", map[string]any{"threadId": session.state.thread, "expectedTurnId": session.state.turn, "input": textInput(text)})
	if err != nil {
		return "", err
	}
	var result struct {
		TurnID string `json:"turnId"`
	}
	if decodeExact(response, &result) != nil || result.TurnID != session.state.turn {
		session.client.fail(errProtocol)
		return "", errProtocol
	}
	return agent.DeliveryNextTurn, nil
}

func (session *session) Stop(ctx context.Context) error {
	session.mu.Lock()
	session.stopped = true
	session.mu.Unlock()
	session.approvalCancel()
	select {
	case <-session.done:
		return nil
	default:
	}
	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, _ = session.client.call(stopCtx, "turn/interrupt", map[string]string{"threadId": session.state.thread, "turnId": session.state.turn})
	session.client.close()
	return nil
}

func (session *session) Wait() (agent.Result, error) {
	<-session.done
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.result, session.err
}
