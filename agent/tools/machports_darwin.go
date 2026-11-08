//go:build darwin

package tools

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"unsafe"
)

// Mach constants, checked against the macOS SDK headers for arm64 and x86_64.
const (
	kernSuccess          = 0          // KERN_SUCCESS
	machPortNull         = 0          // MACH_PORT_NULL
	machPortDead         = ^uint32(0) // MACH_PORT_DEAD: a dead right, no name to release
	machPortRightReceive = 1          // MACH_PORT_RIGHT_RECEIVE
	machMsgTypeMakeSend  = 20         // MACH_MSG_TYPE_MAKE_SEND
	taskBootstrapPort    = 4          // TASK_BOOTSTRAP_PORT
	taskPortRegisterMax  = 3          // TASK_PORT_REGISTER_MAX
	exceptionDefault     = 1          // EXCEPTION_DEFAULT
	excTypesCount        = 14         // EXC_TYPES_COUNT
	// excMaskAllPorts is EXC_MASK_ALL | EXC_MASK_CRASH | EXC_MASK_CORPSE_NOTIFY:
	// EXC_MASK_ALL alone omits the last two.
	excMaskAllPorts = 0x3FFE
)

// errRegisteredPortsNotEmpty: the registered-port table still holds a port
// after clearing it, so a spawned child would inherit a launchd connection.
var errRegisteredPortsNotEmpty = errors.New("tools: seatbelt: registered Mach ports remain after clearing")

// registeredPortsGuard runs before every native Seatbelt spawn. Tests swap it
// to prove the spawn fails closed; production never reassigns it.
var registeredPortsGuard = ensureRegisteredPortsEmpty

var taskSelf = sync.OnceValue(taskSelfTrap)

// machTaskSelf returns this task's port name. task_self_trap adds a reference
// on every call, so it runs once and the name is cached.
func machTaskSelf() uint32 { return taskSelf() }

func machErr(op string, kr int32) error {
	return fmt.Errorf("%s: kern_return %#x", op, kr)
}

// lookupRegisteredPorts copies the task's registered-port table
// (mach_ports_lookup) and frees the out-of-line array it arrives in. Every
// non-null, non-dead slot is a send right now held by this task; the caller
// owns those references.
func lookupRegisteredPorts() ([]uint32, error) {
	self := machTaskSelf()
	var arr *uint32
	var n uint32
	if kr := machPortsLookup(self, &arr, &n); kr != kernSuccess {
		return nil, machErr("mach_ports_lookup", kr)
	}
	if arr == nil {
		if n != 0 {
			return nil, fmt.Errorf("mach_ports_lookup: %d ports reported with no array", n)
		}
		return nil, nil
	}
	ports := slices.Clone(unsafe.Slice(arr, n))
	if kr := vmDeallocate(self, uintptr(unsafe.Pointer(arr)), uintptr(n)*4); kr != kernSuccess {
		return ports, machErr("vm_deallocate", kr)
	}
	return ports, nil
}

// registeredPortsEmpty reports whether every registered-port slot is
// MACH_PORT_NULL. It runs before every spawn in a long-lived host, so it
// releases the array and every send right the lookup copied out.
func registeredPortsEmpty() (bool, error) {
	ports, err := lookupRegisteredPorts()
	empty := true
	for _, p := range ports {
		if p == machPortNull {
			continue
		}
		empty = false
		if p == machPortDead {
			continue
		}
		if kr := machPortDeallocate(machTaskSelf(), p); kr != kernSuccess {
			err = errors.Join(err, machErr(fmt.Sprintf("mach_port_deallocate(%#x)", p), kr))
		}
	}
	return empty, err
}

// registerNoPorts empties this task's registered-port table.
func registerNoPorts() error {
	if kr := machPortsRegister(machTaskSelf(), nil, 0); kr != kernSuccess {
		return machErr("mach_ports_register", kr)
	}
	return nil
}

// ensureRegisteredPortsEmpty makes sure this process's registered-port table
// is empty, so a spawned child cannot inherit launchd's bootstrap port
// through it. It is process-wide and idempotent.
func ensureRegisteredPortsEmpty() error {
	return ensureRegisteredPortsEmptyWith(registeredPortsEmpty, registerNoPorts)
}

// ensureRegisteredPortsEmptyWith checks the table with empty and, if it is not
// empty, clears it with clear and checks again. A table still not empty is
// errRegisteredPortsNotEmpty; an error from either function is wrapped. It
// keeps no state, so concurrent callers are safe.
func ensureRegisteredPortsEmptyWith(empty func() (bool, error), clear func() error) error {
	ok, err := empty()
	if err != nil {
		return fmt.Errorf("tools: seatbelt: read registered Mach ports: %w", err)
	}
	if ok {
		return nil
	}
	if err := clear(); err != nil {
		return fmt.Errorf("tools: seatbelt: clear registered Mach ports: %w", err)
	}
	ok, err = empty()
	if err != nil {
		return fmt.Errorf("tools: seatbelt: re-read registered Mach ports: %w", err)
	}
	if !ok {
		return errRegisteredPortsNotEmpty
	}
	return nil
}

// machState is a process's launchd-relevant Mach state, for tests.
//
// Bootstrap, Registered and the exception fields come from getters that must
// succeed: inspectMachState fails rather than report a zero value. Other holds
// the informational special ports (inspect, read, access, debug-control,
// resource-notify) by name; a getter the kernel refuses leaves its name out of
// Other and records the kern_return_t in OtherRefused instead of failing.
type machState struct {
	Bootstrap         uint32
	Registered        []uint32
	ExceptionMasks    []uint32
	ExceptionHandlers []uint32
	Other             map[string]uint32
	OtherRefused      map[string]int32
}

// inspectMachState reads this process's machState. The port names it returns
// are send rights left in this task (a test may reinstall one), so it is for
// short-lived test processes only.
func inspectMachState() (machState, error) {
	self := machTaskSelf()
	st := machState{Other: map[string]uint32{}, OtherRefused: map[string]int32{}}
	if kr := taskGetSpecialPort(self, taskBootstrapPort, &st.Bootstrap); kr != kernSuccess {
		return machState{}, machErr("task_get_special_port(bootstrap)", kr)
	}
	for _, sp := range []struct {
		name  string
		which int32
	}{{"inspect", 5}, {"read", 6}, {"access", 9}, {"debug-control", 10}, {"resource-notify", 11}} {
		var p uint32
		if kr := taskGetSpecialPort(self, sp.which, &p); kr != kernSuccess {
			st.OtherRefused[sp.name] = kr
			continue
		}
		st.Other[sp.name] = p
	}
	regs, err := lookupRegisteredPorts()
	if err != nil {
		return machState{}, err
	}
	st.Registered = regs
	var (
		masks     [excTypesCount]uint32
		handlers  [excTypesCount]uint32
		behaviors [excTypesCount]int32
		flavors   [excTypesCount]int32
		n         = uint32(excTypesCount)
	)
	if kr := taskGetExceptionPorts(self, excMaskAllPorts, &masks[0], &n, &handlers[0], &behaviors[0], &flavors[0]); kr != kernSuccess {
		return machState{}, machErr("task_get_exception_ports", kr)
	}
	st.ExceptionMasks = slices.Clone(masks[:n])
	st.ExceptionHandlers = slices.Clone(handlers[:n])
	return st, nil
}
