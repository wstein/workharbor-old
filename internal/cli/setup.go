package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wstein/workharbor/internal/doctor"
	"github.com/wstein/workharbor/internal/launchd"
	"github.com/wstein/workharbor/internal/render"
	rt "github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/setup"
	"github.com/wstein/workharbor/internal/setup/answers"
	"github.com/wstein/workharbor/internal/setup/protocol"
	"github.com/wstein/workharbor/internal/textsafe"
)

// SetupEnv is what `whr setup` needs of the machine, so a test runs it with a
// fake Host and no terminal. Zero values mean this machine.
type SetupEnv struct {
	Host       setup.Host
	User       string
	UID        int
	GOOS       string
	IsTerminal func() bool
	Executable func() (string, error)
	Manager    *launchd.Manager
	// LookPath finds a program on the PATH; nil is the real PATH. A test
	// replaces it so a report does not depend on what the machine has installed.
	LookPath func(string) (string, error)
	// RuntimeSockets overrides known host sockets in tests; empty probes none.
	RuntimeSockets []string
	// Identity is the build identity answer files are bound to; nil is
	// answers.Identity. OpenLog opens the setup protocol of the account whose
	// home directory is given; nil is protocol.Open. A test replaces both.
	Identity func() (string, error)
	OpenLog  func(home string) (*protocol.Log, error)
	// NoRunLog skips the default run log (issue #379) so a test's output stays
	// fixed; an explicit --log-file is still written.
	NoRunLog bool
}

func (e SetupEnv) resolve(st *state, style render.Style) (SetupEnv, error) {
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	if e.User == "" {
		u, err := user.Current()
		if err != nil {
			return e, err
		}
		e.User, e.UID = u.Username, os.Getuid()
	}
	if e.IsTerminal == nil {
		e.IsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) } //nolint:gosec // a file descriptor of this process
	}
	if e.Executable == nil {
		e.Executable = os.Executable
	}
	if e.LookPath == nil {
		e.LookPath = doctor.DefaultLookPath
	}
	if e.Host == nil {
		e.Host = setup.Terminal{In: bufio.NewReader(st.env.Stdin), Err: st.env.Stderr, Stdin: os.Stdin, Style: style, Sig: &setup.Interrupts{}, Probes: &setup.Probes{}}
	}
	return e, nil
}

// autoPhase picks the part of `whr setup` that fits the account running it: a
// step named by --only or --from decides when it belongs to the user part;
// otherwise the whr account itself (when it is no administrator) runs the user
// part, and every other account the administrator's part, which also writes
// the whr account's base configuration.
func autoPhase(ctx context.Context, env SetupEnv, whrUser string, only []string, from string) doctor.Phase {
	names := append(append([]string{}, only...), from)
	user := map[string]bool{}
	for _, s := range doctor.Steps(doctor.Checks(doctor.Deps{ConfigPath: "x/config.json"}), doctor.PhaseUser) {
		user[s.Name] = true
	}
	for _, n := range names {
		if n == "whr-user" {
			n = "workharbor-user"
		}
		if user[n] {
			return doctor.PhaseUser
		}
	}
	if env.User != whrUser {
		return doctor.PhaseHost
	}
	if member, _, _ := doctor.Membership(ctx, env.Host, env.User); member {
		return doctor.PhaseHost
	}
	return doctor.PhaseUser
}

