//go:build darwin && arm64

#include "textflag.h"

TEXT libc_task_self_trap_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_task_self_trap(SB)
GLOBL	·libc_task_self_trap_trampoline_addr(SB), RODATA, $8
DATA	·libc_task_self_trap_trampoline_addr(SB)/8, $libc_task_self_trap_trampoline<>(SB)

TEXT libc_mach_ports_lookup_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_ports_lookup(SB)
GLOBL	·libc_mach_ports_lookup_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_ports_lookup_trampoline_addr(SB)/8, $libc_mach_ports_lookup_trampoline<>(SB)

TEXT libc_mach_ports_register_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_ports_register(SB)
GLOBL	·libc_mach_ports_register_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_ports_register_trampoline_addr(SB)/8, $libc_mach_ports_register_trampoline<>(SB)

TEXT libc_vm_deallocate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_vm_deallocate(SB)
GLOBL	·libc_vm_deallocate_trampoline_addr(SB), RODATA, $8
DATA	·libc_vm_deallocate_trampoline_addr(SB)/8, $libc_vm_deallocate_trampoline<>(SB)

TEXT libc_mach_port_deallocate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_port_deallocate(SB)
GLOBL	·libc_mach_port_deallocate_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_port_deallocate_trampoline_addr(SB)/8, $libc_mach_port_deallocate_trampoline<>(SB)

TEXT libc_task_get_special_port_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_task_get_special_port(SB)
GLOBL	·libc_task_get_special_port_trampoline_addr(SB), RODATA, $8
DATA	·libc_task_get_special_port_trampoline_addr(SB)/8, $libc_task_get_special_port_trampoline<>(SB)

TEXT libc_task_set_special_port_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_task_set_special_port(SB)
GLOBL	·libc_task_set_special_port_trampoline_addr(SB), RODATA, $8
DATA	·libc_task_set_special_port_trampoline_addr(SB)/8, $libc_task_set_special_port_trampoline<>(SB)

TEXT libc_task_get_exception_ports_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_task_get_exception_ports(SB)
GLOBL	·libc_task_get_exception_ports_trampoline_addr(SB), RODATA, $8
DATA	·libc_task_get_exception_ports_trampoline_addr(SB)/8, $libc_task_get_exception_ports_trampoline<>(SB)

TEXT libc_task_set_exception_ports_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_task_set_exception_ports(SB)
GLOBL	·libc_task_set_exception_ports_trampoline_addr(SB), RODATA, $8
DATA	·libc_task_set_exception_ports_trampoline_addr(SB)/8, $libc_task_set_exception_ports_trampoline<>(SB)

TEXT libc_mach_port_allocate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_port_allocate(SB)
GLOBL	·libc_mach_port_allocate_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_port_allocate_trampoline_addr(SB)/8, $libc_mach_port_allocate_trampoline<>(SB)

TEXT libc_mach_port_insert_right_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_port_insert_right(SB)
GLOBL	·libc_mach_port_insert_right_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_port_insert_right_trampoline_addr(SB)/8, $libc_mach_port_insert_right_trampoline<>(SB)
