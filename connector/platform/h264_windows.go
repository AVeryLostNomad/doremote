package platform

import (
	"errors"
	"fmt"
	"log"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// h264DrainAfter is how long an async encoder may sit on a frame before it is told to
	// drain; one that needs this is drained after every frame from then on.
	h264DrainAfter = time.Second
	// h264EventTimeout bounds every other wait for the encoder.
	h264EventTimeout = 10 * time.Second
)

// h264Encoder encodes BGRA desktop images to H.264 with Media Foundation (hardware MFT
// preferred, Microsoft software MFT fallback). All Media Foundation calls run on one
// OS-locked COM (MTA) goroutine; encode hands each frame to it and waits for the result.
type h264Encoder struct {
	w, h    int
	label   string
	bitrate atomic.Uint32

	mu      sync.Mutex // serialises encode and close
	closed  bool
	reqs    chan h264Request
	results chan h264Result
	done    chan struct{} // closed when the COM goroutine has torn everything down
}

type h264Request struct {
	bgra     []byte
	stride   int
	forceKey bool
}

type h264Result struct {
	au  []byte
	key bool
	err error
}

func newH264Encoder(width, height, fps, bitrate int) (*h264Encoder, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("h264: frame size %dx%d is not positive and even", width, height)
	}
	fps = max(fps, 1)
	if bitrate <= 0 {
		bitrate = width * height * fps / 20
	}
	e := &h264Encoder{
		w: width, h: height,
		reqs:    make(chan h264Request),
		results: make(chan h264Result),
		done:    make(chan struct{}),
	}
	e.setBitrate(bitrate)
	ready := make(chan error, 1)
	go e.run(fps, ready)
	if err := <-ready; err != nil {
		<-e.done
		return nil, err
	}
	return e, nil
}

// encode converts bgra (top-down, stride bytes per row) to NV12 (BT.709 limited range) and
// encodes it. It returns one Annex B access unit, valid until the next encode call; keyframes
// are IDR with SPS/PPS in-band. forceKey forces an IDR; the first call always returns one.
func (e *h264Encoder) encode(bgra []byte, stride int, forceKey bool) (au []byte, key bool, err error) {
	if stride < e.w*4 || len(bgra) < (e.h-1)*stride+e.w*4 {
		return nil, false, fmt.Errorf("h264: %d-byte image with stride %d is too small for %dx%d", len(bgra), stride, e.w, e.h)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, false, errors.New("h264: encoder closed")
	}
	e.reqs <- h264Request{bgra, stride, forceKey}
	r := <-e.results
	return r.au, r.key, r.err
}

// setBitrate changes the target average bitrate (bits/s) from the next frame on. It is safe
// to call from any goroutine.
func (e *h264Encoder) setBitrate(bps int) {
	if bps > 0 {
		e.bitrate.Store(uint32(min(uint64(bps), math.MaxUint32)))
	}
}

// name describes the encoder MFT in use, for logs.
func (e *h264Encoder) name() string { return e.label }

func (e *h264Encoder) close() {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.reqs)
	}
	e.mu.Unlock()
	<-e.done
}

func (e *h264Encoder) run(fps int, ready chan<- error) {
	defer close(e.done)
	// Never unlocked: the thread exits with this goroutine instead of carrying COM state back
	// into Go's thread pool.
	runtime.LockOSThread()
	if err := mfCoInitialize(); err != nil {
		ready <- err
		return
	}
	defer windows.CoUninitialize()
	if err := mfStartup(); err != nil {
		ready <- err
		return
	}
	defer procMFShutdown.Call()
	conv := newNV12Converter(runtime.GOMAXPROCS(0))
	defer conv.close()
	m, err := openH264MFT(e.w, e.h, fps, e.bitrate.Load(), conv)
	if err != nil {
		ready <- err
		return
	}
	defer m.close()
	e.label = m.name
	ready <- nil

	needKey := true
	for req := range e.reqs {
		au, key, err := m.encode(req.bgra, req.stride, req.forceKey || needKey, e.bitrate.Load())
		if err == nil {
			needKey = false
		}
		e.results <- h264Result{au, key, err}
	}
}

func mfCoInitialize() error {
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && err != syscall.Errno(1) { // S_FALSE: already initialised
		return fmt.Errorf("h264: CoInitializeEx: %w", err)
	}
	return nil
}

