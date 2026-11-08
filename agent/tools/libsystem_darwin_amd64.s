//go:build darwin && amd64

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

TEXT libc_posix_spawn_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn(SB)
GLOBL	·libc_posix_spawn_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_trampoline_addr(SB)/8, $libc_posix_spawn_trampoline<>(SB)

TEXT libc_posix_spawnattr_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_init(SB)
GLOBL	·libc_posix_spawnattr_init_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_init_trampoline_addr(SB)/8, $libc_posix_spawnattr_init_trampoline<>(SB)

TEXT libc_posix_spawnattr_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_destroy(SB)
GLOBL	·libc_posix_spawnattr_destroy_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_destroy_trampoline_addr(SB)/8, $libc_posix_spawnattr_destroy_trampoline<>(SB)

TEXT libc_posix_spawnattr_setflags_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setflags(SB)
GLOBL	·libc_posix_spawnattr_setflags_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setflags_trampoline_addr(SB)/8, $libc_posix_spawnattr_setflags_trampoline<>(SB)

TEXT libc_posix_spawnattr_setpgroup_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setpgroup(SB)
GLOBL	·libc_posix_spawnattr_setpgroup_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setpgroup_trampoline_addr(SB)/8, $libc_posix_spawnattr_setpgroup_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigmask_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setsigmask(SB)
GLOBL	·libc_posix_spawnattr_setsigmask_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setsigmask_trampoline_addr(SB)/8, $libc_posix_spawnattr_setsigmask_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigdefault_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setsigdefault(SB)
GLOBL	·libc_posix_spawnattr_setsigdefault_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setsigdefault_trampoline_addr(SB)/8, $libc_posix_spawnattr_setsigdefault_trampoline<>(SB)

TEXT libc_posix_spawnattr_setspecialport_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setspecialport_np(SB)
GLOBL	·libc_posix_spawnattr_setspecialport_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setspecialport_np_trampoline_addr(SB)/8, $libc_posix_spawnattr_setspecialport_np_trampoline<>(SB)

TEXT libc_posix_spawnattr_setexceptionports_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setexceptionports_np(SB)
GLOBL	·libc_posix_spawnattr_setexceptionports_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setexceptionports_np_trampoline_addr(SB)/8, $libc_posix_spawnattr_setexceptionports_np_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_init(SB)
GLOBL	·libc_posix_spawn_file_actions_init_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_init_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_init_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_destroy(SB)
GLOBL	·libc_posix_spawn_file_actions_destroy_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_destroy_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_destroy_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_adddup2_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_adddup2(SB)
GLOBL	·libc_posix_spawn_file_actions_adddup2_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_adddup2_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_adddup2_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_addopen_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_addopen(SB)
GLOBL	·libc_posix_spawn_file_actions_addopen_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_addopen_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_addopen_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_addchdir_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_addchdir_np(SB)
GLOBL	·libc_posix_spawn_file_actions_addchdir_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_addchdir_np_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_addchdir_np_trampoline<>(SB)

TEXT libc_pthread_sigmask_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_pthread_sigmask(SB)
GLOBL	·libc_pthread_sigmask_trampoline_addr(SB), RODATA, $8
DATA	·libc_pthread_sigmask_trampoline_addr(SB)/8, $libc_pthread_sigmask_trampoline<>(SB)
