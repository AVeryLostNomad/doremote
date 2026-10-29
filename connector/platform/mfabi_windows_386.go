package platform

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mfTEnumEx calls MFTEnumEx. Its first parameter is a GUID passed by value, which stdcall
// pushes onto the stack as four 32-bit words.
func mfTEnumEx(category *windows.GUID, flags uint32, in, out *mfRegisterTypeInfo, acts ***mfActivate, n *uint32) int32 {
	words := (*[4]uint32)(unsafe.Pointer(category))
	r, _, _ := procMFTEnumEx.Call(uintptr(words[0]), uintptr(words[1]), uintptr(words[2]), uintptr(words[3]),
		uintptr(flags), uintptr(unsafe.Pointer(in)), uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(acts)),
		uintptr(unsafe.Pointer(n)))
	return int32(uint32(r))
}

// mfCallKeyU64 calls a COM method taking (REFGUID key, UINT64 v), or just (UINT64 v) when key
// is nil. A 64-bit value takes two stack words, low word first.
func mfCallKeyU64(fn, this uintptr, key *windows.GUID, v uint64) int32 {
	if key == nil {
		return mfHR(syscall.SyscallN(fn, this, uintptr(uint32(v)), uintptr(v>>32)))
	}
	return mfHR(syscall.SyscallN(fn, this, uintptr(unsafe.Pointer(key)), uintptr(uint32(v)), uintptr(v>>32)))
}
