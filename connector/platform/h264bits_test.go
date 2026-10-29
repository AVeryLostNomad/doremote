package platform

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestH264ToAnnexB(t *testing.T) {
	b := []byte{0, 0, 0, 2, 0x67, 0x42, 0, 0, 0, 1, 0x65}
	if err := h264ToAnnexB(b); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x65}; !bytes.Equal(b, want) {
		t.Fatalf("got % x, want % x", b, want)
	}

	// A 300-byte NAL's length prefix 00 00 01 2c looks like a 3-byte start code.
	long := append([]byte{0, 0, 1, 0x2c, 0x41}, make([]byte, 299)...)
	if err := h264ToAnnexB(long); err != nil || !bytes.HasPrefix(long, []byte{0, 0, 0, 1, 0x41}) {
		t.Fatalf("length-prefixed 300-byte NAL: err=%v prefix % x", err, long[:5])
	}

	for _, annexB := range [][]byte{
		{0, 0, 0, 1, 0x67, 0x64, 0, 0x1f, 0, 0, 1, 0x68, 0xee},
		{0, 0, 1, 0x65, 0x88, 0x84},
	} {
		orig := bytes.Clone(annexB)
		if err := h264ToAnnexB(annexB); err != nil || !bytes.Equal(annexB, orig) {
			t.Fatalf("Annex B changed: err=%v % x", err, annexB)
		}
	}
	if err := h264ToAnnexB([]byte{0, 0, 0, 9, 0x65}); err == nil {
		t.Fatal("overrunning length accepted")
	}
}

func TestH264AnnexBFromAVCC(t *testing.T) {
	rec := []byte{1, 0x64, 0, 0x1f, 0xff, 0xe1, 0, 2, 0x67, 0x64, 1, 0, 1, 0x68}
	got, err := h264AnnexBFromAVCC(rec)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 0, 0, 1, 0x67, 0x64, 0, 0, 0, 1, 0x68}; !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
	annexB := []byte{0, 0, 0, 1, 0x67}
	if got, _ := h264AnnexBFromAVCC(annexB); !bytes.Equal(got, annexB) {
		t.Fatal("Annex B sequence header changed")
	}
}

func TestH264AssemblePrependsParamSets(t *testing.T) {
	var a h264AccessUnit
	ps := []byte{0, 0, 1, 0x68, 0xce}
	out, key, vcl := a.assemble([]byte{0, 0, 1, 0x65, 0x88, 0, 0, 0, 1, 0x65, 0x99, 0}, ps)
	if !key || !vcl {
		t.Fatalf("key=%v vcl=%v", key, vcl)
	}
	if want := []byte{0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x88, 0, 0, 0, 1, 0x65, 0x99}; !bytes.Equal(out, want) {
		t.Fatalf("got % x, want % x", out, want)
	}
	out, key, vcl = a.assemble([]byte{0, 0, 0, 1, 0x41, 0x9a}, ps)
	if key || !vcl || !bytes.Equal(out, []byte{0, 0, 0, 1, 0x41, 0x9a}) {
		t.Fatalf("P frame: key=%v vcl=%v % x", key, vcl, out)
	}
	if _, _, vcl = a.assemble([]byte{0, 0, 0, 1, 0x09, 0x10}, ps); vcl {
		t.Fatal("AUD-only output reported as a picture")
	}
}

// TestH264SPSColourPatch encodes a clip with x264 (which writes VUI without colour
// description), patches its SPS both as-is and with the VUI stripped, and checks with
// ffprobe/ffmpeg that the colour is signalled as BT.709 limited range and that the decoded
// pictures are bit-identical.
func TestH264SPSColourPatch(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.h264")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30",
		"-frames:v", "8", "-c:v", "libx264", "-profile:v", "high", "-bf", "0", "-f", "h264", src).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg/libx264 unavailable: %v: %s", err, out)
	}
	orig, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if p := ffprobeStream(t, src); p["color_primaries"] == "bt709" {
		t.Fatalf("x264 already signals BT.709; test needs an unsignalled source: %v", p)
	}

	stripVUI := func(nal []byte) []byte {
		rbsp := h264Unescape(nal[1:])
		r, _ := h264ReadSPSHead(rbsp)
		if r.err != nil {
			t.Fatal(r.err)
		}
		var w h264BitWriter
		w.copyBits(rbsp, 0, r.pos)
		w.put(0, 1) // vui_parameters_present_flag
		w.put(1, 1) // rbsp_stop_one_bit
		for w.n%8 != 0 {
			w.put(0, 1)
		}
		return h264Escape([]byte{nal[0]}, w.b)
	}

	for _, strip := range []bool{false, true} {
		var out []byte
		for nal, rest := h264NextNAL(orig); nal != nil; nal, rest = h264NextNAL(rest) {
			if nal[0]&0x1f == h264NALSPS {
				if strip {
					nal = stripVUI(nal)
				}
				patched, changed, err := h264SPSWithBT709VUI(nal)
				if err != nil || !changed {
					t.Fatalf("strip=%v: changed=%v err=%v", strip, changed, err)
				}
				again, changed, err := h264SPSWithBT709VUI(patched)
				if err != nil || changed || !bytes.Equal(again, patched) {
					t.Fatalf("strip=%v: re-patching changed=%v err=%v", strip, changed, err)
				}
				nal = patched
			}
			out = append(append(out, h264StartCode...), nal...)
		}
		dst := filepath.Join(dir, "patched.h264")
		if err := os.WriteFile(dst, out, 0o644); err != nil {
			t.Fatal(err)
		}
		p := ffprobeStream(t, dst)
		for k, want := range map[string]any{"color_range": "tv", "color_primaries": "bt709", "color_transfer": "bt709",
			"color_space": "bt709", "width": 320.0, "height": 240.0, "has_b_frames": 0.0} {
			if p[k] != want {
				t.Errorf("strip=%v: %s = %v, want %v", strip, k, p[k], want)
			}
		}
		if a, b := framemd5(t, src), framemd5(t, dst); a != b {
			t.Errorf("strip=%v: decoded pictures differ after patching:\n%s\n%s", strip, a, b)
		}
	}
}

func ffprobeStream(t *testing.T, path string) map[string]any {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var v struct{ Streams []map[string]any }
	if err := json.Unmarshal(out, &v); err != nil || len(v.Streams) != 1 {
		t.Fatalf("ffprobe %s: %v %s", path, err, out)
	}
	return v.Streams[0]
}

func framemd5(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg framemd5 %s: %v", path, err)
	}
	var lines []string
	for _, l := range bytes.Split(out, []byte("\n")) {
		if len(l) > 0 && l[0] != '#' {
			f := bytes.Split(l, []byte(","))
			lines = append(lines, string(bytes.TrimSpace(f[len(f)-1])))
		}
	}
	if len(lines) != 8 {
		t.Fatalf("%s decoded to %d frames, want 8", path, len(lines))
	}
	return string(bytes.Join([][]byte{[]byte(lines[0]), []byte(lines[7])}, []byte(" ")))
}
