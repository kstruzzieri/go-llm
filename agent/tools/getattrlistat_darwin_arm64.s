//go:build darwin && arm64

#include "textflag.h"

TEXT libc_getattrlistat_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getattrlistat(SB)
GLOBL	·libc_getattrlistat_trampoline_addr(SB), RODATA, $8
DATA	·libc_getattrlistat_trampoline_addr(SB)/8, $libc_getattrlistat_trampoline<>(SB)