func mfStartup() error {
	if err := modMFPlat.Load(); err != nil {
		return fmt.Errorf("h264: Media Foundation is not installed (Windows N/KN editions need the Media Feature Pack, Windows Server the Media Foundation feature): %w", err)
	}
	for _, p := range []*windows.LazyProc{procMFStartup, procMFShutdown, procMFTEnumEx, procMFCreateMediaType, procMFCreateSample, procMFCreateAlignedMemoryBuffer} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("h264: Media Foundation is incomplete: %w", err)
		}
	}
	r, _, _ := procMFStartup.Call(mfVersion, mfStartupLite)
	return mfCheck("MFStartup", int32(uint32(r)))
}

// h264MFT is one configured encoder transform. Only the COM goroutine uses it, apart from the
// event pump of an async (hardware) MFT.
type h264MFT struct {
	name  string
	async bool
	act   *mfActivate
	tf    *mfTransform
	codec *mfCodecAPI
	gen   *mfEventGenerator // async only
	shut  *mfShutdown       // async only, if implemented

	events   chan h264Event // async: events read by the pump
	pumpDone chan struct{}
	stopping atomic.Bool
	timer    *time.Timer
	// needInput counts METransformNeedInput events not yet answered with ProcessInput.
	needInput int
	// drainEachFrame is set once the encoder held a frame back despite low-latency mode.
	drainEachFrame bool

	inID, outID uint32
	w, h        int
	frameDur    int64 // 100 ns units
	start       time.Time
	lastTime    int64
	bitrate     uint32
	in          [2]h264InputSample // alternated so the MFT may keep a reference to the previous one
	inNext      int
	outSample   *mfSample // when the MFT doesn't allocate output samples
	outBuf      *mfMediaBuffer
	conv        *nv12Converter

	raw       []byte // this frame's encoder output, Annex B
	au        h264AccessUnit
	paramSets []byte // MF_MT_MPEG_SEQUENCE_HEADER as Annex B, if the encoder provides it
}

type h264InputSample struct {
	s *mfSample
	b *mfMediaBuffer
}

type h264Event struct {
	typ    uint32
	status int32
}

// openH264MFT tries the hardware H.264 encoders, then the software ones, and returns the first
// that configures and encodes a test frame.
func openH264MFT(w, h, fps int, bitrate uint32, conv *nv12Converter) (*h264MFT, error) {
	in := mfRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatNV12}
	out := mfRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatH264}
	var failures []string
	for _, hw := range []bool{true, false} {
		kind, flags := "software", uint32(mftEnumFlagSyncMFT|mftEnumFlagSortAndFilter)
		if hw {
			kind, flags = "hardware", mftEnumFlagHardware|mftEnumFlagSortAndFilter
		}
		acts, err := mfEnumActivates(&mftCategoryVideoEncoder, flags, &in, &out)
		if err != nil {
			failures = append(failures, kind+" encoder enumeration: "+err.Error())
			continue
		}
		var m *h264MFT
		for _, act := range acts {
			if m != nil {
				act.release()
				continue
			}
			name, _ := act.getAllocatedString(&mftFriendlyNameAttribute)
			if name == "" {
				name = "unnamed H.264 encoder MFT"
			}
			m = &h264MFT{name: name + " (" + kind + ")", act: act, w: w, h: h, conv: conv}
			if err := m.init(fps, bitrate); err != nil {
				log.Printf("h264: %s unusable: %v", m.name, err)
				failures = append(failures, m.name+": "+err.Error())
				m.close()
				m = nil
			}
		}
		if m != nil {
			return m, nil
		}
	}
	if len(failures) == 0 {
		return nil, errors.New("h264: Media Foundation has no H.264 encoder (Windows N/KN editions need the Media Feature Pack, Windows Server the Media Foundation feature)")
	}
	return nil, fmt.Errorf("h264: no usable H.264 encoder: %s", strings.Join(failures, "; "))
}

