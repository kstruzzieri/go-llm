//go:build darwin

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// excMaskRPCAlert (EXC_MASK_RPC_ALERT) is a type the kernel never raises, so a
// helper can hold an unserviced handler for it without stalling any thread.
const excMaskRPCAlert = 1 << 9

// platformSpawnerCases adds the Seatbelt backend's spawner, so the unix runner
// and starter suites also run through nativeCmd.
func platformSpawnerCases() []spawnerCase { return []spawnerCase{{"native", nativeSpawn}} }

// platformHelperMain runs the darwin-only helper modes. handled is false for
// any other mode, leaving helperMain's fallthrough unchanged.
//
//	["machregclear"] -> seed all registered slots, require the lookup to see
//	                    them without leaking rights, require
//	                    ensureRegisteredPortsEmpty to empty the table, print
//	                    REGCLEAR-OK
//	["machinspect"]  -> require inspectMachState to succeed with a live
//	                    bootstrap port, to see a seeded exception handler and
//	                    a set bootstrap port, print the state and INSPECT-OK
//	["machstate"]    -> print inspectMachState as one MACHSTATE <json> line
//	["nativedriver"] -> seed registered ports and exception receivers, spawn
//	                    machstate through nativeCmd, require the child to have
//	                    no launchd connection, print NATIVEDRIVER-OK
//	["sigdriver"]    -> ignore SIGHUP, spawn sigignored through nativeCmd and
//	                    os/exec, require both to report it ignored, print
//	                    SIGDRIVER-OK
//	["sigignored"]   -> print whether SIGHUP is ignored
//	["usr1driver"]   -> block SIGUSR1 on this thread, spawn a shell that sends
//	                    itself SIGUSR1 through nativeCmd and os/exec, require
//	                    the native child to die of it and the os/exec child to
//	                    survive it, print USR1DRIVER-OK
//	["fddriver"]     -> hold an inheritable fd, spawn fdprobe through nativeCmd
//	                    and os/exec, require only the os/exec child to see it,
//	                    print FDDRIVER-OK
//	["fdprobe", fd]  -> print whether fd is open
func platformHelperMain(args []string) (int, bool) {
	var err error
	switch args[0] {
	case "machregclear":
		err = helperMachRegClear()
	case "machinspect":
		err = helperMachInspect()
	case "machstate":
		err = helperMachState()
	case "nativedriver":
		err = helperNativeDriver()
	case "sigdriver":
		err = helperSigDriver()
	case "sigignored":
		_, _ = fmt.Printf("signal.Ignored(SIGHUP)=%v\n", signal.Ignored(syscall.SIGHUP))
	case "usr1driver":
		err = helperUSR1Driver()
	case "fddriver":
		err = helperFDDriver()
	case "fdprobe":
		err = helperFDProbe(args[1:])
	default:
		return 0, false
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, args[0]+":", err)
		return 1, true
	}
	return 0, true
}

func helperMachRegClear() error {
	if err := seedRegisteredPorts(); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	st, err := inspectMachState()
	if err != nil {
		return err
	}
	seeded := st.Registered[0]
	for i, p := range st.Registered {
		if p == machPortNull || p != seeded {
			return fmt.Errorf("seeded table reads %#x, want one port in every slot (slot %d)", st.Registered, i)
		}
	}
	for range 100 {
		empty, err := registeredPortsEmpty()
		if err != nil {
			return fmt.Errorf("lookup of a seeded table: %w", err)
		}
		if empty {
			return fmt.Errorf("lookup reports a seeded table empty")
		}
	}
	// The seed holds one send uref and the inspection copied out three more.
	// Holding the receive right keeps the name alive once the send urefs are
	// gone, so draining them counts exactly what the 100 lookups left behind.
	held := 0
	for held <= 1000 && machPortDeallocate(machTaskSelf(), seeded) == kernSuccess {
		held++
	}
	if held != 4 {
		return fmt.Errorf("seeded port held %d send urefs after 100 lookups, want 4 (lookup leaks rights)", held)
	}
	if err := ensureRegisteredPortsEmpty(); err != nil {
		return fmt.Errorf("ensure: %w", err)
	}
	empty, err := registeredPortsEmpty()
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("table not empty after ensureRegisteredPortsEmpty")
	}
	_, _ = fmt.Println("REGCLEAR-OK")
	return nil
}