// newSetup is `whr setup` (provisional, design D46, issue #104): the
// first-time setup wizard. The administrator's run sets up the host and
// initializes the whr account's base configuration; `whr setup host` is a
// silently accepted alias of it. The whr user's part runs in its desktop
// session. Each step is a check with an optional fix, shown before it runs.
func newSetup(st *state) *cobra.Command {
	var (
		dryRun  bool
		only    []string
		from    string
		whrUser string
		prefix  string
		plain   bool
		verbose bool
		// issue #337: the answer file, saving one, and asking nothing
		answersPath, savePath string
		unattended            bool
		logFile               string
		yes                   bool
		doctorOn              = func(env SetupEnv, path string) []doctor.Check {
			home := st.env.Getenv("HOME")
			exe, _ := env.Executable()
			return doctor.Checks(doctor.Deps{
				ConfigPath: path, Home: home, FS: rt.OSFS{}, LookPath: env.LookPath, Getenv: st.env.Getenv, RuntimeSockets: env.RuntimeSockets,
				Runner: env.Host, Yes: yes, GOOS: env.GOOS, User: env.User, Account: whrUser, UID: env.UID, Whr: exe, Prefix: prefix,
			})
		}
	)
	configPath := func() string {
		if st.configPath != "" {
			return st.configPath
		}
		return DefaultConfigPath(st.env.Getenv)
	}
	run := func(cmd *cobra.Command, phase doctor.Phase) (runErr error) {
		var reportSteps []doctor.Check
		noSudo := false
		var reportOutcomes []setup.Outcome
		var notWhr bool // notWhr: the administrator, not whr's account, runs this
		defer func() {
			if dryRun {
				return
			}
			repair := repairContext{Account: whrUser}
			if cmd.Flags().Changed("prefix") {
				repair.Prefix = prefix
			}
			repair.RunAs = reportRunAs(phase, notWhr, whrUser)
			presentation := setupPresentation(reportSteps, reportOutcomes, phase, repair)
			if runErr != nil && presentation.OK {
				detail := oneLineError(runErr)
				if detail == "" {
					detail = "setup did not complete"
				}
				presentation = doctor.Present(append(presentation.Checks, doctor.ReportCheck{Result: doctor.Result{Check: "setup", Phase: phase, Status: doctor.Fail, Detail: detail}}))
			}
			if reportErr := writeSetupReport(st, configPath(), presentation, "setup", phase, whrUser); reportErr != nil {
				fmt.Fprintf(st.env.Stderr, "warning: %s\n", oneLineError(reportErr))
			}
		}()
		style := st.style(st.env.Stderr, plain)
		ui := render.Writer{W: st.env.Stderr, S: style}
		env, err := st.env.Setup.resolve(st, style)
		if err != nil {
			return err
		}
		notWhr = env.User != whrUser
		if phase == "" {
			phase = autoPhase(cmd.Context(), env, whrUser, only, from)
		}
		if phase == doctor.PhaseHost && (answersPath != "" || savePath != "" || unattended) {
			return usageError{"the administrator's whr setup is never answered from a file and never unattended: it changes the host with sudo, and you confirm each step yourself (--answers, --save-answers and --unattended are for `whr setup`)"}
		}
		if unattended && answersPath == "" {
			return usageError{"--unattended needs --answers FILE: it asks nothing, and only a file can answer"}
		}
		if savePath != "" && dryRun {
			return usageError{"--save-answers saves what you answer, and a dry run asks nothing"}
		}
		if !dryRun && !unattended && !env.IsTerminal() {
			return usageError{"whr setup asks you questions and runs commands after your answer, so it needs a terminal: run it in one, add --dry-run to see what it would do, or give --answers FILE with --unattended"}
		}
		for name, v := range map[string]string{"--user": whrUser, "--answers": answersPath, "--save-answers": savePath} {
			if err := plainFlag(name, v); err != nil {
				return err
			}
		}
		if savePath != "" {
			if err := answers.CheckSavePath(savePath); err != nil {
				return usageError{"--save-answers: " + clean(err.Error()) + " (an existing file is never overwritten; remove it first)"}
			}
		}
		runLog, err := st.startRunLog(&env, "setup", logFile, verbose)
		if err != nil {
			return err
		}
		defer st.finishRunLog(runLog, style)
		exe, exeErr := env.Executable()
		prefix, err = installationPrefix(cmd, prefix)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if phase == doctor.PhaseHost {
			member, known, _ := doctor.Membership(ctx, env.Host, env.User)
			admin := member && env.User == whrUser // GuardHost needs the answer for the whr account only
			// an unreadable answer is no reason to warn (#507); root is refused below
			noSudo = known && !member
			if err := setup.GuardHost(env.User, env.UID, whrUser, admin); err != nil {
				return usageError{err.Error()}
			}
			if noSudo {
				ui.Report(render.LevelWarn, env.User+" is not an administrator and cannot sudo: the read-only checks run, each step that needs sudo is listed with its exact command for an administrator and not run, and the exit code is non-zero while steps are open")
			}
		} else {
			m := launchd.Manager{R: launchd.ExecRunner{}, UID: env.UID, GOOS: env.GOOS}
			if env.Manager != nil {
				m = *env.Manager
			}
			if err := setup.GuardUser(ctx, m, env.User, env.UID, whrUser); err != nil {
				if !dryRun {
					return usageError{err.Error()}
				}
				fmt.Fprintf(st.env.Stderr, "note (dry run): %s\n", oneLineError(err))
			}
		}
		if exeErr != nil {
			return exeErr
		}
		if err := launchd.CheckBinary(exe); err != nil {
			if !dryRun {
				return usageError{err.Error()}
			}
			fmt.Fprintf(st.env.Stderr, "note (dry run): %s\n", oneLineError(err))
		}
		steps := doctorOn(env, configPath())
		reportSteps = steps
		resume := []string{"whr", "setup"}
		if cmd.Flags().Changed("user") {
			resume = append(resume, "--user", whrUser)
		}
		if cmd.Flags().Changed("prefix") {
			resume = append(resume, "--prefix", prefix)
		}
		if answersPath != "" {
			resume = append(resume, "--answers", answersPath)
		}
		if unattended {
			resume = append(resume, "--unattended")
		}
		var setupLog *protocol.Log
		if !dryRun {
			open := env.OpenLog
			if open == nil {
				open = func(home string) (*protocol.Log, error) { return protocol.Open(home, nil, nil) }
			}
			lg, err := open(st.env.Getenv("HOME"))
			if err != nil {
				return fmt.Errorf("cannot open the setup protocol, so nothing is run: %w", err)
			}
			defer func() { _ = lg.Close() }()
			for _, w := range lg.Warnings() {
				fmt.Fprintf(st.env.Stderr, "note: %s\n", clean(w))
			}
			setupLog = lg
		}
		// D49: without separation and with remote access, one explicit y that
		// names the risk. Doctor fails on it too; nothing is enforced.
		for _, c := range steps {
			if c.Name != "account" {
				continue
			}
			if stt, detail := c.Run(ctx); stt == doctor.Fail && !dryRun {
				ui.Report(render.LevelFail, "account: "+clean(strings.TrimSpace(detail)))
				if unattended {
					if err := recordAccountStop(setupLog, env.User, phase, resume, st.env.Getenv("HOME"), c, protocol.AnswerNone, protocol.SourceNone, protocol.OutNeedsHuman, protocol.RunNeedsHuman); err != nil {
						return err
					}
					return needsHumanError{"unattended: account risk needs your confirmation; run the setup in a terminal"}
				}
				// accepting a risk is not undoable by running it again: Enter is no
				a, err := setup.Ask(env.Host, "Go on without a dedicated standard account, knowing this?", render.DefaultNo)
				if err == nil && a == render.Quit {
					if err := recordAccountStop(setupLog, env.User, phase, resume, st.env.Getenv("HOME"), c, protocol.AnswerQuit, protocol.SourceInteractive, protocol.OutQuit, protocol.RunQuit); err != nil {
						return err
					}
					ui.Report(render.LevelSkipped, "stopped at your request, nothing was run")
					fmt.Fprintf(st.env.Stderr, "to start again, run: %s\n", setup.QuoteArgv(resume))
					return quitError{}
				}
				if err != nil || a != render.Yes {
					return usageError{"stopped: set up a dedicated standard account, or remove the remote access from the configuration"}
				}
			} else if stt == doctor.Fail {
				fmt.Fprintf(st.env.Stderr, "note (dry run): account: %s\n", clean(strings.TrimSpace(detail)))
			}
		}
		for i, name := range only {
			if name == "whr-user" {
				only[i] = "workharbor-user"
			}
		}
		if from == "whr-user" {
			from = "workharbor-user"
		}
		so := setup.Options{
			Phase: phase, DryRun: dryRun, Only: only, From: from, Resume: resume, Out: st.env.Stdout, Err: st.env.Stderr, Style: style, Verbose: verbose,
			Unattended: unattended, Yes: yes, Account: env.User, Home: st.env.Getenv("HOME"),
			NoSudo: noSudo, Paged: !dryRun && !unattended && env.IsTerminal(),
		}
		if answersPath != "" {
			f, digest, err := loadAnswers(env, answersPath, st.env.Stderr)
			if err != nil {
				return usageError{err.Error()}
			}
			if f != nil {
				so.Answers, so.AnswersDigest = f, digest
			}
		}
		so.Log = setupLog
		so.RunLog = runLog

		outs, err := setup.Run(ctx, steps, env.Host, so)
		reportOutcomes = outs
		var quit *setup.QuitError
		isQuit := errors.As(err, &quit)
		if err != nil && !isQuit {
			var stopped *setup.InterruptedError
			if errors.As(err, &stopped) {
				printInterrupted(ui, stopped)
			}
			return runFailure(err)
		}
		var saveErr error
		if savePath != "" {
			saveErr = answers.CheckSavePath(savePath) // the file may have appeared during the run
			if saveErr == nil {
				saveErr = saveAnswers(env, savePath, steps, outs, st.env.Stderr)
			}
		}
		if isQuit {
			printQuit(ui, quit)
			if saveErr != nil {
				fmt.Fprintf(st.env.Stderr, "whr: %s\n", clean(saveErr.Error()))
			}
			return quitError{}
		}
		setup.Summary(st.env.Stderr, outs, so)
		if saveErr != nil {
			fmt.Fprintf(st.env.Stderr, "whr: %s\n", clean(saveErr.Error()))
			return quietError{}
		}
		if left := needsPerson(outs); unattended && len(left) > 0 {
			return needsHumanError{"unattended: " + strings.Join(left, ", ") + " need a person (the answers file does not decide them); run the setup in a terminal"}
		}
		var admin []string
		for _, o := range outs {
			if o.NeedsAdmin {
				admin = append(admin, o.Step)
			}
		}
		if len(admin) > 0 {
			fmt.Fprintf(st.env.Stderr, "whr: %s need an administrator (sudo): hand them the commands above; this run is not complete\n", strings.Join(admin, ", "))
			return quietError{}
		}
		for _, o := range outs {
			if o.Status == doctor.Fail {
				if dryRun {
					return quietError{}
				}
				fmt.Fprintln(st.env.Stderr, "whr: some steps are not done; run the setup again after you have dealt with them")
				return quietError{}
			}
		}
		fmt.Fprintln(st.env.Stderr, "all selected steps are done (or were not checked: see the lines marked not_verified); the doctor checks the rest")
		return nil
	}
	flags := func(c *cobra.Command) {
		f := c.Flags()
		f.BoolVar(&dryRun, "dry-run", false, "run the read-only checks for real and print every fix without running any")
		f.BoolVar(&plain, "plain", false, "no colour and no symbols beyond ASCII, as when the output is not a terminal; also no fzf")
		f.BoolVar(&verbose, "verbose", false, "also show the raw text of the tools a step ran")
		f.StringSliceVar(&only, "only", nil, "run only these steps (optional steps too)")
		f.StringVar(&logFile, "log-file", "", "write the run log to this `path` (default: a new file under the state directory logs/); follow it with tail -f in a second terminal")
		f.StringVar(&from, "from", "", "start at this step")
		f.StringVar(&whrUser, "user", doctor.WhrUser, "the account workharbor runs as")
		f.StringVar(&answersPath, "answers", "", "answer the questions of the steps this file decides (user part only; host steps, sudo and guided steps are always asked; with --yes, the steps the file leaves open are answered yes when they can be undone)")
		f.StringVar(&savePath, "save-answers", "", "save your run/skip answers of this run to this file (0600; never a password, token or key)")
		f.BoolVar(&yes, "yes", false, "answer yes to the questions you can undo; still ask before anything that cannot be undone, and sudo still asks for its password; the backup it makes covers only config.json (not the launchd plist or the tool store)")
		f.BoolVar(&unattended, "unattended", false, "ask nothing: run what --answers decides, leave the rest for you and exit 6 (user part only)")
		f.StringVar(&prefix, "prefix", doctor.DefaultPrefix, "the installation prefix (default: /opt/whr; any absolute directory works, the doctor warns when it is not administrator-owned)")
		names := func(phase doctor.Phase) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
				var out []string
				phases := []doctor.Phase{phase}
				if c.Name() == "setup" { // both parts: the run picks the one a named step belongs to
					phases = []doctor.Phase{doctor.PhaseHost, doctor.PhaseUser}
				}
				for _, ph := range phases {
					for _, s := range doctor.Steps(doctor.Checks(doctor.Deps{ConfigPath: "x/config.json"}), ph) {
						out = append(out, s.Name+"\t"+s.Title)
					}
				}
				return out, cobra.ShellCompDirectiveNoFileComp
			}
		}
		phase := doctor.PhaseUser
		if c.Name() == "setup" || c.Name() == "host" {
			phase = doctor.PhaseHost
		}
		_ = c.RegisterFlagCompletionFunc("only", names(phase))
		_ = c.RegisterFlagCompletionFunc("from", names(phase))
	}
	root := &cobra.Command{
		Use:   "setup",
		Short: "first-time setup: as the administrator it sets up the host and the workharbor account; as a standard workharbor account, its desktop-session part (provisional)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return run(cmd, "") },
	}
	flags(root)
	hostCmd := &cobra.Command{
		Use:    "host",
		Hidden: true,
		Short:  "alias of whr setup as the administrator (kept for the alpha.4 documentation)",
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, _ []string) error { return run(cmd, doctor.PhaseHost) },
	}
	flags(hostCmd)
	root.AddCommand(hostCmd)
	return root
}

