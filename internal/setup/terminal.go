package setup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/wstein/workharbor/internal/doctor"
	"github.com/wstein/workharbor/internal/render"
	"github.com/wstein/workharbor/internal/runlog"
	"github.com/wstein/workharbor/internal/textsafe"
)

// Terminal is the real Host: a person at a terminal. Prompts go to Err (stdout is
// data), answers come from In, and a secret is read from the terminal itself with
// no echo. Commands run with the terminal attached so sudo can ask for a password.
type Terminal struct {
	In  *bufio.Reader
	Err io.Writer
	// Stdin is the terminal a secret is read from; it must be a terminal.
	Stdin *os.File
	// Style draws the prompts and the output of commands (zero: plain ASCII).
	Style render.Style
	// Sig records that a command died of an interrupt, see Interrupts. Nil: not
	// recorded, and only the context tells.
	Sig *Interrupts
	// Log records every command, its exit code and output (issue #379); nil: none.
	Log *runlog.Log
	// Probes remembers the answers of read-only commands for one run, so a
	// check that the doctor and the wizard both make runs its command once. Any
	// command the wizard runs (a fix) forgets them. Nil: every call runs.
	Probes *Probes
	// readSecret replaces Secret in tests.
	readSecret func(question string) (string, error)
}

// Interrupts is the sticky record that a command was ended by an interrupt.
// Ctrl-C reaches the command and whr together, and the command often dies
// before whr's signal goroutine cancels the context; a check returns only a
// status, so the mark on its error is lost. The record is what Run, the
// re-check and the guards before a question or a prefix check read besides
// ctx.Err().
type Interrupts struct{ seen atomic.Bool }

// Seen says whether a command died of an interrupt since the last Reset.
func (i *Interrupts) Seen() bool { return i != nil && i.seen.Load() }

// Reset forgets what was seen; Run does it at its start.
func (i *Interrupts) Reset() {
	if i != nil {
		i.seen.Store(false)
	}
}

func (i *Interrupts) note() {
	if i != nil {
		i.seen.Store(true)
	}
}

// Interrupted implements InterruptSeen.
func (t Terminal) Interrupted() bool { return t.Sig.Seen() }

// ResetInterrupts implements InterruptSeen.
func (t Terminal) ResetInterrupts() { t.Sig.Reset() }

// InterruptSeen is what a Host adds to tell Run that a command died of an
// interrupt while the context was still live.
type InterruptSeen interface {
	Interrupted() bool
	ResetInterrupts()
}

// Interrupted says whether the context ended or the host saw a command die of
// an interrupt: the person pressed Ctrl-C, whichever of the two noticed first.
func Interrupted(ctx context.Context, h any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s, ok := h.(InterruptSeen); ok && s.Interrupted() {
		return context.Canceled
	}
	return nil
}

// Probes is the per-run memory of read-only command answers, see Terminal.
type Probes struct {
	mu sync.Mutex
	m  map[string]probe
}

type probe struct {
	out []byte
	err error
}

func (p *Probes) get(key string) (probe, bool) {
	if p == nil {
		return probe{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[key]
	return v, ok
}

func (p *Probes) put(key string, v probe) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]probe{}
	}
	p.m[key] = v
}

// Reset forgets every answer: something may have changed the system.
func (p *Probes) Reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = nil
}

// Output implements doctor.Runner. With Probes, a command already run since
// the last fix answers from memory and is neither run nor logged again; an
// answer cut short by an interrupt is never kept.
func (t Terminal) Output(ctx context.Context, argv ...string) ([]byte, error) {
	key := strings.Join(argv, "\x00")
	if v, ok := t.Probes.get(key); ok {
		return v.out, v.err
	}
	out, err := t.output(ctx, argv...)
	if ctx.Err() == nil && !t.Sig.Seen() {
		t.Probes.put(key, probe{out, err})
	}
	return out, err
}

