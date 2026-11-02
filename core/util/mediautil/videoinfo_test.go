package mediautil

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------- ISO-BMFF fixtures ----------------

func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func mvhd(timescale, duration uint32) []byte {
	return box("mvhd", u32(0), u32(0), u32(0), u32(timescale), u32(duration), make([]byte, 80))
}

func mvhdV1(timescale uint32, duration uint64) []byte {
	return box("mvhd", u32(1<<24), u64(0), u64(0), u32(timescale), u64(duration), make([]byte, 80))
}

// tkhd v0 with the given matrix {a, b, c, d} and 16.16 fixed-point size
func tkhd(a, b, c, d int32, w, h uint32) []byte {
	matrix := make([]byte, 0, 36)
	for _, v := range []int32{a, b, 0, c, d, 0, 0, 0, 0x40000000} {
		matrix = binary.BigEndian.AppendUint32(matrix, uint32(v))
	}
	return box("tkhd", u32(0), make([]byte, 20), make([]byte, 16), matrix, u32(w<<16), u32(h<<16))
}

func trak(handler string, w, h uint32, rotate bool) []byte {
	a, b, c, d := int32(0x10000), int32(0), int32(0), int32(0x10000)
	if rotate { // 90 degrees
		a, b, c, d = 0, 0x10000, -0x10000, 0
	}
	return box("trak",
		tkhd(a, b, c, d, w, h),
		box("mdia",
			box("mdhd", u32(0), u32(0), u32(0), u32(1000), u32(2500), u32(0)),
			box("hdlr", u32(0), u32(0), []byte(handler), make([]byte, 13)),
			box("minf", box("stbl", make([]byte, 64))),
		),
	)
}

func ftyp() []byte { return box("ftyp", []byte("isom"), u32(512), []byte("isomiso2avc1mp41")) }

func TestGetVideoInfo_ISOBMFF(t *testing.T) {
	mdat := box("mdat", make([]byte, 4096))

	tests := []struct {
		name     string
		data     []byte
		expected *VideoInfo
		wantErr  bool
	}{
		{
			name:     "moov after mdat",
			data:     bytes.Join([][]byte{ftyp(), mdat, box("moov", mvhd(1000, 3000), trak("soun", 0, 0, false), trak("vide", 1920, 1080, false))}, nil),
			expected: &VideoInfo{Duration: 3 * time.Second, Width: 1920, Height: 1080, Streamable: true},
		},
		{
			name:     "faststart",
			data:     bytes.Join([][]byte{ftyp(), box("moov", mvhd(600, 900), trak("vide", 640, 360, false)), mdat}, nil),
			expected: &VideoInfo{Duration: 1500 * time.Millisecond, Width: 640, Height: 360, Streamable: true},
		},
		{
			name:     "rotated portrait video",
			data:     bytes.Join([][]byte{ftyp(), box("moov", mvhd(1000, 3000), trak("vide", 1920, 1080, true)), mdat}, nil),
			expected: &VideoInfo{Duration: 3 * time.Second, Width: 1080, Height: 1920, Streamable: true},
		},
		{
			name:     "mvhd v1",
			data:     bytes.Join([][]byte{ftyp(), box("moov", mvhdV1(90000, 90000*65), trak("vide", 1280, 720, false))}, nil),
			expected: &VideoInfo{Duration: 65 * time.Second, Width: 1280, Height: 720, Streamable: true},
		},
		{
			name:     "fallback to track duration",
			data:     bytes.Join([][]byte{ftyp(), box("moov", mvhd(1000, 0), trak("vide", 1280, 720, false))}, nil),
			expected: &VideoInfo{Duration: 2500 * time.Millisecond, Width: 1280, Height: 720, Streamable: true},
		},
		{
			name: "fragmented with mehd",
			data: bytes.Join([][]byte{ftyp(), box("moov", mvhd(1000, 0),
				box("mvex", box("mehd", u32(0), u32(4000))),
				trak("vide", 1280, 720, false))}, nil),
			expected: &VideoInfo{Duration: 4 * time.Second, Width: 1280, Height: 720, Streamable: true},
		},
		{
			name:    "audio only",
			data:    bytes.Join([][]byte{ftyp(), box("moov", mvhd(1000, 3000), trak("soun", 0, 0, false))}, nil),
			wantErr: true,
		},
		{
			name:    "no moov (truncated download)",
			data:    bytes.Join([][]byte{ftyp(), mdat[:1024]}, nil),
			wantErr: true,
		},
		{
			name:    "unsupported container",
			data:    []byte("RIFF\x00\x00\x00\x00AVI LIST"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := GetVideoInfo(bytes.NewReader(tt.data))
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, info)
		})
	}
}