func installationPrefix(cmd *cobra.Command, prefix string) (string, error) {
	if cmd.Flags().Changed("prefix") {
		if err := plainFlag("--prefix", prefix); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(prefix) {
		return "", usageError{"--prefix must be absolute"}
	}
	return filepath.Clean(prefix), nil
}

// plainFlag refuses a flag value with a control, bidirectional or separator
// character: the value is printed in the suggested next command, which a human
// may paste, and a newline in it would start a second command.
func plainFlag(name, value string) error {
	if strings.IndexFunc(value, func(r rune) bool { return textsafe.IsControl(r) || textsafe.IsBidiOrSeparator(r) || r == '\t' }) >= 0 {
		return usageError{name + " must not contain a control, bidirectional or separator character"}
	}
	return nil
}

// runFailure is the error of a run that stopped: a usage error when the wizard
// refused the command line, the plain error when the protocol could not be
// written.
func runFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return interruptedError{}
	}
	if errors.Is(err, setup.ErrUnattended) || strings.HasPrefix(err.Error(), "setup protocol:") {
		return err
	}
	return usageError{err.Error()}
}

// needsPerson names the steps an unattended run left for a person.
func needsPerson(outs []setup.Outcome) []string {
	var left []string
	for _, o := range outs {
		if o.NeedsHuman {
			left = append(left, o.Step)
		}
	}
	return left
}