func (t Terminal) output(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // a read-only command named by the steps
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = runWaitDelay // a background child holding the pipe must not stall the read
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // the command itself succeeded; its output was read
	}
	err = t.interruptOr(ctx, err)
	code := exitCodeOf(err)
	t.Log.CommandAnswer(argv, code, escapeLines(out.String()+errb.String()), "", doctor.ExpectedAnswer(argv, code, errb.String()))
	if err != nil && errb.Len() > 0 {
		// what the command said is what tells "not set" from "could not read"
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), err
}

// DockerContextEndpoint captures only read-only context endpoint metadata. Neither
// stdout, stderr, the context name nor the executable path reaches the run log,
// probe cache or returned errors: metadata may contain credentials.
func (t Terminal) DockerContextEndpoint(ctx context.Context, binary, name string) ([]byte, error) {
	argv := []string{"context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"}
	if name != "" {
		argv = append(argv, name)
	}
	cmd := exec.CommandContext(ctx, binary, argv...) //nolint:gosec // trusted Docker executable; fixed read-only metadata command
	out := &contextOutput{}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	cmd.WaitDelay = runWaitDelay
	err := t.interruptOr(ctx, cmd.Run())
	if err != nil || len(out.data) > 4096 {
		return nil, errors.New("docker context metadata unavailable")
	}
	return out.data, nil
}

// contextOutput drains the subprocess while retaining at most the response cap
// plus one byte. Context cancellation and WaitDelay bound execution and draining.
type contextOutput struct{ data []byte }

func (o *contextOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4097 - len(o.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		o.data = append(o.data, p...)
	}
	return n, nil
}

// runWaitDelay is how long Run waits for the output pipes after the command
// exited or the context ended.
const runWaitDelay = 2 * time.Second

// secretAttempts is how often a new password is asked for when the macOS
// password policy rejects it.
const secretAttempts = 3

// policyRejected is sysadminctl's error code for a password the macOS password
// policy refuses ("New account password error. (5402)", seen on a real host).
const policyRejected = "(5402)"

// ErrPasswordPolicy is the message for that rejection.
var ErrPasswordPolicy = errors.New("password rejected by the macOS password policy: too short/simple")

// Run implements Host. A command with a SecretPrompt gets its secret from
// whr, see doctor.Cmd; with SecretConfirm it is a new password, asked twice and
// asked again (up to three times) when the password policy rejects it. Any
// other failure is not retried, and a third rejection stops the whole run.
func (t Terminal) Run(ctx context.Context, c doctor.Cmd) error {
	t.Probes.Reset() // a command that is not a probe may change what the probes read
	if c.SecretPrompt == "" {
		return t.runOnce(ctx, c, "", nil)
	}
	read := t.Secret
	if t.readSecret != nil {
		read = t.readSecret
	}
	if c.SecretConfirm {
		// show the host's password rules first; silent when pwpolicy does not say
		if out, err := t.Output(ctx, "pwpolicy", "-getaccountpolicies"); err == nil {
			lang := os.Getenv("LC_ALL")
			if lang == "" {
				lang = os.Getenv("LANG")
			}
			if d := doctor.PolicyDescription(out, lang); d != "" {
				fmt.Fprintln(t.Err, "Password rules of this Mac: "+textsafe.Escape(d))
			}
		}
	}
	for attempt := 1; ; attempt++ {
		pw, err := read(c.SecretPrompt)
		if err != nil {
			return err
		}
		if c.SecretConfirm {
			again, err := read("Type it again")
			if err != nil {
				return err
			}
			if again != pw {
				fmt.Fprintln(t.Err, "the two passwords differ")
				if attempt >= secretAttempts {
					return fatalError{errors.New("the passwords did not match three times: nothing was created; run the step again")}
				}
				continue
			}
		}
		var seen bytes.Buffer
		err = t.runOnce(ctx, c, pw, &seen)
		if err == nil || !c.SecretConfirm || !strings.Contains(seen.String(), policyRejected) {
			return err
		}
		fmt.Fprintln(t.Err, ErrPasswordPolicy.Error())
		if attempt >= secretAttempts {
			return fatalError{fmt.Errorf("%w: no account was created after %d tries; choose a longer, less simple password and run the step again", ErrPasswordPolicy, attempt)}
		}
	}
}

