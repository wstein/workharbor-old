package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: whr-shim <run|kill|chown|reserve-env|release-env|clear-env> [flags] [cmd...]\n")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "kill":
		killCmd(os.Args[2:])
	case "reserve-env":
		if err := reserveCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "reservation: %v\n", err)
			os.Exit(1)
		}
	case "clear-env":
		if err := clearCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
			os.Exit(1)
		}
	case "release-env":
		if err := releaseCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "release: %v\n", err)
			os.Exit(1)
		}
	case "chown":
		if err := chownCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "chown: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(2)
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	pidfile := fs.String("pidfile", "", "path to write the child PGID/PID")
	envfile := fs.String("env-file", "", "private one-use environment file")
	cancelFile := fs.String("cancel-file", "", "private execution cancellation marker")
	_ = fs.Parse(args)
	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintf(os.Stderr, "run requires a command to execute\n")
		os.Exit(2)
	}
	if *pidfile == "" {
		fmt.Fprintf(os.Stderr, "run requires -pidfile\n")
		os.Exit(2)
	}

	if cancellationRequested(*cancelFile) {
		os.Exit(130)
	}
	var childEnv []string
	executable := cmdArgs[0]
	if *envfile != "" {
		env, err := readExecEnvironment(*envfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "environment: %v\n", err)
			os.Exit(1)
		}
		childEnv = append(os.Environ(), env...)
		executable, err = resolveExecPath(executable, childEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "command: %v\n", err)
			os.Exit(1)
		}
	}
	cmd := exec.CommandContext(context.Background(), executable, cmdArgs[1:]...) //nolint:gosec // launcher runs the commanded child process
	cmd.Env = childEnv
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start failed: %v\n", err)
		os.Exit(1)
	}

	pid := cmd.Process.Pid
	if err := writePIDFile(*pidfile, pid); err != nil {
		fmt.Fprintf(os.Stderr, "write pidfile failed: %v\n", err)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		os.Exit(1)
	}

	if cancellationRequested(*cancelFile) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(-pid, s)
			}
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	// Remove the pidfile before exiting on every path: os.Exit skips deferred
	// calls, and a stale pidfile would let a later kill signal whatever
	// process group reuses this ID.
	_ = os.Remove(*pidfile)
	os.Exit(exitCode(err))
}

// exitCode maps the child's end to the shim's exit status: the child's own
// status, or 128 plus the signal that ended it.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
	}
	return 1
}

// writePIDFile writes the PID through a temporary file and a rename, so kill
// never reads a partly written file.
func writePIDFile(path string, pid int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := fmt.Fprintf(tmp, "%d\n", pid); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func killCmd(args []string) {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	pidfile := fs.String("pidfile", "", "path to read the child PGID/PID")
	cancelFile := fs.String("cancel-file", "", "private execution cancellation marker")
	grace := fs.Duration("grace", 500*time.Millisecond, "grace period before SIGKILL")
	_ = fs.Parse(args)

	if *pidfile == "" {
		fmt.Fprintf(os.Stderr, "kill requires -pidfile\n")
		os.Exit(2)
	}

	if err := markCancelled(*cancelFile, *pidfile); err != nil {
		fmt.Fprintf(os.Stderr, "cancel: %v\n", err)
		os.Exit(1)
	}
	data, err := os.ReadFile(*pidfile)
	if err != nil {
		if os.IsNotExist(err) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "read pidfile: %v\n", err)
		os.Exit(1)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		fmt.Fprintf(os.Stderr, "invalid pid in pidfile: %q\n", string(data))
		os.Exit(1)
	}

	t0 := time.Now()
	// Send SIGINT to the entire process group
	_ = syscall.Kill(-pid, syscall.SIGINT)

	dead := false
	deadline := time.Now().Add(*grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); err != nil {
			dead = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !dead {
		// Send SIGKILL to the entire process group
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		killDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(killDeadline) {
			if err := syscall.Kill(-pid, 0); err != nil {
				dead = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	elapsed := time.Since(t0)
	_ = os.Remove(*pidfile)
	if !dead {
		fmt.Fprintf(os.Stderr, "process group %d still alive after %v\n", pid, elapsed)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "killed process group %d in %v\n", pid, elapsed)
}

// chownCmd gives one directory to a numeric user, without following a link
// and without recursing: `whr-shim chown -owner 1000:1000 /v`. The runtime
// adapter runs it as root on a new volume, from the tool store, so that no
// program of the environment's image runs as root (design §5.1).
func chownCmd(args []string) error {
	fs := flag.NewFlagSet("chown", flag.ContinueOnError)
	owner := fs.String("owner", "", "numeric uid:gid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("one directory is needed")
	}
	u, g, ok := strings.Cut(*owner, ":")
	uid, uerr := strconv.Atoi(u)
	gid, gerr := strconv.Atoi(g)
	if !ok || uerr != nil || gerr != nil || uid <= 0 || gid < 0 {
		return fmt.Errorf("-owner %q must be numeric uid:gid with a non-root uid", *owner)
	}
	dir := fs.Arg(0)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return os.Lchown(dir, uid, gid)
}
