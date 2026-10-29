package platform

// Minimal Media Foundation bindings for the H.264 encoder, called through COM vtables (the
// Windows build has no cgo). Vtable slots and GUIDs are from the Windows SDK headers
// mftransform.h, mfobjects.h, mfidl.h, mfapi.h, codecapi.h, strmif.h and mferror.h.

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modMFPlat                       = windows.NewLazySystemDLL("mfplat.dll")
	procMFStartup                   = modMFPlat.NewProc("MFStartup")
	procMFShutdown                  = modMFPlat.NewProc("MFShutdown")
	procMFTEnumEx                   = modMFPlat.NewProc("MFTEnumEx")
	procMFCreateMediaType           = modMFPlat.NewProc("MFCreateMediaType")
	procMFCreateSample              = modMFPlat.NewProc("MFCreateSample")
	procMFCreateAlignedMemoryBuffer = modMFPlat.NewProc("MFCreateAlignedMemoryBuffer")
)

// mfVersion is MF_VERSION for Windows 7 and later (MF_SDK_VERSION 2, MF_API_VERSION 0x70).
const mfVersion = 0x00020070

const (
	mfStartupLite = 1 // MFSTARTUP_LITE: no socket support

	mftEnumFlagSyncMFT       = 0x01
	mftEnumFlagHardware      = 0x04
	mftEnumFlagSortAndFilter = 0x40

	mftMessageCommandFlush           = 0x00000000
	mftMessageCommandDrain           = 0x00000001
	mftMessageNotifyBeginStreaming   = 0x10000000
	mftMessageNotifyEndStreaming     = 0x10000001
	mftMessageNotifyStartOfStream    = 0x10000003
	mftOutputStreamProvidesSamples   = 0x100
	mftOutputStreamCanProvideSamples = 0x200

	mfEventError                  = 1   // MEError
	mfEventTransformNeedInput     = 601 // METransformNeedInput
	mfEventTransformHaveOutput    = 602 // METransformHaveOutput
	mfEventTransformDrainComplete = 603 // METransformDrainComplete

	mfVideoInterlaceProgressive = 2   // MFVideoInterlace_Progressive
	mfVideoPrimariesBT709       = 2   // MFVideoPrimaries_BT709
	mfVideoTransFunc709         = 5   // MFVideoTransFunc_709
	mfVideoTransferMatrixBT709  = 1   // MFVideoTransferMatrix_BT709
	mfNominalRange16_235        = 2   // MFNominalRange_16_235
	mfH264ProfileMain           = 77  // eAVEncH264VProfile_Main
	mfH264ProfileHigh           = 100 // eAVEncH264VProfile_High
	mfRateControlCBR            = 0   // eAVEncCommonRateControlMode_CBR
	mfScenarioDisplayRemoting   = 1   // eAVScenarioInfo_DisplayRemoting

	mfVTBool = 11 // VT_BOOL
	mfVTUI4  = 19 // VT_UI4
)

// HRESULTs from winerror.h and mferror.h, compared as uint32(hr).
const (
	mfENotImpl                = 0x80004001 // E_NOTIMPL
	mfENotAccepting           = 0xC00D36B5 // MF_E_NOTACCEPTING
	mfETransformStreamChange  = 0xC00D6D61 // MF_E_TRANSFORM_STREAM_CHANGE
	mfETransformNeedMoreInput = 0xC00D6D72 // MF_E_TRANSFORM_NEED_MORE_INPUT
)

func mfGUID(d1 uint32, d2, d3 uint16, d4 ...byte) windows.GUID {
	return windows.GUID{Data1: d1, Data2: d2, Data3: d3, Data4: [8]byte(d4)}
}

// mfMediaSubtype is DEFINE_MEDIATYPE_GUID: a FOURCC in the base video subtype GUID.
func mfMediaSubtype(fourCC uint32) windows.GUID {
	return mfGUID(fourCC, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71)
}