// ---------------- Matroska fixtures ----------------

func ebml(id uint64, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	var out []byte
	for shift := 56; shift >= 0; shift -= 8 { // id keeps its marker bits
		if b := byte(id >> shift); b != 0 || len(out) > 0 {
			out = append(out, b)
		}
	}
	// 8-byte size vint
	size := u64(uint64(len(body)))
	size[0] = 0x01
	return append(append(out, size...), body...)
}

func ebmlUint(id, v uint64) []byte { return ebml(id, u64(v)) }

func ebmlFloat(id uint64, v float64) []byte { return ebml(id, u64(math.Float64bits(v))) }

func mkvHeader() []byte { return ebml(mkvIDEBML, ebml(0x4282, []byte("matroska"))) }

func TestGetVideoInfo_Matroska(t *testing.T) {
	info := ebml(mkvIDInfo, ebmlUint(mkvIDTimestampScale, 1000000), ebmlFloat(mkvIDDuration, 4200))
	audio := ebml(mkvIDTrackEntry, ebmlUint(mkvIDTrackType, 2))
	video := func(fields ...[]byte) []byte {
		return ebml(mkvIDTrackEntry, ebmlUint(mkvIDTrackType, mkvTrackTypeVideo), ebml(mkvIDVideo, fields...))
	}
	cluster := ebml(0x1F43B675, make([]byte, 2048))

	tests := []struct {
		name     string
		data     []byte
		expected *VideoInfo
		wantErr  bool
	}{
		{
			name: "basic",
			data: append(mkvHeader(), ebml(mkvIDSegment, info, ebml(mkvIDTracks, audio,
				video(ebmlUint(mkvIDPixelWidth, 1920), ebmlUint(mkvIDPixelHeight, 1080))), cluster)...),
			expected: &VideoInfo{Duration: 4200 * time.Millisecond, Width: 1920, Height: 1080},
		},
		{
			name: "anamorphic uses display size",
			data: append(mkvHeader(), ebml(mkvIDSegment, info, ebml(mkvIDTracks,
				video(ebmlUint(mkvIDPixelWidth, 720), ebmlUint(mkvIDPixelHeight, 576),
					ebmlUint(mkvIDDisplayWidth, 1024), ebmlUint(mkvIDDisplayHeight, 576))))...),
			expected: &VideoInfo{Duration: 4200 * time.Millisecond, Width: 1024, Height: 576},
		},
		{
			name: "tracks after clusters",
			data: append(mkvHeader(), ebml(mkvIDSegment, info, cluster, cluster, ebml(mkvIDTracks,
				video(ebmlUint(mkvIDPixelWidth, 640), ebmlUint(mkvIDPixelHeight, 360))))...),
			expected: &VideoInfo{Duration: 4200 * time.Millisecond, Width: 640, Height: 360},
		},
		{
			name:    "audio only",
			data:    append(mkvHeader(), ebml(mkvIDSegment, info, ebml(mkvIDTracks, audio))...),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := GetVideoInfo(bytes.NewReader(tt.data))
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, info)
		})
	}

	t.Run("unknown size segment", func(t *testing.T) {
		seg := ebml(mkvIDSegment, info, ebml(mkvIDTracks, video(ebmlUint(mkvIDPixelWidth, 640), ebmlUint(mkvIDPixelHeight, 360))))
		// replace 8-byte size with "unknown" (all value bits set)
		copy(seg[4:12], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

		got, err := GetVideoInfo(bytes.NewReader(append(mkvHeader(), seg...)))
		require.NoError(t, err)
		assert.Equal(t, 640, got.Width)
		assert.Equal(t, 360, got.Height)
	})
}

