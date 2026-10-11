package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wstein/workharbor/internal/agent"
	"github.com/wstein/workharbor/internal/runtime"
)

// Runner is what the adapter needs of a runtime: Exec with stdin. Every
// runtime.Adapter is one.
type Runner interface {
	Exec(ctx context.Context, envID string, req runtime.ExecRequest) (runtime.ExecStream, error)
}

// Config is what the adapter needs to know about the installation.
type Config struct {
	Bin       string // the claude binary in the environment; default "claude"
	Model     string // optional --model
	ConfigDir string // a per-environment auth directory, passed as CLAUDE_CONFIG_DIR, never $HOME
	// Settings is the supervisor's own settings, a JSON object passed inline as
	// --settings. The CLI reads no other settings, hooks, MCP servers or
	// skills: not the repository's and not the agent home's, because the agent
	// can write both (design §5.2, spike/claude-config). Default "{}".
	Settings string
	Env      []string // extra KEY=VALUE for the process
	// ResumeProbe is how long Resume waits to learn whether the session
	// exists. Default 10 s.
	ResumeProbe time.Duration
	// InstructionCheckTimeout bounds the instruction-file check. Default
	// instructionCheckTimeout.
	InstructionCheckTimeout time.Duration
}

// instructionCheckTimeout is the default deadline of the instruction-file
// check. The check only lists a few files, so a few seconds is generous; a
// longer wait means the exec is wedged (#279).
const instructionCheckTimeout = 15 * time.Second

// ErrInstructionCheckTimeout is returned when the instruction-file check does
// not finish in time. It is a launch failure, never a passed check: the file
// list is unknown, so the launch refuses (D38).
var ErrInstructionCheckTimeout = errors.New("the instruction-file check did not finish in time")

// Adapter is the Claude Code agent adapter. In manual mode the CLI runs with
// `--permission-prompt-tool stdio` and every permission prompt reaches the
// host as a control_request on the exec's stdout; the host's answer goes back
// as a control_response on its stdin, matched by request_id (D26, spike #7).
// In dontAsk mode nothing is asked: only the allowlist runs (design §5.2).
type Adapter struct {
	r           Runner
	cfg         Config
	now         func() time.Time
	settingsErr error // the supervisor's settings did not parse; every start refuses
}

// ErrInstructionFiles is returned when a CLAUDE.md-family file sits above the
// workspace, or a managed one exists: it would replace or add to the
// repository's AGENTS.md (D38).
var ErrInstructionFiles = errors.New("a CLAUDE.md-family file outside the repository would change the agent's instructions")

// instructionFileMode makes the CLI read the repository's AGENTS.md when it has
// no CLAUDE.md, set explicitly so that a changed default in a new CLI version
// cannot change which file is read (D38).
const instructionFileMode = "claude-md-or-agents-md"

// withInstructionFiles adds the agents-md plugin's instructionFiles option to
// the supervisor's settings unless they set it themselves.
func withInstructionFiles(settings string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(settings), &m); err != nil || m == nil {
		return "", fmt.Errorf("%w: the supervisor's settings are not a JSON object", agent.ErrBadSpec)
	}
	sub := func(parent map[string]any, key string) map[string]any {
		if v, ok := parent[key].(map[string]any); ok {
			return v
		}
		v := map[string]any{}
		parent[key] = v
		return v
	}
	opts := sub(sub(sub(m, "pluginConfigs"), "agents-md@builtin"), "options")
	if _, set := opts["instructionFiles"]; !set {
		opts["instructionFiles"] = instructionFileMode
	}
	out, err := json.Marshal(m)
	return string(out), err
}

// instructionCheck prints every CLAUDE.md-family file in the directories above
// the workspace ($1), and a managed CLAUDE.md. The workspace's own files are the
// repository's and are left to it (D38).
const instructionCheck = `found=""
[ -e /etc/claude-code/CLAUDE.md ] && found="/etc/claude-code/CLAUDE.md"
d=$1
if [ -n "$d" ] && [ "$d" != / ]; then
	p=$(dirname "$d")
	while :; do
		for f in CLAUDE.md .claude/CLAUDE.md CLAUDE.local.md; do
			[ -e "$p/$f" ] && found="$found $p/$f"
		done
		[ "$p" = / ] && break
		p=$(dirname "$p")
	done
fi
printf '%s' "$found"`