var (
	mfIIDTransform           = mfGUID(0xbf94c121, 0x5b05, 0x4e6f, 0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d)
	mfIIDMediaEventGenerator = mfGUID(0x2cd0bd52, 0xbcd5, 0x4b89, 0xb6, 0x2c, 0xea, 0xdc, 0x0c, 0x03, 0x1e, 0x7d)
	mfIIDShutdown            = mfGUID(0x97ec2ea4, 0x0e42, 0x4937, 0x97, 0xac, 0x9d, 0x6d, 0x32, 0x88, 0x24, 0xe1)
	mfIIDCodecAPI            = mfGUID(0x901db4c7, 0x31ce, 0x41a2, 0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda)

	mftCategoryVideoEncoder = mfGUID(0xf79eac7d, 0xe545, 0x4387, 0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a)
	mfMediaTypeVideo        = mfGUID(0x73646976, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71)
	mfVideoFormatNV12       = mfMediaSubtype('N' | 'V'<<8 | '1'<<16 | '2'<<24)
	mfVideoFormatH264       = mfMediaSubtype('H' | '2'<<8 | '6'<<16 | '4'<<24)

	mfMTMajorType          = mfGUID(0x48eba18e, 0xf8c9, 0x4687, 0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f)
	mfMTSubtype            = mfGUID(0xf7e34c9a, 0x42e8, 0x4714, 0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5)
	mfMTFrameSize          = mfGUID(0x1652c33d, 0xd6b2, 0x4012, 0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d)
	mfMTFrameRate          = mfGUID(0xc459a2e8, 0x3d2c, 0x4e44, 0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0)
	mfMTPixelAspectRatio   = mfGUID(0xc6376a1e, 0x8d0a, 0x4027, 0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6)
	mfMTInterlaceMode      = mfGUID(0xe2724bb8, 0xe676, 0x4806, 0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd)
	mfMTTransferFunction   = mfGUID(0x5fb0fce9, 0xbe5c, 0x4935, 0xa8, 0x11, 0xec, 0x83, 0x8f, 0x8e, 0xed, 0x93)
	mfMTVideoPrimaries     = mfGUID(0xdbfbe4d7, 0x0740, 0x4ee0, 0x81, 0x92, 0x85, 0x0a, 0xb0, 0xe2, 0x19, 0x35)
	mfMTYUVMatrix          = mfGUID(0x3e23d450, 0x2c75, 0x4d25, 0xa0, 0x0e, 0xb9, 0x16, 0x70, 0xd1, 0x23, 0x27)
	mfMTVideoNominalRange  = mfGUID(0xc21b8ee5, 0xb956, 0x4071, 0x8d, 0xaf, 0x32, 0x5e, 0xdf, 0x5c, 0xab, 0x11)
	mfMTAvgBitrate         = mfGUID(0x20332624, 0xfb0d, 0x4d9e, 0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e)
	mfMTDefaultStride      = mfGUID(0x644b4e48, 0x1e02, 0x4516, 0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6)
	mfMTMPEG2Profile       = mfGUID(0xad76a80b, 0x2d5c, 0x4e0b, 0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36)
	mfMTMPEGSequenceHeader = mfGUID(0x3c036de7, 0x3ad0, 0x4c9e, 0x92, 0x16, 0xee, 0x6d, 0x6a, 0xc2, 0x1c, 0xb3)

	mfTransformAsync         = mfGUID(0xf81a699a, 0x649a, 0x497d, 0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a)
	mfTransformAsyncUnlock   = mfGUID(0xe5666d6b, 0x3422, 0x4eb6, 0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07)
	mftFriendlyNameAttribute = mfGUID(0x314ffbae, 0x5b41, 0x4c95, 0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3)

	mfCodecAPILowLatencyMode          = mfGUID(0x9c27891a, 0xed7a, 0x40e1, 0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee)
	mfCodecAPIScenarioInfo            = mfGUID(0xb28a6e64, 0x3ff9, 0x446a, 0x8a, 0x4b, 0x0d, 0x7a, 0x53, 0x41, 0x32, 0x36)
	mfCodecAPIRateControlMode         = mfGUID(0x1c0608e9, 0x370c, 0x4710, 0x8a, 0x58, 0xcb, 0x61, 0x81, 0xc4, 0x24, 0x23)
	mfCodecAPIMeanBitRate             = mfGUID(0xf7222374, 0x2144, 0x4815, 0xb5, 0x50, 0xa3, 0x7f, 0x8e, 0x12, 0xee, 0x52)
	mfCodecAPIMPVDefaultBPictureCount = mfGUID(0x8d390aac, 0xdc5c, 0x4200, 0xb5, 0x7f, 0x81, 0x4d, 0x04, 0xba, 0xba, 0xb2)
	mfCodecAPIMPVGOPSize              = mfGUID(0x95f31b26, 0x95a4, 0x41aa, 0x93, 0x03, 0x24, 0x6a, 0x7f, 0xc6, 0xee, 0xf1)
	mfCodecAPIH264CABACEnable         = mfGUID(0xee6cad62, 0xd305, 0x4248, 0xa5, 0x0e, 0xe1, 0xb2, 0x55, 0xf7, 0xca, 0xf8)
	mfCodecAPIVideoForceKeyFrame      = mfGUID(0x398c1b98, 0x8353, 0x475a, 0x9e, 0xf2, 0x8f, 0x26, 0x5d, 0x26, 0x03, 0x45)
)

