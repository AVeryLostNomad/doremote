package platform

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mfTEnumEx calls MFTEnumEx. Its first parameter is a GUID passed by value; the x64 calling
// convention passes structs larger than 8 bytes as a pointer to a caller-owned copy.
func mfTEnumEx(category *windows.GUID, flags uint32, in, out *mfRegisterTypeInfo, acts ***mfActivate, n *uint32) int32 {
	c := *category
	r, _, _ := procMFTEnumEx.Call(uintptr(unsafe.Pointer(&c)), uintptr(flags), uintptr(unsafe.Pointer(in)),
		uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(acts)), uintptr(unsafe.Pointer(n)))
	return int32(uint32(r))
}

// mfCallKeyU64 calls a COM method taking (REFGUID key, UINT64 v), or just (UINT64 v) when key
// is nil. A 64-bit value fits one argument register.
func mfCallKeyU64(fn, this uintptr, key *windows.GUID, v uint64) int32 {
	if key == nil {
		return mfHR(syscall.SyscallN(fn, this, uintptr(v)))
	}
	return mfHR(syscall.SyscallN(fn, this, uintptr(unsafe.Pointer(key)), uintptr(v)))
}