// New returns an adapter that runs the CLI through r.
func New(r Runner, cfg Config) *Adapter {
	if cfg.Bin == "" {
		cfg.Bin = "claude"
	}
	if cfg.Settings == "" {
		cfg.Settings = "{}"
	}
	if cfg.InstructionCheckTimeout <= 0 {
		cfg.InstructionCheckTimeout = instructionCheckTimeout
	}
	if cfg.ResumeProbe <= 0 {
		cfg.ResumeProbe = 10 * time.Second
	}
	a := &Adapter{r: r, cfg: cfg, now: time.Now}
	if s, err := withInstructionFiles(cfg.Settings); err != nil {
		a.settingsErr = err
	} else {
		a.cfg.Settings = s
	}
	return a
}

// checkInstructionFiles runs instructionCheck in the environment and refuses
// when it finds anything. It runs under its own deadline: launch runs under
// context.WithoutCancel, so without one a wedged exec would keep the run's
// session slot until the supervisor restarts (#279). On expiry the exec's
// context is cancelled, which makes the runtime stop the process (the Apple
// Container adapter signals the process group in the guest, then ends its
// client), and the check is abandoned at once: it does not wait for a runtime
// that ignores the cancel, so the slot is freed either way. Whether a guest
// process outlives the deadline on Apple Container is unverified (not
// measured); a leftover process only lists files and holds no slot.
func (a *Adapter) checkInstructionFiles(ctx context.Context, spec agent.StartSpec) error {
	cctx, cancel := context.WithTimeout(ctx, a.cfg.InstructionCheckTimeout)
	defer cancel()
	type result struct {
		out  []byte
		code int
		err  error
	}
	done := make(chan result, 1) // buffered: an abandoned check's goroutine ends
	go func() {
		st, err := a.r.Exec(cctx, spec.EnvID, runtime.ExecRequest{Cmd: []string{"/bin/sh", "-c", instructionCheck, "whr-instruction-check", spec.Workdir}})
		if err != nil {
			done <- result{err: fmt.Errorf("the instruction-file check: %w", err)}
			return
		}
		out, _, code, err := runtime.Collect(st)
		if err != nil {
			err = fmt.Errorf("the instruction-file check failed: %w", err)
		}
		done <- result{out, code, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-cctx.Done():
		return a.checkTimedOut(ctx)
	}
	if res.err != nil {
		if errors.Is(res.err, context.DeadlineExceeded) && cctx.Err() != nil {
			return a.checkTimedOut(ctx)
		}
		return res.err
	}
	if res.code != 0 {
		return fmt.Errorf("the instruction-file check failed with exit %d", res.code)
	}
	if found := strings.TrimSpace(string(res.out)); found != "" {
		return fmt.Errorf("%w: %s", ErrInstructionFiles, found)
	}
	return nil
}

// checkTimedOut is the error of a check that ended with its context: the
// caller's own cancel is returned as it is, the check's deadline as
// ErrInstructionCheckTimeout.
func (a *Adapter) checkTimedOut(parent context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	return fmt.Errorf("%w (%s)", ErrInstructionCheckTimeout, a.cfg.InstructionCheckTimeout)
}

// Name implements agent.Adapter.
func (a *Adapter) Name() string { return "claude-code" }

// Capabilities implements agent.Adapter.
func (a *Adapter) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ContractVersion:   agent.ContractVersion,
		Headless:          true,
		StructuredEvents:  true,
		MidRunInstruction: true,
		HostApprovals:     true, // the stdio control protocol (D26)
		SessionResume:     true,
		AwaitingGuidance:  false,
		ReportsQuota:      true, // unverified rule, design §5.2
		ReportsUsage:      true,
		ToolWatchdog:      false, // correlated output and tool-only abort are unverified
		AuthModes:         []agent.AuthMode{agent.AuthSubscription, agent.AuthAPIKey},
	}
}

// Start implements agent.Adapter.
func (a *Adapter) Start(ctx context.Context, spec agent.StartSpec) (agent.Session, error) {
	return a.launch(ctx, spec, "")
}

// Resume implements agent.Adapter. The prompt of the spec is the next message.
// An unknown session is recognized by an error result before any init that is
// not a login failure; that is unverified (design §5.2).
func (a *Adapter) Resume(ctx context.Context, spec agent.StartSpec, sessionID string) (agent.Session, error) {
	if sessionID == "" {
		return nil, agent.ErrNoSession
	}
	return a.launch(ctx, spec, sessionID)
}