// mfRegisterTypeInfo is MFT_REGISTER_TYPE_INFO.
type mfRegisterTypeInfo struct{ major, subtype windows.GUID }

// mfInputStreamInfo is MFT_INPUT_STREAM_INFO.
type mfInputStreamInfo struct {
	maxLatency                           int64
	flags, size, maxLookahead, alignment uint32
}

// mfOutputStreamInfo is MFT_OUTPUT_STREAM_INFO.
type mfOutputStreamInfo struct{ flags, size, alignment uint32 }

// mfOutputDataBuffer is MFT_OUTPUT_DATA_BUFFER.
type mfOutputDataBuffer struct {
	streamID uint32
	sample   *mfSample
	status   uint32
	events   *mfObj // IMFCollection
}

// mfVariant is a VARIANT holding a scalar: vt, three reserved WORDs, then the value union at
// offset 8. It is sized for the 24-byte 64-bit VARIANT (16 bytes on 386).
type mfVariant struct {
	vt  uint16
	_   [3]uint16
	val uint64
	_   uint64
}

type mfError struct {
	op string
	hr int32
}

func (e mfError) Error() string { return fmt.Sprintf("%s failed: HRESULT 0x%08X", e.op, uint32(e.hr)) }

func mfCheck(op string, hr int32) error {
	if hr >= 0 {
		return nil
	}
	return mfError{op, hr}
}

// mfHR takes syscall.SyscallN's results and returns the HRESULT.
func mfHR(r, _ uintptr, _ syscall.Errno) int32 { return int32(uint32(r)) }

// mfObj is a COM interface pointer: the object's first word points at its vtable. Each MF
// interface type embeds it (at offset 0, so the interface pointer is the struct's address).
// Every method converts its pointer arguments inside the SyscallN call expression so the Go
// referents stay alive and unmoved for the call.
type mfObj struct{ vtbl *[48]uintptr }

func (o *mfObj) this() uintptr { return uintptr(unsafe.Pointer(o)) }

// release calls IUnknown::Release.
func (o *mfObj) release() { syscall.SyscallN(o.vtbl[2], o.this()) }

// query calls IUnknown::QueryInterface; out must point to an interface pointer variable.
func (o *mfObj) query(iid *windows.GUID, out unsafe.Pointer) int32 {
	return mfHR(syscall.SyscallN(o.vtbl[0], o.this(), uintptr(unsafe.Pointer(iid)), uintptr(out)))
}