// A malformed file must never panic, since one broken file would crash the whole upload batch.
func TestGetVideoInfo_Malformed(t *testing.T) {
	samples := [][]byte{
		bytes.Join([][]byte{ftyp(), box("moov", mvhd(1000, 3000), trak("vide", 1920, 1080, true)), box("mdat", make([]byte, 64))}, nil),
		append(mkvHeader(), ebml(mkvIDSegment,
			ebml(mkvIDInfo, ebmlFloat(mkvIDDuration, 1000)),
			ebml(mkvIDTracks, ebml(mkvIDTrackEntry, ebmlUint(mkvIDTrackType, 1),
				ebml(mkvIDVideo, ebmlUint(mkvIDPixelWidth, 1), ebmlUint(mkvIDPixelHeight, 1)))))...),
	}

	rnd := rand.New(rand.NewSource(1))
	for _, sample := range samples {
		for i := 0; i < len(sample); i++ {
			_, _ = GetVideoInfo(bytes.NewReader(sample[:i])) // truncated at every position

			mutated := bytes.Clone(sample)
			mutated[i] = byte(rnd.Intn(256))
			_, _ = GetVideoInfo(bytes.NewReader(mutated))
		}
	}
}

// TestGetVideoInfo_FFmpeg checks real files of popular codecs, which the old H.264-only parser
// rejected, so they were uploaded as plain files that can't be previewed.
func TestGetVideoInfo_FFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}

	dir := t.TempDir()
	ffmpeg := func(t *testing.T, args ...string) {
		args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg %v: %v: %s", args, err, out)
		}
	}
	encode := func(codec ...string) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			args := []string{"-f", "lavfi", "-i", "testsrc=size=320x180:rate=10", "-t", "2"}
			ffmpeg(t, append(append(args, codec...), path)...)
		}
	}

	tests := []struct {
		file          string
		gen           func(t *testing.T, path string)
		width, height int
		streamable    bool
	}{
		{file: "h264.mp4", gen: encode("-c:v", "libx264"), width: 320, height: 180, streamable: true},
		{file: "hevc.mp4", gen: encode("-c:v", "libx265", "-tag:v", "hvc1"), width: 320, height: 180, streamable: true},
		{file: "hevc.mov", gen: encode("-c:v", "libx265", "-tag:v", "hvc1"), width: 320, height: 180, streamable: true},
		{file: "vp9.mp4", gen: encode("-c:v", "libvpx-vp9"), width: 320, height: 180, streamable: true},
		{file: "faststart.mp4", gen: encode("-c:v", "libx264", "-movflags", "+faststart"), width: 320, height: 180, streamable: true},
		{file: "h264.mkv", gen: encode("-c:v", "libx264"), width: 320, height: 180},
		{file: "vp9.webm", gen: encode("-c:v", "libvpx-vp9"), width: 320, height: 180},
		{
			// phones record portrait videos as landscape frames with a rotation matrix
			file: "portrait.mp4",
			gen: func(t *testing.T, path string) {
				base := filepath.Join(dir, "portrait_base.mp4")
				encode("-c:v", "libx264")(t, base)
				ffmpeg(t, "-display_rotation", "90", "-i", base, "-c", "copy", path)
			},
			width: 180, height: 320, streamable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			path := filepath.Join(dir, tt.file)
			tt.gen(t, path)

			f, err := os.Open(path)
			require.NoError(t, err)
			defer f.Close()

			info, err := GetVideoInfo(f)
			require.NoError(t, err)
			assert.Equal(t, tt.width, info.Width)
			assert.Equal(t, tt.height, info.Height)
			assert.Equal(t, tt.streamable, info.Streamable)
			assert.InDelta(t, 2, info.Duration.Seconds(), 0.2)
		})
	}
}