func (a *Adapter) args(spec agent.StartSpec, resume string) []string {
	args := []string{a.cfg.Bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if spec.Mode() == agent.PermissionManual {
		// Every prompt goes to the host over the control channel (D26).
		args = append(args, "--permission-mode", string(agent.PermissionManual), "--permission-prompts", "host", "--permission-prompt-tool", "stdio")
	} else {
		args = append(args, "--permission-prompts", "none", "--permission-mode", string(agent.PermissionDontAsk))
	}
	args = append(args,
		// The CLI reads only what the supervisor passes. The agent writes its
		// repository and its home, and both are settings sources: pinning the
		// sources to user was measured not to be enough (design §5.2).
		"--setting-sources", "", "--settings", a.cfg.Settings, "--strict-mcp-config", "--disable-slash-commands",
	)
	if len(spec.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(spec.AllowedTools, ","))
	}
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	return args
}

func (a *Adapter) launch(ctx context.Context, spec agent.StartSpec, resume string) (agent.Session, error) {
	if err := a.Capabilities().CheckSpec(spec); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return nil, errors.Join(agent.ErrBadSpec, errors.New("a session starts with its first message, so the prompt is needed"))
	}
	if a.settingsErr != nil {
		return nil, a.settingsErr
	}
	if err := a.checkInstructionFiles(ctx, spec); err != nil {
		return nil, err
	}

	pctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	env := append(append([]string(nil), a.cfg.Env...), spec.Env...)
	if a.cfg.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+a.cfg.ConfigDir)
	}
	st, err := a.r.Exec(pctx, spec.EnvID, runtime.ExecRequest{Cmd: a.args(spec, resume), Env: env, Dir: spec.Workdir, Stdin: pr})
	if err != nil {
		cancel()
		_ = pw.Close()
		return nil, err
	}

	actx, acancel := context.WithCancel(pctx)
	s := &session{
		cancel: cancel, stdin: pw, events: make(chan agent.Event, 256), done: make(chan struct{}),
		inited: make(chan struct{}), p: newParser(a.now, spec.Mode() == agent.PermissionDontAsk, spec.AllowedTools), resume: resume != "",
		now: a.now, approver: spec.Approver, timeout: spec.ApprovalTimeout, actx: actx, acancel: acancel, open: map[string]bool{},
	}
	go s.run(pctx, st)
	// The prompt is the first message; the CLI reports its session ID only after it.
	go func() { _ = s.send(spec.Prompt) }()

	if resume != "" {
		if err := s.probe(a.cfg.ResumeProbe); err != nil {
			return nil, err
		}
	}
	return s, nil
}

type session struct {
	cancel context.CancelFunc
	stdin  *io.PipeWriter
	events chan agent.Event
	done   chan struct{}
	inited chan struct{} // closed when the session event arrives
	resume bool
	p      *parser // used by run, and by probe after ready
	now    func() time.Time

	// Permission prompts (D26). Each control_request is asked of the approver in
	// its own goroutine, under actx, which ends when the session is stopped or
	// the output ends, so no prompt outlives the process.
	approver agent.Approver
	timeout  time.Duration
	actx     context.Context
	acancel  context.CancelFunc
	asks     sync.WaitGroup
	omu      sync.Mutex
	ord      sync.Mutex      // orders events: an approval's record precedes what the CLI does after the answer
	open     map[string]bool // request IDs asked and not yet answered

	wmu         sync.Mutex  // one message at a time on stdin
	stdinClosed atomic.Bool // set before the pipe is closed

	mu       sync.Mutex // guards the fields below
	id       string
	stopped  bool
	result   agent.Result
	unknown  bool  // a resume that found no such session
	earlyErr error // a resume that failed before init for another reason
	initOnce sync.Once
}

func (s *session) markInited() { s.initOnce.Do(func() { close(s.inited) }) }

// send writes one user message as a stream-json line.
func (s *session) send(text string) error {
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}},
	})
	if err != nil {
		return err
	}
	return s.writeLine(line)
}

// writeLine writes one line to the process's stdin, one at a time.
func (s *session) writeLine(line []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.stdinClosed.Load() {
		return agent.ErrNotRunning
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		return agent.ErrNotRunning
	}
	return nil
}

// closeStdin ends the input so that the process exits after its result.
// It does not wait for a write in progress: closing the pipe ends it.
func (s *session) closeStdin() {
	s.stdinClosed.Store(true)
	_ = s.stdin.Close()
}

func (s *session) emit(e agent.Event) {
	s.ord.Lock()
	defer s.ord.Unlock()
	s.events <- e
}