func (m *h264MFT) init(fps int, bitrate uint32) error {
	if err := mfCheck("IMFActivate::ActivateObject", m.act.activateObject(&mfIIDTransform, unsafe.Pointer(&m.tf))); err != nil {
		return err
	}
	// Async MFTs reject every call until unlocked.
	if attrs, hr := m.tf.getAttributes(); hr >= 0 && attrs != nil {
		if v, hr := attrs.getUINT32(&mfTransformAsync); hr >= 0 && v != 0 {
			m.async = true
			hr = attrs.setUINT32(&mfTransformAsyncUnlock, 1)
			if err := mfCheck("set MF_TRANSFORM_ASYNC_UNLOCK", hr); err != nil {
				attrs.release()
				return err
			}
		}
		attrs.release()
	}
	if err := mfCheck("QueryInterface(ICodecAPI)", m.tf.query(&mfIIDCodecAPI, unsafe.Pointer(&m.codec))); err != nil {
		return err
	}
	if hr := m.tf.getStreamIDs(&m.inID, &m.outID); hr < 0 {
		m.inID, m.outID = 0, 0 // E_NOTIMPL: fixed streams numbered from 0
	}

	// Encoders take some properties only before the output type is set (the Microsoft encoder:
	// rate control mode, B-frame count) and others only after, so anything rejected now is
	// retried once the types are set. None is fatal: the test frame proves the encoder works.
	props := []struct {
		name string
		set  func() int32
	}{
		{"low latency", func() int32 { return m.codec.setBool(&mfCodecAPILowLatencyMode, true) }},
		{"display-remoting scenario", func() int32 { return m.codec.setUI4(&mfCodecAPIScenarioInfo, mfScenarioDisplayRemoting) }},
		{"CBR rate control", func() int32 { return m.codec.setUI4(&mfCodecAPIRateControlMode, mfRateControlCBR) }},
		{"mean bitrate", func() int32 { return m.codec.setUI4(&mfCodecAPIMeanBitRate, bitrate) }},
		{"no B-frames", func() int32 { return m.codec.setUI4(&mfCodecAPIMPVDefaultBPictureCount, 0) }},
		{"CABAC", func() int32 { return m.codec.setBool(&mfCodecAPIH264CABACEnable, true) }},
		{"GOP size", func() int32 {
			// As long as the encoder allows: keyframes are only sent on request. MaxInt32
			// rather than MaxUint32 so encoders that hold the GOP in an int stay sane.
			var hr int32
			for _, n := range []uint32{math.MaxInt32, math.MaxUint16} {
				if hr = m.codec.setUI4(&mfCodecAPIMPVGOPSize, n); hr >= 0 {
					break
				}
			}
			return hr
		}},
	}
	failed := make([]bool, len(props))
	for i, p := range props {
		failed[i] = p.set() < 0
	}

	var err error
	for _, profile := range []uint32{mfH264ProfileHigh, mfH264ProfileMain} {
		if err = m.setOutputType(fps, bitrate, profile); err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	if err := m.setInputType(fps); err != nil {
		return err
	}
	var ignored []string
	for i, p := range props {
		if !failed[i] {
			continue
		}
		if hr := p.set(); hr < 0 {
			ignored = append(ignored, fmt.Sprintf("%s (0x%08X)", p.name, uint32(hr)))
		}
	}
	if len(ignored) > 0 {
		log.Printf("h264: %s rejected: %s", m.name, strings.Join(ignored, ", "))
	}
	m.bitrate = bitrate
	m.frameDur = 10_000_000 / int64(fps)
	m.fetchParamSets()

	var inInfo mfInputStreamInfo
	if err := mfCheck("IMFTransform::GetInputStreamInfo", m.tf.getInputStreamInfo(m.inID, &inInfo)); err != nil {
		return err
	}
	frameSize := m.w * m.h * 3 / 2
	for i := range m.in {
		s, b, err := mfCreateSampleWithBuffer(max(frameSize, int(inInfo.size)), max(int(inInfo.alignment), 16))
		if err != nil {
			return err
		}
		m.in[i] = h264InputSample{s, b}
	}
	var outInfo mfOutputStreamInfo
	if err := mfCheck("IMFTransform::GetOutputStreamInfo", m.tf.getOutputStreamInfo(m.outID, &outInfo)); err != nil {
		return err
	}
	if outInfo.flags&(mftOutputStreamProvidesSamples|mftOutputStreamCanProvideSamples) == 0 {
		// An encoded picture is practically never larger than the raw one.
		if m.outSample, m.outBuf, err = mfCreateSampleWithBuffer(max(frameSize, int(outInfo.size)), int(outInfo.alignment)); err != nil {
			return err
		}
	}

	if m.async {
		if err := mfCheck("QueryInterface(IMFMediaEventGenerator)", m.tf.query(&mfIIDMediaEventGenerator, unsafe.Pointer(&m.gen))); err != nil {
			return err
		}
		if m.tf.query(&mfIIDShutdown, unsafe.Pointer(&m.shut)) < 0 {
			m.shut = nil
		}
		m.events = make(chan h264Event, 16)
		m.pumpDone = make(chan struct{})
		m.timer = time.NewTimer(time.Hour)
		m.timer.Stop()
		go m.pump()
	}
	if err := mfCheck("MFT_MESSAGE_NOTIFY_BEGIN_STREAMING", m.tf.processMessage(mftMessageNotifyBeginStreaming)); err != nil {
		return err
	}
	if err := mfCheck("MFT_MESSAGE_NOTIFY_START_OF_STREAM", m.tf.processMessage(mftMessageNotifyStartOfStream)); err != nil {
		return err
	}
	m.start = time.Now()

	// Some encoders accept everything above and only fail (or stall) on the first frame;
	// catch that here so the next candidate gets a chance.
	if _, _, err := m.encode(nil, 0, true, bitrate); err != nil {
		return fmt.Errorf("test frame: %w", err)
	}
	return nil
}

// setVideoFormat sets the frame size, rate, square pixels, progressive scan and BT.709
// limited-range colour (which encoders write into the SPS VUI) on t.
func (m *h264MFT) setVideoFormat(t *mfMediaType, fps int) error {
	for _, hr := range []int32{
		t.setUINT64(&mfMTFrameSize, uint64(m.w)<<32|uint64(m.h)),
		t.setUINT64(&mfMTFrameRate, uint64(fps)<<32|1),
		t.setUINT64(&mfMTPixelAspectRatio, 1<<32|1),
		t.setUINT32(&mfMTInterlaceMode, mfVideoInterlaceProgressive),
		t.setUINT32(&mfMTVideoPrimaries, mfVideoPrimariesBT709),
		t.setUINT32(&mfMTTransferFunction, mfVideoTransFunc709),
		t.setUINT32(&mfMTYUVMatrix, mfVideoTransferMatrixBT709),
		t.setUINT32(&mfMTVideoNominalRange, mfNominalRange16_235),
	} {
		if hr < 0 {
			return mfCheck("IMFMediaType::Set", hr)
		}
	}
	return nil
}

func (m *h264MFT) setOutputType(fps int, bitrate, profile uint32) error {
	t, err := mfCreateMediaType()
	if err != nil {
		return err
	}
	defer t.release()
	for _, hr := range []int32{
		t.setGUID(&mfMTMajorType, &mfMediaTypeVideo),
		t.setGUID(&mfMTSubtype, &mfVideoFormatH264),
		t.setUINT32(&mfMTAvgBitrate, bitrate),
		t.setUINT32(&mfMTMPEG2Profile, profile),
	} {
		if hr < 0 {
			return mfCheck("IMFMediaType::Set", hr)
		}
	}
	if err := m.setVideoFormat(t, fps); err != nil {
		return err
	}
	return mfCheck(fmt.Sprintf("IMFTransform::SetOutputType(profile %d)", profile), m.tf.setOutputType(m.outID, t))
}

// setInputType sets NV12 input, starting from the encoder's own NV12 type when it offers one
// (hardware encoders may attach private attributes to it).
func (m *h264MFT) setInputType(fps int) error {
	var t *mfMediaType
	for i := uint32(0); ; i++ {
		c, hr := m.tf.getInputAvailableType(m.inID, i)
		if hr < 0 {
			break
		}
		if sub, hr := c.getGUID(&mfMTSubtype); hr >= 0 && sub == mfVideoFormatNV12 {
			t = c
			break
		}
		c.release()
	}
	if t == nil {
		var err error
		if t, err = mfCreateMediaType(); err != nil {
			return err
		}
		if hr := t.setGUID(&mfMTMajorType, &mfMediaTypeVideo); hr < 0 {
			t.release()
			return mfCheck("IMFMediaType::SetGUID", hr)
		}
		if hr := t.setGUID(&mfMTSubtype, &mfVideoFormatNV12); hr < 0 {
			t.release()
			return mfCheck("IMFMediaType::SetGUID", hr)
		}
	}
	defer t.release()
	if err := m.setVideoFormat(t, fps); err != nil {
		return err
	}
	// The input buffers are contiguous with the luma pitch equal to the width.
	if err := mfCheck("IMFMediaType::SetUINT32", t.setUINT32(&mfMTDefaultStride, uint32(m.w))); err != nil {
		return err
	}
	return mfCheck("IMFTransform::SetInputType(NV12)", m.tf.setInputType(m.inID, t))
}

// fetchParamSets reads the SPS/PPS the encoder publishes on its output type, used for
// keyframes that lack them in-band.
func (m *h264MFT) fetchParamSets() {
	t, hr := m.tf.getOutputCurrentType(m.outID)
	if hr < 0 {
		return
	}
	blob, hr := t.getAllocatedBlob(&mfMTMPEGSequenceHeader)
	t.release()
	if hr < 0 {
		return
	}
	ps, err := h264AnnexBFromAVCC(blob)
	if err != nil {
		log.Printf("h264: %s: ignoring sequence header: %v", m.name, err)
		return
	}
	m.paramSets = ps
}

// encode encodes one picture: bgra, or black when bgra is nil. It returns once the
// encoder produced that picture's access unit.
func (m *h264MFT) encode(bgra []byte, stride int, forceKey bool, bitrate uint32) ([]byte, bool, error) {
	if bitrate != m.bitrate {
		m.bitrate = bitrate
		if hr := m.codec.setUI4(&mfCodecAPIMeanBitRate, bitrate); hr < 0 {
			log.Printf("h264: %s rejected bitrate %d: HRESULT 0x%08X", m.name, bitrate, uint32(hr))
		}
	}

	in := m.in[m.inNext]
	m.inNext ^= 1
	mem, _, hr := in.b.lock()
	if err := mfCheck("IMFMediaBuffer::Lock", hr); err != nil {
		return nil, false, err
	}
	ySize := m.w * m.h
	y, uv := mem[:ySize], mem[ySize:ySize+ySize/2]
	if bgra != nil {
		m.conv.convert(y, m.w, uv, m.w, bgra, stride, m.w, m.h)
	} else {
		for i := range y {
			y[i] = 16
		}
		for i := range uv {
			uv[i] = 128
		}
	}
	in.b.unlock()
	if err := mfCheck("IMFMediaBuffer::SetCurrentLength", in.b.setCurrentLength(ySize*3/2)); err != nil {
		return nil, false, err
	}
	t := time.Since(m.start).Nanoseconds() / 100
	if t <= m.lastTime {
		t = m.lastTime + 1
	}
	m.lastTime = t
	if err := mfCheck("IMFSample::SetSampleTime", in.s.setSampleTime(t)); err != nil {
		return nil, false, err
	}
	if err := mfCheck("IMFSample::SetSampleDuration", in.s.setSampleDuration(m.frameDur)); err != nil {
		return nil, false, err
	}
	if forceKey {
		if err := mfCheck("CODECAPI_AVEncVideoForceKeyFrame", m.codec.setUI4(&mfCodecAPIVideoForceKeyFrame, 1)); err != nil {
			return nil, false, err
		}
	}

	m.raw = m.raw[:0]
	var err error
	if m.async {
		err = m.encodeAsync(in.s)
	} else {
		err = m.encodeSync(in.s)
	}
	if err != nil {
		return nil, false, err
	}
	mask := h264NALMask(m.raw)
	if mask&h264PictureMask == 0 {
		return nil, false, fmt.Errorf("h264: %s produced no picture", m.name)
	}
	if forceKey && mask&(1<<h264NALIDR) == 0 {
		return nil, false, fmt.Errorf("h264: %s ignored the keyframe request", m.name)
	}
	if mask&(1<<h264NALIDR) != 0 && mask&(1<<h264NALSPS) == 0 {
		if m.paramSets == nil {
			m.fetchParamSets()
		}
		if m.paramSets == nil {
			return nil, false, fmt.Errorf("h264: %s emitted a keyframe without SPS/PPS", m.name)
		}
	}
	au, key, _ := m.au.assemble(m.raw, m.paramSets)
	return au, key, nil
}

// encodeSync drives a synchronous MFT: ProcessInput, then ProcessOutput until it needs more
// input.
func (m *h264MFT) encodeSync(s *mfSample) error {
	hr := m.tf.processInput(m.inID, s)
	if err := mfCheck("IMFTransform::ProcessInput", hr); err != nil {
		return err
	}
	if m.drainEachFrame {
		return m.drainSync()
	}
	if err := m.pullSync(); err != nil {
		return err
	}
	if h264NALMask(m.raw)&h264PictureMask == 0 {
		log.Printf("h264: %s holds frames back; draining after every frame", m.name)
		m.drainEachFrame = true
		return m.drainSync()
	}
	return nil
}

func (m *h264MFT) drainSync() error {
	if err := mfCheck("MFT_MESSAGE_COMMAND_DRAIN", m.tf.processMessage(mftMessageCommandDrain)); err != nil {
		return err
	}
	return m.pullSync()
}

func (m *h264MFT) pullSync() error {
	for {
		got, err := m.processOutput()
		if err != nil || !got {
			return err
		}
	}
}

// processOutput fetches one output sample into m.raw. got is false when the MFT has none.
func (m *h264MFT) processOutput() (got bool, err error) {
	for {
		buf := mfOutputDataBuffer{streamID: m.outID}
		if m.outSample != nil {
			m.outBuf.setCurrentLength(0)
			buf.sample = m.outSample
		}
		hr := m.tf.processOutput(&buf)
		if buf.events != nil {
			buf.events.release()
		}
		switch uint32(hr) {
		case mfETransformNeedMoreInput:
			return false, nil
		case mfETransformStreamChange:
			// The encoder revised its output type; accept it and ask again.
			if err := m.renegotiateOutput(); err != nil {
				return false, err
			}
			if m.async {
				return false, nil // the output follows with another METransformHaveOutput
			}
			continue
		}
		if hr < 0 {
			return false, mfCheck("IMFTransform::ProcessOutput", hr)
		}
		if buf.sample == nil {
			return false, nil
		}
		err := m.appendSample(buf.sample)
		if buf.sample != m.outSample {
			buf.sample.release()
		}
		return true, err
	}
}

func (m *h264MFT) renegotiateOutput() error {
	t, hr := m.tf.getOutputAvailableType(m.outID, 0)
	if err := mfCheck("IMFTransform::GetOutputAvailableType", hr); err != nil {
		return err
	}
	hr = m.tf.setOutputType(m.outID, t)
	t.release()
	if err := mfCheck("IMFTransform::SetOutputType", hr); err != nil {
		return err
	}
	m.paramSets = nil
	m.fetchParamSets()
	return nil
}

// appendSample appends the sample's bytes to m.raw as Annex B.
func (m *h264MFT) appendSample(s *mfSample) error {
	b, hr := s.convertToContiguousBuffer()
	if err := mfCheck("IMFSample::ConvertToContiguousBuffer", hr); err != nil {
		return err
	}
	defer b.release()
	mem, n, hr := b.lock()
	if err := mfCheck("IMFMediaBuffer::Lock", hr); err != nil {
		return err
	}
	start := len(m.raw)
	m.raw = append(m.raw, mem[:n]...)
	b.unlock()
	return h264ToAnnexB(m.raw[start:])
}

// encodeAsync drives an asynchronous (hardware) MFT through its events: wait for
// METransformNeedInput, ProcessInput, then handle events until the frame's picture is out.
// An encoder that sits on the frame is drained (from then on after every frame).
func (m *h264MFT) encodeAsync(s *mfSample) error {
	for {
		for m.needInput == 0 {
			if err := m.handleEvent(h264EventTimeout, "METransformNeedInput"); err != nil {
				return err
			}
		}
		hr := m.tf.processInput(m.inID, s)
		if uint32(hr) == mfENotAccepting {
			m.needInput = 0 // stale requests, e.g. from before a drain
			continue
		}
		if err := mfCheck("IMFTransform::ProcessInput", hr); err != nil {
			return err
		}
		m.needInput--
		break
	}

	if !m.drainEachFrame {
		for h264NALMask(m.raw)&h264PictureMask == 0 {
			ev, ok, err := m.nextEvent(h264DrainAfter)
			if err != nil {
				return err
			}
			if !ok {
				log.Printf("h264: %s holds frames back; draining after every frame", m.name)
				m.drainEachFrame = true
				break
			}
			if err := m.onEvent(ev); err != nil {
				return err
			}
		}
		if !m.drainEachFrame {
			return nil
		}
	}
	if err := mfCheck("MFT_MESSAGE_COMMAND_DRAIN", m.tf.processMessage(mftMessageCommandDrain)); err != nil {
		return err
	}
	for {
		ev, ok, err := m.nextEvent(h264EventTimeout)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("h264: %s: no METransformDrainComplete within %v", m.name, h264EventTimeout)
		}
		if ev.typ == mfEventTransformDrainComplete {
			return nil
		}
		if err := m.onEvent(ev); err != nil {
			return err
		}
	}
}