// loadAnswers reads the answer file the person named and says whether it may
// be used: only a file of this build (its identity) and of the running account
// answers; anything else is noted and every step is asked. The digest is of
// the bytes that were read.
func loadAnswers(env SetupEnv, path string, errW io.Writer) (*answers.File, string, error) {
	f, data, warnings, err := answers.LoadRaw(path, os.Getuid())
	if err != nil {
		return nil, "", fmt.Errorf("cannot use the answers file: %w", err)
	}
	for _, w := range warnings {
		fmt.Fprintf(errW, "warning: %s\n", clean(w))
	}
	identity := env.Identity
	if identity == nil {
		identity = answers.Identity
	}
	id, err := identity()
	switch {
	case err != nil:
		fmt.Fprintf(errW, "note: the answers file is not used, every step is asked: %s\n", clean(err.Error()))
		return nil, "", nil
	case f.Whr != id:
		fmt.Fprintf(errW, "note: the answers file is not used, every step is asked: it was saved by %s and this is %s\n", clean(f.Whr), clean(id))
		return nil, "", nil
	case f.Account != env.User:
		fmt.Fprintf(errW, "note: the answers file is not used, every step is asked: it is for the account %s and this is %s\n", clean(f.Account), clean(env.User))
		return nil, "", nil
	}
	return &f, protocol.AnswersDigest(data), nil
}