// mfAttributes is IMFAttributes (also the base of IMFMediaType, IMFSample, IMFActivate and
// IMFMediaEvent).
type mfAttributes struct{ mfObj }

func (a *mfAttributes) getUINT32(key *windows.GUID) (uint32, int32) {
	var v uint32
	hr := mfHR(syscall.SyscallN(a.vtbl[7], a.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v))))
	return v, hr
}

// getAllocatedString returns the string value of key, freeing the COM allocation.
func (a *mfAttributes) getAllocatedString(key *windows.GUID) (string, int32) {
	var p *uint16
	var n uint32
	hr := mfHR(syscall.SyscallN(a.vtbl[13], a.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n))))
	if hr < 0 {
		return "", hr
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), hr
}

// getAllocatedBlob returns a copy of the blob value of key, freeing the COM allocation.
func (a *mfAttributes) getAllocatedBlob(key *windows.GUID) ([]byte, int32) {
	var p *byte
	var n uint32
	hr := mfHR(syscall.SyscallN(a.vtbl[16], a.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n))))
	if hr < 0 {
		return nil, hr
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return append([]byte(nil), unsafe.Slice(p, n)...), hr
}

func (a *mfAttributes) setUINT32(key *windows.GUID, v uint32) int32 {
	return mfHR(syscall.SyscallN(a.vtbl[21], a.this(), uintptr(unsafe.Pointer(key)), uintptr(v)))
}

func (a *mfAttributes) setUINT64(key *windows.GUID, v uint64) int32 {
	return mfCallKeyU64(a.vtbl[22], a.this(), key, v)
}

func (a *mfAttributes) setGUID(key, v *windows.GUID) int32 {
	return mfHR(syscall.SyscallN(a.vtbl[24], a.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(v))))
}

func (a *mfAttributes) getGUID(key *windows.GUID) (windows.GUID, int32) {
	var v windows.GUID
	hr := mfHR(syscall.SyscallN(a.vtbl[10], a.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v))))
	return v, hr
}

// mfActivate is IMFActivate.
type mfActivate struct{ mfAttributes }

func (a *mfActivate) activateObject(iid *windows.GUID, out unsafe.Pointer) int32 {
	return mfHR(syscall.SyscallN(a.vtbl[33], a.this(), uintptr(unsafe.Pointer(iid)), uintptr(out)))
}

func (a *mfActivate) shutdownObject() int32 { return mfHR(syscall.SyscallN(a.vtbl[34], a.this())) }

// mfMediaType is IMFMediaType.
type mfMediaType struct{ mfAttributes }

// mfMediaEvent is IMFMediaEvent.
type mfMediaEvent struct{ mfAttributes }

func (e *mfMediaEvent) getType() (uint32, int32) {
	var t uint32
	hr := mfHR(syscall.SyscallN(e.vtbl[33], e.this(), uintptr(unsafe.Pointer(&t))))
	return t, hr
}

func (e *mfMediaEvent) getStatus() (int32, int32) {
	var s int32
	hr := mfHR(syscall.SyscallN(e.vtbl[35], e.this(), uintptr(unsafe.Pointer(&s))))
	return s, hr
}

// mfSample is IMFSample.
type mfSample struct{ mfAttributes }

func (s *mfSample) setSampleTime(t int64) int32 {
	return mfCallKeyU64(s.vtbl[36], s.this(), nil, uint64(t))
}

func (s *mfSample) setSampleDuration(d int64) int32 {
	return mfCallKeyU64(s.vtbl[38], s.this(), nil, uint64(d))
}

func (s *mfSample) convertToContiguousBuffer() (*mfMediaBuffer, int32) {
	var b *mfMediaBuffer
	hr := mfHR(syscall.SyscallN(s.vtbl[41], s.this(), uintptr(unsafe.Pointer(&b))))
	return b, hr
}