func (s *session) run(ctx context.Context, st runtime.ExecStream) {
	defer close(s.done)
	defer close(s.events)
	defer s.cancel()

	var stderr []byte
	var lines int
	for c := range st.Chunks() {
		switch c.Stream {
		case runtime.Stdout:
			for _, e := range s.p.feed(c.Data) {
				s.observe(e)
				s.emit(e)
			}
			for _, cr := range s.p.takeControls() {
				s.control(cr)
			}
			if s.p.result != nil {
				s.closeStdin()
			}
		case runtime.Stderr:
			stderr = append(stderr, c.Data...)
			stderr = s.flushStderr(stderr, &lines)
		}
	}
	code, err := st.Wait()
	s.closeStdin()
	// No prompt outlives the process: whatever is still open is denied and
	// recorded before the events end (D26).
	s.acancel()
	s.asks.Wait()

	id := s.p.sessionID
	res, ok := s.p.outcome()
	s.mu.Lock()
	stopped := s.stopped || ctx.Err() != nil
	s.mu.Unlock()

	var final agent.Result
	var early error
	unknown := false
	switch {
	case ok:
		// A result that arrived is the outcome, even if Stop came after it.
		final = res
		if s.resume && !s.p.sawInit && !s.p.authFailed && s.p.result.IsError {
			if missingSession(s.p.result.Result) {
				unknown = true
			} else {
				early = fmt.Errorf("claude: resume failed: %s", capText(s.p.result.Result))
			}
		}
	case stopped:
		final = agent.Result{Status: agent.ResultStopped, SessionID: id}
	default:
		text := "the agent ended without a result"
		if err != nil || code != 0 {
			text = "the agent exited with code " + itoa(code)
		}
		s.emit(s.p.errorEvent(text))
		final = agent.Result{Status: agent.ResultFailed, SessionID: id, Text: text}
	}
	s.mu.Lock()
	s.result, s.unknown, s.earlyErr = final, unknown, early
	s.mu.Unlock()
}

// observe records the session ID the moment its event arrives.
func (s *session) observe(e agent.Event) {
	if e.Kind == agent.EventSession {
		s.mu.Lock()
		s.id = e.SessionID
		s.mu.Unlock()
		s.markInited()
	}
}

// Limits on stderr: it is untrusted and must not grow without bound.
const (
	maxStderrLines = 200
	maxStderrLine  = 64 << 10 // bytes kept of one line, and of an unfinished one
)

// flushStderr turns each complete stderr line into an error event and returns
// the incomplete rest. At most maxStderrLines lines become events per session,
// the rest are dropped after one note, and no line or unfinished line keeps
// more than maxStderrLine bytes.
func (s *session) flushStderr(buf []byte, lines *int) []byte {
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			if len(buf) > maxStderrLine {
				buf = buf[:maxStderrLine] // an unfinished line is kept to its cap
			}
			return buf
		}
		line := buf[:i]
		if len(line) > maxStderrLine {
			line = line[:maxStderrLine]
		}
		if text := strings.TrimSpace(string(line)); text != "" {
			*lines++
			switch {
			case *lines < maxStderrLines:
				s.emit(s.p.errorEvent(text))
			case *lines == maxStderrLines:
				s.emit(s.p.errorEvent("stderr has more lines than are kept; the rest are dropped"))
			}
		}
		buf = buf[i+1:]
	}
}

// probe waits until a resumed session says whether it exists: the session
// event means it does, and an end before one means it may not.
func (s *session) probe(limit time.Duration) error {
	select {
	case <-s.inited:
		return nil
	case <-s.done:
	case <-time.After(limit):
		return nil // no verdict: go on as if it exists
	}
	s.mu.Lock()
	unknown, early := s.unknown, s.earlyErr
	s.mu.Unlock()
	if unknown {
		return agent.ErrNoSession
	}
	return early
}

// missingSession reports whether an error result says the session does not
// exist. What the CLI really prints is unverified, so this is a heuristic on
// the wording; any other failure is an ordinary error.
func missingSession(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "no conversation") ||
		(strings.Contains(t, "session") && (strings.Contains(t, "not found") || strings.Contains(t, "no such") || strings.Contains(t, "does not exist")))
}

func (s *session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *session) Events() <-chan agent.Event { return s.events }

// Instruct writes a user message into the running process, which picks it up
// at its next model step, after the running tool (spike #1).
func (s *session) Instruct(_ context.Context, message string) (agent.Delivery, error) {
	select {
	case <-s.done:
		return "", agent.ErrNotRunning
	default:
	}
	if err := s.send(message); err != nil {
		return "", err
	}
	return agent.DeliveryNextTurn, nil
}