// handleEvent waits up to d for the next event (waitingFor names what for, in the timeout
// error) and handles it.
func (m *h264MFT) handleEvent(d time.Duration, waitingFor string) error {
	ev, ok, err := m.nextEvent(d)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("h264: %s: no %s within %v", m.name, waitingFor, d)
	}
	return m.onEvent(ev)
}

func (m *h264MFT) onEvent(ev h264Event) error {
	switch ev.typ {
	case mfEventTransformNeedInput:
		m.needInput++
	case mfEventTransformHaveOutput:
		_, err := m.processOutput()
		return err
	case mfEventError:
		return mfCheck("encoder event MEError", min(ev.status, -1))
	}
	return nil
}

// nextEvent returns the next event from the pump; ok is false if none came within d.
func (m *h264MFT) nextEvent(d time.Duration) (ev h264Event, ok bool, err error) {
	m.timer.Reset(d)
	defer m.timer.Stop()
	select {
	case ev, open := <-m.events:
		if !open {
			return ev, false, errors.New("h264: encoder event queue closed")
		}
		if ev.status < 0 && ev.typ != mfEventError {
			return ev, false, fmt.Errorf("h264: encoder event %d failed: HRESULT 0x%08X", ev.typ, uint32(ev.status))
		}
		return ev, true, nil
	case <-m.timer.C:
		return ev, false, nil
	}
}

