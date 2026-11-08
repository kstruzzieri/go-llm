//go:build darwin

package tools

import (
	"fmt"
	"os"
)

// excMaskRPCAlert (EXC_MASK_RPC_ALERT) is a type the kernel never raises, so a
// helper can hold an unserviced handler for it without stalling any thread.
const excMaskRPCAlert = 1 << 9

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
func platformHelperMain(args []string) (int, bool) {
	var err error
	switch args[0] {
	case "machregclear":
		err = helperMachRegClear()
	case "machinspect":
		err = helperMachInspect()
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
