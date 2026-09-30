//go:build windows

package cli

import (
	"errors"
	"syscall"
	"unsafe"
)

// sysctlByName has no meaning on Windows, and the memory question still has to
// be answered somewhere: `caf doctor` reports "this machine did not report its
// memory" when a read fails, and on a platform with no sysctl at all that is what
// it would always say — which is a false alarm about every Windows machine.
//
// So the two names are split. sysctlByName is the unixes' accessor and refuses
// here; totalMemory is the question, and it is answered through the API the
// platform has always had for it.
func sysctlByName(string) (uint64, error) {
	return 0, errors.New("windows has no sysctl; caf doctor reads memory through GlobalMemoryStatusEx")
}

// memoryStatusEx is what GlobalMemoryStatusEx fills in. Only the field caf reads
// is named, and the rest are the padding the call requires: a structure shorter
// than the API expects is a call that writes past the end of it.
type memoryStatusEx struct {
	Length            uint32
	MemoryLoad        uint32
	TotalPhys         uint64
	AvailablePhys     uint64
	TotalPageFile     uint64
	AvailablePageFile uint64
	TotalVirtual      uint64
	AvailableVirtual  uint64
	AvailableExtended uint64
}

// totalMemoryWindows asks the kernel for the machine's physical memory.
//
// This is the one place in caf that reaches for `syscall` rather than the
// standard library's own wrappers, and the reason is that the standard library
// has no cross-platform way to ask: the number lives behind three different
// sysctls on the unixes and behind one Win32 call here, and a fourth dependency
// to read one number in a report is not a trade AGENTS.md lets this package
// make.
func totalMemoryWindows() (uint64, error) {
	globalMemoryStatusEx := syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

	status := memoryStatusEx{}
	status.Length = uint32(unsafe.Sizeof(status))
	ok, _, err := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		if err == nil {
			err = errors.New("GlobalMemoryStatusEx reported no status")
		}
		return 0, err
	}
	return status.TotalPhys, nil
}

// totalMemoryBytes is the memory question in this platform's spelling.
func totalMemoryBytes() (uint64, error) { return totalMemoryWindows() }