// pump forwards the async MFT's events to m.events. IMFMediaEventGenerator::GetEvent blocks,
// so it runs on its own (MTA) thread; it ends when GetEvent fails after shutdown.
func (m *h264MFT) pump() {
	defer close(m.pumpDone)
	defer close(m.events)
	runtime.LockOSThread()
	if mfCoInitialize() == nil {
		defer windows.CoUninitialize()
	}
	for !m.stopping.Load() {
		ev, hr := m.gen.getEvent()
		if hr < 0 {
			if !m.stopping.Load() {
				m.events <- h264Event{typ: mfEventError, status: hr}
			}
			return
		}
		typ, _ := ev.getType()
		status, _ := ev.getStatus()
		ev.release()
		m.events <- h264Event{typ, status}
	}
}

func (m *h264MFT) close() {
	if m.tf != nil {
		m.tf.processMessage(mftMessageCommandFlush)
		m.tf.processMessage(mftMessageNotifyEndStreaming)
	}
	if m.pumpDone != nil {
		m.stopping.Store(true)
		if m.shut != nil {
			m.shut.shutdown() // makes the pump's GetEvent fail with MF_E_SHUTDOWN
		} else {
			m.gen.queueEvent(0) // MEUnknown wakes the pump, which then sees stopping
		}
		deadline := time.After(5 * time.Second)
	wait:
		for {
			select {
			case _, ok := <-m.events:
				if !ok {
					break wait
				}
			case <-deadline:
				// Releasing the MFT under a blocked GetEvent could crash; leak it instead.
				log.Printf("h264: %s did not shut down; leaking it", m.name)
				return
			}
		}
		<-m.pumpDone
	}
	for _, in := range m.in {
		if in.b != nil {
			in.b.release()
		}
		if in.s != nil {
			in.s.release()
		}
	}
	if m.outBuf != nil {
		m.outBuf.release()
	}
	if m.outSample != nil {
		m.outSample.release()
	}
	if m.gen != nil {
		m.gen.release()
	}
	if m.shut != nil {
		m.shut.release()
	}
	if m.codec != nil {
		m.codec.release()
	}
	if m.tf != nil {
		m.tf.release()
	}
	if m.act != nil {
		m.act.shutdownObject()
		m.act.release()
	}
}