func (t Terminal) runOnce(ctx context.Context, c doctor.Cmd, pw string, seen *bytes.Buffer) error {
	argv := c.Full()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the fixes the steps list, shown before they run
	// what the tool says is set apart from whr's own text, line by line, with
	// nothing held back, so a prompt of the tool appears at once
	tw := render.NewToolWriter(t.Err, t.Style)
	defer tw.End()
	var w io.Writer = tw
	if seen != nil {
		w = io.MultiWriter(tw, seen)
	}
	// the log gets the same filtered text the terminal gets: a tool's prompt for
	// a secret is dropped, and the secret value itself is masked. The raw bytes
	// are kept and masked first, escaped after: the escaper rewrites DEL, C1 and
	// bidi controls, and a secret holding one would no longer match.
	var rawLog bytes.Buffer
	if t.Log != nil {
		w = io.MultiWriter(w, &rawLog)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, w, w
	if c.SecretPrompt != "" {
		// whr reads the secret itself, without echo, and hands it over on stdin:
		// never argv, never the environment, and the child gets no terminal as
		// its input. Echo stays off on the terminal while it runs, in case it
		// opens /dev/tty itself; its prompt for a secret is dropped by tw.
		cmd.Stdin = strings.NewReader(pw + "\n")
		// Ctrl-\ (SIGQUIT) would end whr at once, with echo still off: catch it
		// while the child runs; the child dies of it and the restore runs.
		defer catchQuit()()
		if t.Stdin != nil {
			defer echoOff(int(t.Stdin.Fd()))() //nolint:gosec // a file descriptor of this process
		}
	}
	// a background child that keeps the pipe open must not stall Run
	cmd.WaitDelay = runWaitDelay
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// the command itself succeeded; a leftover child only held the pipe
		err = nil
	} else {
		err = t.interruptOr(ctx, err)
	}
	if t.Log != nil {
		masked := rawLog.String()
		if pw != "" {
			masked = strings.ReplaceAll(masked, pw, "***")
		}
		var logged bytes.Buffer
		lw := render.NewBareToolWriter(&logged)
		_, _ = lw.Write([]byte(masked))
		lw.End() // flush the held-back partial text
		t.Log.CommandShown(argv, exitCodeOf(err), logged.String(), pw)
	}
	return err
}

// escapeLines shows every control character of a tool's output escaped, as the
// ToolWriter does on the terminal, and keeps the line breaks (LF or CRLF as LF).
func escapeLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = textsafe.Escape(strings.TrimSuffix(ln, "\r"))
	}
	return strings.Join(lines, "\n")
}

// catchQuit makes whr survive Ctrl-\ (SIGQUIT) until the returned function runs,
// which restores the default. It is for the times the terminal's echo is off.
func catchQuit() (stop func()) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGQUIT)
	return func() { signal.Stop(quit) }
}

// exitCodeOf is the exit status in an error from a command: 0 for none, -1
// when the command did not end with a status (not found, killed).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode()
	}
	return -1
}

// interruptOr marks the error of a command that an interrupt ended, so the
// callers see the context error: os/exec reports the signal that killed the
// command, not the context.
//
// A command that was killed by SIGINT, SIGTERM or SIGHUP, or that caught one
// and exited 130, 129 or 143 (the shell convention, as a trap does), counts as
// interrupted and is recorded in t.Sig.
func (t Terminal) interruptOr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	if isInterruptEnd(err) {
		// Ctrl-C reaches the command and whr together, and the command often
		// dies first: that is an interrupt even before the context says so
		t.Sig.note()
		return fmt.Errorf("%w: %w", context.Canceled, err)
	}
	return err
}