func helperMachInspect() error {
	st, err := inspectMachState()
	if err != nil {
		return err
	}
	_, _ = fmt.Printf("state: %+v\n", st)
	if st.Bootstrap == machPortNull {
		return fmt.Errorf("bootstrap port is null in a normal process")
	}
	// Exception getter: a seeded handler must show up under its mask.
	if err := seedExceptionReceiver(excMaskRPCAlert); err != nil {
		return fmt.Errorf("seed exception receiver: %w", err)
	}
	seeded, err := inspectMachState()
	if err != nil {
		return err
	}
	found := false
	for i, m := range seeded.ExceptionMasks {
		if m&excMaskRPCAlert != 0 && seeded.ExceptionHandlers[i] != machPortNull {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("seeded RPC_ALERT handler not reported: masks %#x handlers %#x", seeded.ExceptionMasks, seeded.ExceptionHandlers)
	}
	// Special-port setter and getter agree, then the original is restored.
	if err := setBootstrapPort(machPortNull); err != nil {
		return err
	}
	cleared, err := inspectMachState()
	if err != nil {
		return err
	}
	if cleared.Bootstrap != machPortNull {
		return fmt.Errorf("bootstrap reads %#x after setting null", cleared.Bootstrap)
	}
	if err := setBootstrapPort(st.Bootstrap); err != nil {
		return err
	}
	restored, err := inspectMachState()
	if err != nil {
		return err
	}
	if restored.Bootstrap != st.Bootstrap {
		return fmt.Errorf("bootstrap reads %#x after restoring %#x", restored.Bootstrap, st.Bootstrap)
	}
	_, _ = fmt.Println("INSPECT-OK")
	return nil
}

// runNative runs c with stdout and stderr captured together (one shared pipe,
// as os/exec does for an equal writer) and returns the output, the final
// state and the Start or Wait error.
func runNative(c *nativeCmd) (string, *os.ProcessState, error) {
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Start()
	if err == nil {
		err = c.Wait()
	}
	return out.String(), c.State(), err
}

// nativeHelper builds a nativeCmd that runs this binary's helper mode args.
func nativeHelper(args ...string) (*nativeCmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return newNativeCmd(context.Background(), self, append([]string{self, "__golem_exec_helper__"}, args...)), nil
}

// osExecHelper runs this binary's helper mode args through os/exec, the
// control the native spawns are compared with.
func osExecHelper(args ...string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(self, append([]string{"__golem_exec_helper__"}, args...)...).CombinedOutput()
	return string(out), err
}

func helperMachState() error {
	st, err := inspectMachState()
	if err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, _ = fmt.Printf("MACHSTATE %s\n", b)
	return nil
}

// machStateFromOutput parses the MACHSTATE line a machstate helper printed.
func machStateFromOutput(out string) (machState, error) {
	for line := range strings.Lines(out) {
		if js, ok := strings.CutPrefix(line, "MACHSTATE "); ok {
			var st machState
			err := json.Unmarshal([]byte(js), &st)
			return st, err
		}
	}
	return machState{}, fmt.Errorf("no MACHSTATE line in %q", out)
}

// noLaunchdConnection requires a null bootstrap port, an empty registered
// table, and null exception handlers for every mask in excMaskAllPorts.
func noLaunchdConnection(st machState) error {
	var errs []error
	if st.Bootstrap != machPortNull {
		errs = append(errs, fmt.Errorf("bootstrap port %#x, want null", st.Bootstrap))
	}
	for i, p := range st.Registered {
		if p != machPortNull {
			errs = append(errs, fmt.Errorf("registered slot %d holds %#x, want null", i, p))
		}
	}
	var covered uint32
	for i, m := range st.ExceptionMasks {
		covered |= m
		if st.ExceptionHandlers[i] != machPortNull {
			errs = append(errs, fmt.Errorf("exception mask %#x has handler %#x, want null", m, st.ExceptionHandlers[i]))
		}
	}
	if covered&excMaskAllPorts != excMaskAllPorts {
		errs = append(errs, fmt.Errorf("exception masks %#x cover %#x, want %#x", st.ExceptionMasks, covered, excMaskAllPorts))
	}
	return errors.Join(errs...)
}

// excMaskSeeded is EXC_MASK_CORPSE_NOTIFY | EXC_MASK_CRASH | EXC_MASK_BAD_ACCESS:
// two masks EXC_MASK_ALL leaves out, and one it covers.
const excMaskSeeded = 0x2402

func helperNativeDriver() error {
	if err := seedRegisteredPorts(); err != nil {
		return fmt.Errorf("seed registered ports: %w", err)
	}
	if err := seedExceptionReceiver(excMaskSeeded); err != nil {
		return fmt.Errorf("seed exception receiver: %w", err)
	}
	seeded, err := inspectMachState()
	if err != nil {
		return err
	}
	_, _ = fmt.Printf("driver state: %+v\n", seeded)
	if len(seeded.Registered) == 0 || seeded.Bootstrap == machPortNull {
		return fmt.Errorf("seeds not visible: bootstrap %#x, registered %#x", seeded.Bootstrap, seeded.Registered)
	}
	for i, p := range seeded.Registered {
		if p == machPortNull {
			return fmt.Errorf("registered slot %d null after seeding", i)
		}
	}
	var held uint32
	for i, m := range seeded.ExceptionMasks {
		if seeded.ExceptionHandlers[i] != machPortNull {
			held |= m
		}
	}
	if held&excMaskSeeded != excMaskSeeded {
		return fmt.Errorf("seeded exception handlers cover %#x, want %#x", held, excMaskSeeded)
	}
	c, err := nativeHelper("machstate")
	if err != nil {
		return err
	}
	out, _, err := runNative(c)
	_, _ = fmt.Printf("child output:\n%s", out)
	if err != nil {
		return fmt.Errorf("native machstate: %w", err)
	}
	st, err := machStateFromOutput(out)
	if err != nil {
		return err
	}
	if err := noLaunchdConnection(st); err != nil {
		return fmt.Errorf("native child: %w", err)
	}
	_, _ = fmt.Println("NATIVEDRIVER-OK")
	return nil
}

func helperSigDriver() error {
	signal.Ignore(syscall.SIGHUP)
	c, err := nativeHelper("sigignored")
	if err != nil {
		return err
	}
	native, _, err := runNative(c)
	if err != nil {
		return fmt.Errorf("native sigignored: %w: %s", err, native)
	}
	control, err := osExecHelper("sigignored")
	if err != nil {
		return fmt.Errorf("os/exec sigignored: %w: %s", err, control)
	}
	_, _ = fmt.Printf("native: %sos/exec: %s", native, control)
	if want := "signal.Ignored(SIGHUP)=true\n"; native != want || control != want {
		return fmt.Errorf("children report native %q, os/exec %q, want both %q", native, control, want)
	}
	_, _ = fmt.Println("SIGDRIVER-OK")
	return nil
}

func helperUSR1Driver() error {
	// The block applies to this OS thread only, and both spawns below run on it.
	runtime.LockOSThread()
	set := uint32(1) << (syscall.SIGUSR1 - 1)
	if rc := pthreadSigmask(sigBlock, &set, nil); rc != 0 {
		return fmt.Errorf("pthread_sigmask: %w", syscall.Errno(rc))
	}
	const script = "kill -USR1 $$; echo survived"
	out, st, err := runNative(newNativeCmd(context.Background(), "/bin/sh", []string{"sh", "-c", script}))
	control, cerr := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	_, _ = fmt.Printf("native: %q state %v err %v\nos/exec: %q err %v\n", out, st, err, control, cerr)
	if st == nil {
		return fmt.Errorf("native spawn did not run: %w", err)
	}
	ws := st.Sys().(syscall.WaitStatus)
	if strings.Contains(out, "survived") || !ws.Signaled() || ws.Signal() != syscall.SIGUSR1 {
		return fmt.Errorf("native child inherited the blocked SIGUSR1: output %q, state %v", out, st)
	}
	// The control: os/exec's fork saves and restores the thread's current
	// mask (runtime BeforeFork/AfterForkInChild), so its child inherits the
	// block and survives. Without this the native result would prove nothing.
	if cerr != nil || string(control) != "survived\n" {
		return fmt.Errorf("os/exec control did not inherit the blocked SIGUSR1: output %q, err %v", control, cerr)
	}
	_, _ = fmt.Println("USR1DRIVER-OK")
	return nil
}

func helperFDDriver() error {
	// syscall.Open, unlike os.Open, leaves the descriptor inheritable; dup2 to
	// a high number keeps the child's own startup descriptors off it.
	fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
	if err != nil {
		return err
	}
	const leaked = 77
	if err := syscall.Dup2(fd, leaked); err != nil {
		return err
	}
	_ = syscall.Close(fd)
	arg := strconv.Itoa(leaked)
	c, err := nativeHelper("fdprobe", arg)
	if err != nil {
		return err
	}
	native, _, err := runNative(c)
	if err != nil {
		return fmt.Errorf("native fdprobe: %w: %s", err, native)
	}
	control, err := osExecHelper("fdprobe", arg)
	if err != nil {
		return fmt.Errorf("os/exec fdprobe: %w: %s", err, control)
	}
	_, _ = fmt.Printf("native: %sos/exec: %s", native, control)
	if native != "fd 77: EBADF\n" || control != "fd 77: open\n" {
		return fmt.Errorf("native %q, os/exec %q: want the native child without fd 77 and the os/exec child with it", native, control)
	}
	_, _ = fmt.Println("FDDRIVER-OK")
	return nil
}

func helperFDProbe(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("want one fd argument")
	}
	fd, err := strconv.Atoi(args[0])
	if err != nil {
		return err
	}
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	switch {
	case err == nil:
		_, _ = fmt.Printf("fd %d: open\n", fd)
	case errors.Is(err, unix.EBADF):
		_, _ = fmt.Printf("fd %d: EBADF\n", fd)
	default:
		return err
	}
	return nil
}