// Stop cancels the Exec, which the runtime contract requires to end the
// process in the guest (design §5.1). The session stays resumable by its ID.
func (s *session) Stop(context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	return nil
}

func (s *session) Wait() (agent.Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// maxOpenPrompts is how many permission prompts of one session may wait for
// the human at once; one more is denied without asking. A CLI asks one at a
// time in practice, so the limit only stops a flood.
const maxOpenPrompts = 8

// maxToolName is the longest tool name asked about; Claude Code's tool names,
// MCP ones included, are far shorter.
const maxToolName = 128

// toolNameRe is a tool name a prompt may carry: Claude Code's own (Bash, Edit)
// and MCP tools (mcp__server__tool).
var toolNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,127}$`)

// control puts one control_request to the host. A permission prompt is asked of
// the approver, which may take as long as a human does, so it runs apart from
// the reader; anything else is refused at once, because an unanswered request
// leaves the CLI waiting.
func (s *session) control(cr controlRequest) {
	if cr.Subtype != "can_use_tool" {
		s.emit(s.p.errorEvent("a control request of kind " + capText(cr.Subtype) + " is not supported and was refused"))
		if err := s.respond(controlError(cr.ID, "unsupported control request")); err != nil {
			s.cancel() // the channel is gone while the agent runs: stop it (D26)
		}
		return
	}
	text, plan := approvalInput(cr.Tool, cr.Input)
	req := agent.ApprovalRequest{ID: cr.ID, Tool: cr.Tool, Input: text, Plan: plan}
	refuse := ""
	if !toolNameRe.MatchString(cr.Tool) { // the name becomes the Decision's subject
		req.Tool = capText(cr.Tool)
		if len(req.Tool) > maxToolName {
			req.Tool = req.Tool[:maxToolName]
		}
		refuse = "not a tool name: denied"
	}

	s.omu.Lock()
	switch {
	case refuse != "":
	case s.open[cr.ID]: // one open request per ID: a second with the same ID is not asked
		refuse = "a request with this ID is already open: denied"
	case len(s.open) >= maxOpenPrompts: // a flood must not fill the human's inbox
		refuse = "too many permission prompts open at once: denied"
	default:
		s.open[cr.ID] = true
	}
	s.omu.Unlock()
	s.asks.Add(1)
	go func() {
		defer s.asks.Done()
		if refuse != "" {
			s.answer(req, agent.Approval{Reason: refuse})
			return
		}
		a := agent.Ask(s.actx, s.approver, s.timeout, req)
		s.omu.Lock()
		delete(s.open, cr.ID)
		s.omu.Unlock()
		s.answer(req, a)
	}()
}

// answer sends the control_response to the request it is for, by request_id,
// and records what the agent was told. An allow that could not be delivered is
// recorded as the denial it is, and the agent is stopped: the channel is gone
// while it still runs, and a request must not outlive its channel (D26).
func (s *session) answer(req agent.ApprovalRequest, a agent.Approval) {
	// The record is emitted before the CLI can react to the answer, so the
	// stream reads in the order things happened: the lock is held across both.
	s.ord.Lock()
	defer s.ord.Unlock()
	reason := a.Reason
	if err := s.respond(controlAnswer(req.ID, a)); err != nil {
		if a.Allow {
			a, reason = agent.Approval{}, "the channel to the agent was lost before the answer: denied"
		}
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if !stopped {
			s.cancel() // the runtime ends the process in the guest (D25)
		}
	}
	e := agent.Event{Kind: agent.EventApproval, At: s.now(), Tool: req.Tool, Input: capText(req.Input)}
	s.mu.Lock()
	e.SessionID = s.id
	s.mu.Unlock()
	e.Approval = &agent.ApprovalRecord{ID: req.ID, Allow: a.Allow, Reason: reason}
	s.events <- e
}

// respond writes a control_response line.
func (s *session) respond(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.writeLine(line)
}

// controlAnswer is the control_response of spike #7: success with allow or deny.
// A denial carries its reason as the message the agent sees.
func controlAnswer(id string, a agent.Approval) map[string]any {
	body := map[string]any{"behavior": "deny", "message": denialMessage(a.Reason)}
	if a.Allow {
		body = map[string]any{"behavior": "allow"}
	}
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": body}}
}

// controlError refuses a request the adapter does not support. The shape is the
// SDK's error response and is unverified against the CLI.
func controlError(id, msg string) map[string]any {
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": msg}}
}

func denialMessage(reason string) string {
	if reason == "" {
		return "Denied by the supervisor"
	}
	return capText(reason)
}