func (s *mfSample) addBuffer(b *mfMediaBuffer) int32 {
	return mfHR(syscall.SyscallN(s.vtbl[42], s.this(), b.this()))
}

// mfMediaBuffer is IMFMediaBuffer.
type mfMediaBuffer struct{ mfObj }

// lock returns the buffer's memory (its maximum length) and current data length.
func (b *mfMediaBuffer) lock() (mem []byte, cur int, hr int32) {
	var p *byte
	var maxLen, curLen uint32
	hr = mfHR(syscall.SyscallN(b.vtbl[3], b.this(), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&curLen))))
	if hr < 0 {
		return nil, 0, hr
	}
	return unsafe.Slice(p, maxLen), int(curLen), hr
}

func (b *mfMediaBuffer) unlock() int32 { return mfHR(syscall.SyscallN(b.vtbl[4], b.this())) }

func (b *mfMediaBuffer) setCurrentLength(n int) int32 {
	return mfHR(syscall.SyscallN(b.vtbl[6], b.this(), uintptr(uint32(n))))
}

// mfTransform is IMFTransform.
type mfTransform struct{ mfObj }

func (t *mfTransform) getStreamIDs(in, out *uint32) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[5], t.this(), 1, uintptr(unsafe.Pointer(in)), 1, uintptr(unsafe.Pointer(out))))
}

func (t *mfTransform) getInputStreamInfo(id uint32, info *mfInputStreamInfo) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[6], t.this(), uintptr(id), uintptr(unsafe.Pointer(info))))
}

func (t *mfTransform) getOutputStreamInfo(id uint32, info *mfOutputStreamInfo) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[7], t.this(), uintptr(id), uintptr(unsafe.Pointer(info))))
}

func (t *mfTransform) getAttributes() (*mfAttributes, int32) {
	var a *mfAttributes
	hr := mfHR(syscall.SyscallN(t.vtbl[8], t.this(), uintptr(unsafe.Pointer(&a))))
	return a, hr
}

func (t *mfTransform) getInputAvailableType(id, index uint32) (*mfMediaType, int32) {
	var m *mfMediaType
	hr := mfHR(syscall.SyscallN(t.vtbl[13], t.this(), uintptr(id), uintptr(index), uintptr(unsafe.Pointer(&m))))
	return m, hr
}

func (t *mfTransform) setInputType(id uint32, m *mfMediaType) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[15], t.this(), uintptr(id), m.this(), 0))
}

func (t *mfTransform) setOutputType(id uint32, m *mfMediaType) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[16], t.this(), uintptr(id), m.this(), 0))
}

func (t *mfTransform) getOutputCurrentType(id uint32) (*mfMediaType, int32) {
	var m *mfMediaType
	hr := mfHR(syscall.SyscallN(t.vtbl[18], t.this(), uintptr(id), uintptr(unsafe.Pointer(&m))))
	return m, hr
}

func (t *mfTransform) getOutputAvailableType(id, index uint32) (*mfMediaType, int32) {
	var m *mfMediaType
	hr := mfHR(syscall.SyscallN(t.vtbl[14], t.this(), uintptr(id), uintptr(index), uintptr(unsafe.Pointer(&m))))
	return m, hr
}

func (t *mfTransform) processMessage(msg uint32) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[23], t.this(), uintptr(msg), 0))
}

func (t *mfTransform) processInput(id uint32, s *mfSample) int32 {
	return mfHR(syscall.SyscallN(t.vtbl[24], t.this(), uintptr(id), s.this(), 0))
}

func (t *mfTransform) processOutput(buf *mfOutputDataBuffer) int32 {
	var status uint32
	return mfHR(syscall.SyscallN(t.vtbl[25], t.this(), 0, 1, uintptr(unsafe.Pointer(buf)), uintptr(unsafe.Pointer(&status))))
}