// isInterruptEnd says whether a command ended by an interrupt signal or exited
// with the status a shell gives one.
func isInterruptEnd(err error) bool {
	if sig, ok := killedBy(err); ok {
		return sig == syscall.SIGINT || sig == syscall.SIGTERM || sig == syscall.SIGHUP
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 128 + int(syscall.SIGHUP), 128 + int(syscall.SIGINT), 128 + int(syscall.SIGTERM):
			return true
		}
	}
	return false
}

// killedBy is the signal that ended a command, if one did.
func killedBy(err error) (syscall.Signal, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal(), true
		}
	}
	return 0, false
}

// Open implements Host. It runs `open`, so a URL or a System Settings pane opens
// the way a click on it would.
func (Terminal) Open(ctx context.Context, target string) error {
	return exec.CommandContext(ctx, "open", target).Run() //nolint:gosec // opens a link the step names
}

// Line implements doctor.Prompter.
func (t Terminal) Line(question string) (string, error) {
	fmt.Fprint(t.Err, render.Question(t.Style, question+":"))
	s, err := t.In.ReadString('\n')
	if err != nil && s == "" {
		return "", errors.New("no answer: standard input ended")
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// Secret implements doctor.Prompter: no echo, and the value goes nowhere but the
// caller.
func (t Terminal) Secret(question string) (string, error) {
	if t.Stdin == nil || !term.IsTerminal(int(t.Stdin.Fd())) { //nolint:gosec // a file descriptor of this process
		return "", errors.New("a secret is only read from a terminal, without echo")
	}
	fmt.Fprint(t.Err, render.Question(t.Style, question+":"))
	// Ctrl-\ (SIGQUIT) during the read would end whr with echo still off: catch
	// it, so the signal does nothing, the read goes on until Enter, and term
	// restores the terminal when it ends
	defer catchQuit()()
	b, err := term.ReadPassword(int(t.Stdin.Fd())) //nolint:gosec // a file descriptor of this process
	fmt.Fprintln(t.Err)
	return string(b), err
}

// Confirm implements doctor.Prompter: only y or yes is a yes. Nested
// configuration writes still require explicit assent; q stops the setup run.
func (t Terminal) Confirm(question string) (bool, error) {
	a, err := t.Ask(question, render.DefaultNo)
	if a == render.Quit {
		return false, render.ErrQuit
	}
	return a == render.Yes, err
}

// Show implements doctor.Prompter.
func (t Terminal) Show(text string) { fmt.Fprintln(t.Err, text) }

// Ask implements Asker: [Y/n/q] or [y/N/q] by the default, as an ACTION line.
func (t Terminal) Ask(question string, d render.Default) (render.Answer, error) {
	defer t.Probes.Reset() // the person may have acted while the question was open
	return render.Ask(t.In, render.Writer{W: t.Err, S: t.Style}, question, d)
}

// AskWord asks for a typed word, for the most destructive steps: only the exact
// word is yes, Enter and anything else is no, q quits.
func (t Terminal) AskWord(question, word string) (render.Answer, error) {
	defer t.Probes.Reset() // a look after the typed word must read the system afresh
	return render.AskWord(t.In, render.Writer{W: t.Err, S: t.Style}, question, word)
}

// Pause implements Pauser: Enter goes on, q or quit quits. A closed input goes on, so a
// run never blocks on it.
func (t Terminal) Pause() error { return t.PauseContext(context.Background()) }

// PauseContext implements ContextPauser: like Pause, but it returns the
// context's error as soon as the context ends, without waiting for Enter.
// After a cancel the reader goroutine still owns t.In and is blocked in
// ReadString: it takes the next line typed, so the caller must not read t.In
// again after a cancelled pause (the run stops there).
func (t Terminal) PauseContext(ctx context.Context) error {
	defer t.Probes.Reset()
	fmt.Fprintln(t.Err, "\nPress Enter to continue (q to quit)")
	line := make(chan string, 1)
	go func() { s, _ := t.In.ReadString('\n'); line <- s }()
	var s string
	select {
	case s = <-line:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s = strings.TrimSpace(s); strings.EqualFold(s, "q") || strings.EqualFold(s, "quit") {
		return render.ErrQuit
	}
	return nil
}
