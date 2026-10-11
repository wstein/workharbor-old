package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/runtime"
)

// Runner provides the runtime's environment-local stdio execution channel.
// Process-tree cancellation and whr-shim wrapping are runtime responsibilities.
type Runner interface {
	Exec(context.Context, string, runtime.ExecRequest) (runtime.ExecStream, error)
}

// Config selects the native executable and explicit model/effort identifiers.
// It does not establish artifact verification, model access or isolation.
type Config struct {
	Bin    string
	Model  string
	Effort string
}

// Adapter retains the offline native protocol foundation. Production start and
// resume refuse until the pinned target's mandatory D53 gates are implemented
// and independently measured. There is no configuration switch to bypass them.
type Adapter struct {
	runner Runner
	config Config
}

// New constructs an adapter without claiming unmeasured native capabilities.
func New(runner Runner, config Config) *Adapter { return &Adapter{runner: runner, config: config} }

// Name implements agent.Adapter.
func (*Adapter) Name() string { return "codex" }

// Capabilities implements agent.Adapter. Offline fixtures are not capability
// evidence; native headless execution and production auth remain unverified.
func (*Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{ContractVersion: agent.ContractVersion, ToolWatchdog: false}
}

func productionGate() error {
	return fmt.Errorf("%w: Codex target artifact, schema, configuration isolation, child admission and native model/effort measurement gates are incomplete", agent.ErrUnsupported)
}

// Start implements agent.Adapter and refuses unmeasured mandatory controls.
func (*Adapter) Start(context.Context, agent.StartSpec) (agent.Session, error) {
	return nil, productionGate()
}

// Resume implements agent.Adapter and refuses unmeasured mandatory controls.
func (*Adapter) Resume(context.Context, agent.StartSpec, string) (agent.Session, error) {
	return nil, productionGate()
}

func (adapter *Adapter) launch(ctx context.Context, spec agent.StartSpec, resume string) (agent.Session, error) {
	if adapter.runner == nil || adapter.config.Bin == "" || adapter.config.Model == "" || adapter.config.Effort == "" || !strings.HasPrefix(spec.Workdir, "/") || strings.TrimSpace(spec.Prompt) == "" || spec.Auth != agent.AuthSubscription {
		return nil, agent.ErrBadSpec
	}
	if spec.Mode() != agent.PermissionManual || len(spec.AllowedTools) > 0 {
		return nil, agent.ErrUnsupported
	}
	if spec.Approver == nil {
		return nil, agent.ErrNoApprover
	}
	if resume != "" && !validID(resume) {
		return nil, agent.ErrNoSession
	}
	processCtx, cancel := context.WithCancel(ctx)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	stream, err := adapter.runner.Exec(processCtx, spec.EnvID, runtime.ExecRequest{Cmd: []string{adapter.config.Bin, "app-server", "--listen", "stdio://"}, Dir: spec.Workdir, Env: append([]string(nil), spec.Env...), Stdin: inputReader})
	if err != nil {
		cancel()
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
		return nil, err
	}
	connection := newClient(processCtx, outputReader, inputWriter)
	reaped := make(chan struct{})
	go func() { <-connection.ctx.Done(); cancel(); _ = inputReader.Close() }()
	go func() {
		defer close(reaped)
		broken := false
		for chunk := range stream.Chunks() {
			if chunk.Stream == runtime.Stdout && !broken {
				if _, err := outputWriter.Write(chunk.Data); err != nil {
					broken = true
					cancel()
				}
			}
		}
		_, _ = stream.Wait()
		_ = outputWriter.Close()
	}()
	startup, startupCancel := context.WithTimeout(ctx, 15*time.Second)
	defer startupCancel()
	refuse := func(err error) (agent.Session, error) { connection.fail(err); <-reaped; return nil, err }
	initialized, err := connection.call(startup, "initialize", map[string]any{"clientInfo": map[string]string{"name": "workharbor", "title": "workharbor", "version": "1"}})
	if err != nil {
		return refuse(err)
	}
	if object(initialized, "userAgent", "codexHome", "platformFamily", "platformOs") != nil {
		return refuse(errProtocol)
	}
	if err := connection.write(startup, message{Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		return refuse(err)
	}
	params := map[string]any{"model": adapter.config.Model, "cwd": spec.Workdir, "approvalPolicy": "untrusted", "approvalsReviewer": "user", "sandbox": "read-only", "config": map[string]string{"model_reasoning_effort": adapter.config.Effort}}
	method := "thread/start"
	if resume != "" {
		method = "thread/resume"
		params["threadId"] = resume
	}
	response, err := connection.call(startup, method, params)
	if err != nil {
		return refuse(err)
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model    string `json:"model"`
		Effort   string `json:"reasoningEffort"`
		Cwd      string `json:"cwd"`
		Policy   string `json:"approvalPolicy"`
		Reviewer string `json:"approvalsReviewer"`
		Sandbox  struct {
			Type string `json:"type"`
		} `json:"sandbox"`
		Sources []string `json:"instructionSources"`
	}
	if decodeExact(response, &thread) != nil || !validID(thread.Thread.ID) || thread.Model != adapter.config.Model || thread.Effort != adapter.config.Effort || thread.Cwd != spec.Workdir || thread.Policy != "untrusted" || thread.Reviewer != "user" || thread.Sandbox.Type != "readOnly" || len(thread.Sources) > 0 || (resume != "" && thread.Thread.ID != resume) {
		return refuse(errProtocol)
	}
	response, err = connection.call(startup, "turn/start", map[string]any{"threadId": thread.Thread.ID, "model": adapter.config.Model, "effort": adapter.config.Effort, "input": textInput(spec.Prompt)})
	if err != nil {
		return refuse(err)
	}
	var turn struct {
		Turn nativeTurn `json:"turn"`
	}
	if decodeExact(response, &turn) != nil || !validID(turn.Turn.ID) || turn.Turn.Status != "inProgress" || turn.Turn.Error != nil {
		return refuse(errProtocol)
	}
	state := newState(thread.Thread.ID, turn.Turn.ID, adapter.config.Model)
	if state.validateSnapshot(turn.Turn) != nil {
		return refuse(errProtocol)
	}
	session, err := newSession(ctx, connection, state, spec, reaped)
	if err != nil {
		return refuse(err)
	}
	return session, nil
}

func textInput(text string) []map[string]string {
	return []map[string]string{{"type": "text", "text": text}}
}