// saveAnswers writes the run/skip decisions of this run, from the person or the
// file, bound to this build and account.
func saveAnswers(env SetupEnv, path string, steps []doctor.Check, outs []setup.Outcome, errW io.Writer) error {
	var f answers.File
	f.Account = env.User
	for _, o := range outs {
		eligible := false
		for _, c := range steps {
			if c.Name == o.Step {
				eligible, _ = answers.Eligible(c)
				break
			}
		}
		if o.Decision != "" && eligible {
			f.Answers = append(f.Answers, answers.Entry{Step: o.Step, Fix: o.Fix, Answer: o.Decision})
		}
	}
	if len(f.Answers) == 0 {
		fmt.Fprintln(errW, "note: no answers to save: only your run or skip answers to a step that a file may decide are kept")
		return nil
	}
	identity := env.Identity
	if identity == nil {
		identity = answers.Identity
	}
	id, err := identity()
	if err != nil {
		return fmt.Errorf("answers not saved: %w", err)
	}
	if err := answers.SaveAs(id, path, f, steps); err != nil {
		return fmt.Errorf("answers not saved: %w", err)
	}
	fmt.Fprintf(errW, "saved %d answers to %s\n", len(f.Answers), clean(path))
	return nil
}

// recordAccountStop records a run stopped by the shared account-risk preflight,
// before the phase-specific engine starts. It uses the existing step vocabulary.
func recordAccountStop(lg *protocol.Log, account string, phase doctor.Phase, resume []string, home string, c doctor.Check, answer, source, outcome, end string) error {
	entries := []protocol.Entry{
		{Event: protocol.EventRunStart, Source: protocol.SourceInteractive, Flags: protocol.Flags(resume, home)},
		{Event: protocol.EventStepBefore, Step: c.Name, Fix: answers.FixDigest(c), Status: string(doctor.Fail), Answer: answer, Source: source},
		{Event: protocol.EventStepAfter, Step: c.Name, Status: string(doctor.Fail), Outcome: outcome},
		{Event: protocol.EventRunEnd, Outcome: end},
	}
	for _, e := range entries {
		e.Account, e.Cmd, e.Phase = account, protocol.CmdSetup, string(phase)
		if err := lg.Append(e); err != nil {
			return fmt.Errorf("setup protocol: %w", err)
		}
	}
	return nil
}

// reportRunAs is the account a user-phase fix in the report is for: the host
// phase names user-phase steps too, and when the administrator (not whr's own
// account) runs it, those fixes are run as whr's account.
func reportRunAs(phase doctor.Phase, notWhr bool, whrUser string) string {
	if phase == doctor.PhaseHost && notWhr {
		return whrUser
	}
	return ""
}
