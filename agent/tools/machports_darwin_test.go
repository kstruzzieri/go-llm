//go:build darwin

package tools

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runMachHelper runs a darwin helper mode in its own process, so seeding and
// clearing never touch this test binary's own Mach state.
func runMachHelper(t *testing.T, mode, want string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(self, "__golem_exec_helper__", mode).CombinedOutput()
	t.Logf("%s output:\n%s", mode, out)
	if err != nil {
		t.Fatalf("helper %s: %v", mode, err)
	}
	if !strings.Contains(string(out), want) {
		t.Fatalf("helper %s did not print %s", mode, want)
	}
}

func TestMachRegisteredPortsEmptyAfterEnsure(t *testing.T) {
	runMachHelper(t, "machregclear", "REGCLEAR-OK")
}

func TestInspectMachStateRequiresSuccess(t *testing.T) {
	runMachHelper(t, "machinspect", "INSPECT-OK")
}

func TestEnsureRegisteredPortsEmptyFailsClosed(t *testing.T) {
	errLookup := errors.New("lookup refused")
	errClear := errors.New("register refused")
	cases := []struct {
		name      string
		empty     []bool // successive empty() answers; the last repeats
		emptyErr  error  // returned by every empty() call when set
		clearErr  error  // returned by clear()
		wantErr   error  // errors.Is target; nil means success
		wantClear int
	}{
		{name: "clear does nothing", empty: []bool{false}, wantErr: errRegisteredPortsNotEmpty, wantClear: 1},
		{name: "lookup fails", emptyErr: errLookup, wantErr: errLookup, wantClear: 0},
		{name: "clear fails", empty: []bool{false}, clearErr: errClear, wantErr: errClear, wantClear: 1},
		{name: "clear works", empty: []bool{false, true}, wantClear: 1},
		{name: "already empty", empty: []bool{true}, wantClear: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, clears := 0, 0
			empty := func() (bool, error) {
				if tc.emptyErr != nil {
					return false, tc.emptyErr
				}
				v := tc.empty[min(calls, len(tc.empty)-1)]
				calls++
				return v, nil
			}
			clear := func() error {
				clears++
				return tc.clearErr
			}
			err := ensureRegisteredPortsEmptyWith(empty, clear)
			if clears != tc.wantClear {
				t.Errorf("clear called %d times, want %d", clears, tc.wantClear)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if err != nil && (err == tc.emptyErr || err == tc.clearErr) {
				t.Errorf("err = %v returned bare, want it wrapped with context", err)
			}
		})
	}
}

// Test-only Mach seeding. These change the calling process's own task state,
// so tests call them from helper processes only.

// newReceivePortWithSend allocates a port this task receives on and holds one
// send uref for.
func newReceivePortWithSend() (uint32, error) {
	self := machTaskSelf()
	var p uint32
	if kr := machPortAllocate(self, machPortRightReceive, &p); kr != kernSuccess {
		return 0, machErr("mach_port_allocate", kr)
	}
	if kr := machPortInsertRight(self, p, p, machMsgTypeMakeSend); kr != kernSuccess {
		return 0, machErr("mach_port_insert_right", kr)
	}
	return p, nil
}

// seedRegisteredPorts registers one fresh port in every registered slot.
func seedRegisteredPorts() error {
	p, err := newReceivePortWithSend()
	if err != nil {
		return err
	}
	ports := [taskPortRegisterMax]uint32{p, p, p}
	if kr := machPortsRegister(machTaskSelf(), &ports[0], uint32(len(ports))); kr != kernSuccess {
		return machErr("mach_ports_register", kr)
	}
	return nil
}

// seedExceptionReceiver installs a fresh port as the task's exception handler
// for mask (EXCEPTION_DEFAULT, THREAD_STATE_NONE). Nothing services the port,
// so mask must name exception types the process never raises.
func seedExceptionReceiver(mask uint32) error {
	p, err := newReceivePortWithSend()
	if err != nil {
		return err
	}
	if kr := taskSetExceptionPorts(machTaskSelf(), mask, p, exceptionDefault, threadStateNone); kr != kernSuccess {
		return machErr("task_set_exception_ports", kr)
	}
	return nil
}

// setBootstrapPort replaces the task's bootstrap special port.
func setBootstrapPort(port uint32) error {
	if kr := taskSetSpecialPort(machTaskSelf(), taskBootstrapPort, port); kr != kernSuccess {
		return machErr("task_set_special_port", kr)
	}
	return nil
}