// mfEventGenerator is IMFMediaEventGenerator.
type mfEventGenerator struct{ mfObj }

// getEvent blocks until the next event (dwFlags 0).
func (g *mfEventGenerator) getEvent() (*mfMediaEvent, int32) {
	var e *mfMediaEvent
	hr := mfHR(syscall.SyscallN(g.vtbl[3], g.this(), 0, uintptr(unsafe.Pointer(&e))))
	return e, hr
}

// queueEvent queues an event of type met with GUID_NULL extended type, S_OK and no value.
func (g *mfEventGenerator) queueEvent(met uint32) int32 {
	var null windows.GUID
	return mfHR(syscall.SyscallN(g.vtbl[6], g.this(), uintptr(met), uintptr(unsafe.Pointer(&null)), 0, 0))
}

// mfShutdown is IMFShutdown.
type mfShutdown struct{ mfObj }

func (s *mfShutdown) shutdown() int32 { return mfHR(syscall.SyscallN(s.vtbl[3], s.this())) }

// mfCodecAPI is ICodecAPI.
type mfCodecAPI struct{ mfObj }

func (c *mfCodecAPI) setUI4(key *windows.GUID, v uint32) int32 {
	val := mfVariant{vt: mfVTUI4, val: uint64(v)}
	return mfHR(syscall.SyscallN(c.vtbl[9], c.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&val))))
}

func (c *mfCodecAPI) setBool(key *windows.GUID, v bool) int32 {
	val := mfVariant{vt: mfVTBool} // VARIANT_FALSE
	if v {
		val.val = 0xffff // VARIANT_TRUE
	}
	return mfHR(syscall.SyscallN(c.vtbl[9], c.this(), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&val))))
}

func mfCreateMediaType() (*mfMediaType, error) {
	var m *mfMediaType
	r, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&m)))
	return m, mfCheck("MFCreateMediaType", int32(uint32(r)))
}

func mfCreateSample() (*mfSample, error) {
	var s *mfSample
	r, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&s)))
	return s, mfCheck("MFCreateSample", int32(uint32(r)))
}

// mfCreateBuffer allocates a system-memory buffer of size bytes aligned to align (a power of
// two, or 0 for the default).
func mfCreateBuffer(size, align int) (*mfMediaBuffer, error) {
	var b *mfMediaBuffer
	r, _, _ := procMFCreateAlignedMemoryBuffer.Call(uintptr(uint32(size)), uintptr(uint32(max(align, 1)-1)), uintptr(unsafe.Pointer(&b)))
	return b, mfCheck("MFCreateAlignedMemoryBuffer", int32(uint32(r)))
}

// mfCreateSampleWithBuffer creates a sample holding one new buffer of size bytes.
func mfCreateSampleWithBuffer(size, align int) (*mfSample, *mfMediaBuffer, error) {
	s, err := mfCreateSample()
	if err != nil {
		return nil, nil, err
	}
	b, err := mfCreateBuffer(size, align)
	if err != nil {
		s.release()
		return nil, nil, err
	}
	if err := mfCheck("IMFSample::AddBuffer", s.addBuffer(b)); err != nil {
		b.release()
		s.release()
		return nil, nil, err
	}
	return s, b, nil
}

// mfEnumActivates calls MFTEnumEx for category with flags and returns the activation objects.
func mfEnumActivates(category *windows.GUID, flags uint32, in, out *mfRegisterTypeInfo) ([]*mfActivate, error) {
	var arr **mfActivate
	var n uint32
	if err := mfCheck("MFTEnumEx", mfTEnumEx(category, flags, in, out, &arr, &n)); err != nil {
		return nil, err
	}
	if arr == nil {
		return nil, nil
	}
	acts := append([]*mfActivate(nil), unsafe.Slice(arr, n)...)
	windows.CoTaskMemFree(unsafe.Pointer(arr))
	return acts, nil
}
