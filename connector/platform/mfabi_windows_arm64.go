package platform

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mfTEnumEx calls MFTEnumEx. Its first parameter is a GUID passed by value; AAPCS64 (which
// Windows on ARM64 follows) passes a 16-byte composite in two consecutive integer registers,
// holding its little-endian in-memory bytes.
func mfTEnumEx(category *windows.GUID, flags uint32, in, out *mfRegisterTypeInfo, acts ***mfActivate, n *uint32) int32 {
	g := category
	lo := uint64(g.Data1) | uint64(g.Data2)<<32 | uint64(g.Data3)<<48
	hi := binary.LittleEndian.Uint64(g.Data4[:])
	r, _, _ := procMFTEnumEx.Call(uintptr(lo), uintptr(hi), uintptr(flags), uintptr(unsafe.Pointer(in)),
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
