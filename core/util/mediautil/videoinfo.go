package mediautil

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"time"

	"github.com/go-faster/errors"
)

// VideoInfo is the metadata Telegram needs to render a document as a playable video.
type VideoInfo struct {
	Duration time.Duration
	// Width and Height are display dimensions, i.e. rotation is already applied.
	Width  int
	Height int
	// Streamable reports whether the container can be streamed by Telegram clients (MP4/MOV family).
	Streamable bool
}

// ErrUnsupportedContainer is returned when the stream is neither ISO-BMFF (mp4/mov/m4v/3gp) nor Matroska/WebM.
var ErrUnsupportedContainer = errors.New("unsupported video container")

// GetVideoInfo reads the container headers of r and returns its video metadata.
//
// Unlike GetMP4Info, it does not depend on the codec (H.264, H.265/HEVC, AV1, VP9... are all fine),
// supports Matroska/WebM, and only reads box/element headers, so media data is never loaded into memory.
func GetVideoInfo(r io.ReadSeeker) (*VideoInfo, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, errors.Wrap(err, "seek end")
	}

	head := make([]byte, 8)
	if _, err = readAt(r, head, 0); err != nil {
		return nil, errors.Wrap(err, "read header")
	}

	switch {
	case bytes.Equal(head[:4], ebmlMagic):
		return getMatroskaInfo(r, size)
	case isISOBoxType(head[4:8]):
		return getISOBMFFInfo(r, size)
	default:
		return nil, ErrUnsupportedContainer
	}
}

func readAt(r io.ReadSeeker, buf []byte, off int64) (int, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r, buf)
}

// ---------------- ISO-BMFF (mp4/mov/m4v/3gp) ----------------

func isISOBoxType(t []byte) bool {
	switch string(t) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide", "pnot":
		return true
	}
	return false
}

type isoBox struct {
	typ        string
	start, end int64 // [start, end) of the whole box
	data       int64 // start of payload
}

// readBox reads the box header at pos. The box must end before limit.
func readBox(r io.ReadSeeker, pos, limit int64) (isoBox, error) {
	h := make([]byte, 16)
	if limit-pos < 8 {
		return isoBox{}, errors.New("truncated box header")
	}
	if _, err := readAt(r, h[:8], pos); err != nil {
		return isoBox{}, err
	}

	b := isoBox{typ: string(h[4:8]), start: pos, data: pos + 8}
	size := int64(binary.BigEndian.Uint32(h[:4]))

	switch size {
	case 0: // box extends to the end of its parent
		b.end = limit
	case 1: // 64-bit largesize
		if limit-pos < 16 {
			return isoBox{}, errors.New("truncated box header")
		}
		if _, err := readAt(r, h[8:16], pos+8); err != nil {
			return isoBox{}, err
		}
		large := binary.BigEndian.Uint64(h[8:16])
		if large > math.MaxInt64 {
			return isoBox{}, errors.New("invalid box size")
		}
		b.data, b.end = pos+16, pos+int64(large)
	default:
		b.end = pos + size
	}

	if b.end < b.data || b.end > limit {
		return isoBox{}, errors.Errorf("invalid %q box size", b.typ)
	}
	return b, nil
}

// walkBoxes calls fn for each child box in [start, end).
func walkBoxes(r io.ReadSeeker, start, end int64, fn func(isoBox) error) error {
	for pos := start; end-pos >= 8; {
		b, err := readBox(r, pos, end)
		if err != nil {
			return err
		}
		if err = fn(b); err != nil {
			return err
		}
		pos = b.end
	}
	return nil
}

// payload reads at most n bytes of the box payload.
func (b isoBox) payload(r io.ReadSeeker, n int64) ([]byte, error) {
	if l := b.end - b.data; l < n {
		n = l
	}
	buf := make([]byte, n)
	if _, err := readAt(r, buf, b.data); err != nil {
		return nil, err
	}
	return buf, nil
}

var errStop = errors.New("stop")

func getISOBMFFInfo(r io.ReadSeeker, size int64) (*VideoInfo, error) {
	var moov *isoBox
	err := walkBoxes(r, 0, size, func(b isoBox) error {
		if b.typ == "moov" {
			moov = &b
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, errors.Wrap(err, "walk boxes")
	}
	if moov == nil {
		return nil, errors.New("moov box not found")
	}

	var (
		timescale, duration, fragmentDuration uint64
		video                                 *isoTrack
	)
	err = walkBoxes(r, moov.data, moov.end, func(b isoBox) error {
		switch b.typ {
		case "mvhd":
			timescale, duration = parseTimeBox(r, b)
		case "mvex": // fragmented mp4 may store its duration in mvex/mehd
			return walkBoxes(r, b.data, b.end, func(c isoBox) error {
				if c.typ == "mehd" {
					fragmentDuration = parseMehd(r, c)
				}
				return nil
			})
		case "trak":
			if video != nil {
				return nil
			}
			t, err := parseTrak(r, b)
			if err != nil {
				return err
			}
			if t.video && t.width > 0 && t.height > 0 {
				video = t
			}
		}
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "parse moov")
	}
	if video == nil {
		return nil, errors.New("no video track found")
	}

	d := scaleDuration(duration, timescale)
	if d == 0 {
		d = scaleDuration(fragmentDuration, timescale)
	}
	if d == 0 {
		d = video.duration
	}

	return &VideoInfo{
		Duration:   d,
		Width:      video.width,
		Height:     video.height,
		Streamable: true,
	}, nil
}

// parseTimeBox returns timescale and duration of mvhd or mdhd, which share the same layout:
//
//	v0: version/flags(4) creation(4) modification(4) timescale(4) duration(4)
//	v1: version/flags(4) creation(8) modification(8) timescale(4) duration(8)
func parseTimeBox(r io.ReadSeeker, b isoBox) (timescale, duration uint64) {
	p, err := b.payload(r, 32)
	if err != nil || len(p) < 20 {
		return 0, 0
	}

	if p[0] == 1 {
		if len(p) < 32 {
			return 0, 0
		}
		timescale, duration = uint64(binary.BigEndian.Uint32(p[20:])), binary.BigEndian.Uint64(p[24:])
		if duration == math.MaxUint64 { // unknown
			duration = 0
		}
		return timescale, duration
	}

	timescale, duration = uint64(binary.BigEndian.Uint32(p[12:])), uint64(binary.BigEndian.Uint32(p[16:]))
	if duration == math.MaxUint32 { // unknown
		duration = 0
	}
	return timescale, duration
}

// parseMehd returns fragment_duration of mehd, which is in movie timescale.
func parseMehd(r io.ReadSeeker, b isoBox) uint64 {
	p, err := b.payload(r, 12)
	if err != nil || len(p) < 8 {
		return 0
	}
	if p[0] == 1 {
		if len(p) < 12 {
			return 0
		}
		return binary.BigEndian.Uint64(p[4:])
	}
	return uint64(binary.BigEndian.Uint32(p[4:]))
}

func scaleDuration(duration, timescale uint64) time.Duration {
	if timescale == 0 || duration == 0 {
		return 0
	}
	sec := float64(duration) / float64(timescale)
	if sec > float64(math.MaxInt64/int64(time.Second)) {
		return 0
	}
	return time.Duration(sec * float64(time.Second))
}

type isoTrack struct {
	video         bool
	width, height int
	duration      time.Duration
}

func parseTrak(r io.ReadSeeker, trak isoBox) (*isoTrack, error) {
	t := &isoTrack{}
	err := walkBoxes(r, trak.data, trak.end, func(b isoBox) error {
		switch b.typ {
		case "tkhd":
			t.width, t.height = parseTkhd(r, b)
		case "mdia":
			return walkBoxes(r, b.data, b.end, func(c isoBox) error {
				switch c.typ {
				case "hdlr":
					// version/flags(4) + pre_defined(4) + handler_type(4)
					if p, err := c.payload(r, 12); err == nil && len(p) == 12 {
						t.video = string(p[8:12]) == "vide"
					}
				case "mdhd":
					timescale, duration := parseTimeBox(r, c)
					t.duration = scaleDuration(duration, timescale)
				}
				return nil
			})
		}
		return nil
	})
	return t, err
}

// parseTkhd returns display width and height of the track, with rotation applied.
func parseTkhd(r io.ReadSeeker, b isoBox) (int, int) {
	p, err := b.payload(r, 96)
	if err != nil || len(p) < 4 {
		return 0, 0
	}

	// matrix offset: version/flags(4) + times&ids(20 for v0, 32 for v1) + reserved(8) + layer/group/volume/reserved(8)
	off := 4 + 20 + 16
	if p[0] == 1 {
		off = 4 + 32 + 16
	}
	if len(p) < off+36+8 {
		return 0, 0
	}

	m := func(i int) int32 { return int32(binary.BigEndian.Uint32(p[off+i*4:])) }
	// width/height are 16.16 fixed-point numbers
	w := int(binary.BigEndian.Uint32(p[off+36:]) >> 16)
	h := int(binary.BigEndian.Uint32(p[off+40:]) >> 16)

	// matrix {a, b, u, c, d, v, x, y, w}: a == d == 0 means the track is rotated by 90 or 270 degrees,
	// e.g. portrait videos recorded by phones
	if a, bb, c, d := m(0), m(1), m(3), m(4); a == 0 && d == 0 && bb != 0 && c != 0 {
		w, h = h, w
	}

	return w, h
}

// ---------------- Matroska / WebM ----------------

var ebmlMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}

const (
	mkvIDEBML           = 0x1A45DFA3
	mkvIDSegment        = 0x18538067
	mkvIDInfo           = 0x1549A966
	mkvIDTimestampScale = 0x2AD7B1
	mkvIDDuration       = 0x4489
	mkvIDTracks         = 0x1654AE6B
	mkvIDTrackEntry     = 0xAE
	mkvIDTrackType      = 0x83
	mkvIDVideo          = 0xE0
	mkvIDPixelWidth     = 0xB0
	mkvIDPixelHeight    = 0xBA
	mkvIDDisplayWidth   = 0x54B0
	mkvIDDisplayHeight  = 0x54BA
	mkvIDDisplayUnit    = 0x54B2

	mkvTrackTypeVideo = 1
)

type ebmlElem struct {
	id        uint64
	data, end int64
	unknown   bool // unknown size, the element extends to the end of its parent
}

// readVint reads an EBML variable-length integer at pos. keepMarker is true for element IDs.
func readVint(r io.ReadSeeker, pos int64, keepMarker bool) (val uint64, n int, allOnes bool, err error) {
	b := make([]byte, 8)
	if _, err = readAt(r, b[:1], pos); err != nil {
		return 0, 0, false, err
	}

	n = 1
	for mask := byte(0x80); n <= 8 && b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 {
		return 0, 0, false, errors.New("invalid vint")
	}
	if n > 1 {
		if _, err = readAt(r, b[1:n], pos+1); err != nil {
			return 0, 0, false, err
		}
	}

	val = uint64(b[0])
	if !keepMarker {
		val &= uint64(0xFF >> n)
	}
	allOnes = val == uint64(0xFF>>n)
	for i := 1; i < n; i++ {
		val = val<<8 | uint64(b[i])
		allOnes = allOnes && b[i] == 0xFF
	}

	return val, n, allOnes, nil
}

func readEBMLElem(r io.ReadSeeker, pos, limit int64) (ebmlElem, error) {
	id, idLen, _, err := readVint(r, pos, true)
	if err != nil {
		return ebmlElem{}, err
	}
	size, sizeLen, unknown, err := readVint(r, pos+int64(idLen), false)
	if err != nil {
		return ebmlElem{}, err
	}

	e := ebmlElem{id: id, data: pos + int64(idLen+sizeLen), unknown: unknown}
	if e.data > limit {
		return ebmlElem{}, errors.New("truncated element header")
	}
	if unknown {
		e.end = limit
	} else {
		if size > uint64(limit-e.data) {
			return ebmlElem{}, errors.Errorf("invalid size of element %#x", id)
		}
		e.end = e.data + int64(size)
	}
	return e, nil
}

// walkEBML calls fn for each child element in [start, end).
// Children with unknown size (e.g. live-streamed clusters) can't be skipped, so walking stops there.
func walkEBML(r io.ReadSeeker, start, end int64, fn func(ebmlElem) error) error {
	for pos := start; pos < end; {
		e, err := readEBMLElem(r, pos, end)
		if err != nil {
			return err
		}
		if err = fn(e); err != nil {
			return err
		}
		if e.unknown {
			return nil
		}
		pos = e.end
	}
	return nil
}

func (e ebmlElem) bytes(r io.ReadSeeker, max int64) ([]byte, error) {
	n := e.end - e.data
	if n > max {
		return nil, errors.Errorf("element %#x too large", e.id)
	}
	buf := make([]byte, n)
	if _, err := readAt(r, buf, e.data); err != nil {
		return nil, err
	}
	return buf, nil
}

func (e ebmlElem) uint(r io.ReadSeeker) (uint64, error) {
	b, err := e.bytes(r, 8)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

func (e ebmlElem) float(r io.ReadSeeker) (float64, error) {
	b, err := e.bytes(r, 8)
	if err != nil {
		return 0, err
	}
	switch len(b) {
	case 0:
		return 0, nil
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	default:
		return 0, errors.Errorf("invalid float size %d", len(b))
	}
}

func getMatroskaInfo(r io.ReadSeeker, size int64) (*VideoInfo, error) {
	header, err := readEBMLElem(r, 0, size)
	if err != nil || header.id != mkvIDEBML || header.unknown {
		return nil, errors.New("invalid EBML header")
	}
	segment, err := readEBMLElem(r, header.end, size)
	if err != nil || segment.id != mkvIDSegment {
		return nil, errors.New("segment not found")
	}

	var (
		scale         uint64 = 1000000 // default TimestampScale: 1ms
		rawDuration   float64
		width, height int
		infoFound     bool
		tracksFound   bool
	)

	err = walkEBML(r, segment.data, segment.end, func(e ebmlElem) error {
		switch e.id {
		case mkvIDInfo:
			infoFound = true
			return walkEBML(r, e.data, e.end, func(c ebmlElem) error {
				switch c.id {
				case mkvIDTimestampScale:
					if v, err := c.uint(r); err == nil && v > 0 {
						scale = v
					}
				case mkvIDDuration:
					if v, err := c.float(r); err == nil {
						rawDuration = v
					}
				}
				return nil
			})
		case mkvIDTracks:
			tracksFound = true
			return walkEBML(r, e.data, e.end, func(c ebmlElem) error {
				if c.id != mkvIDTrackEntry || width > 0 {
					return nil
				}
				w, h, err := parseMatroskaTrack(r, c)
				if err != nil {
					return err
				}
				width, height = w, h
				return nil
			})
		}

		if infoFound && tracksFound {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, errors.Wrap(err, "parse segment")
	}
	if width <= 0 || height <= 0 {
		return nil, errors.New("no video track found")
	}

	var duration time.Duration
	if ns := rawDuration * float64(scale); ns > 0 && ns < math.MaxInt64 {
		duration = time.Duration(ns)
	}

	return &VideoInfo{
		Duration:   duration,
		Width:      width,
		Height:     height,
		Streamable: false,
	}, nil
}

func parseMatroskaTrack(r io.ReadSeeker, entry ebmlElem) (int, int, error) {
	var (
		isVideo                  bool
		pw, ph, dw, dh, dispUnit uint64
	)

	err := walkEBML(r, entry.data, entry.end, func(e ebmlElem) error {
		switch e.id {
		case mkvIDTrackType:
			v, err := e.uint(r)
			isVideo = err == nil && v == mkvTrackTypeVideo
		case mkvIDVideo:
			fields := map[uint64]*uint64{
				mkvIDPixelWidth:    &pw,
				mkvIDPixelHeight:   &ph,
				mkvIDDisplayWidth:  &dw,
				mkvIDDisplayHeight: &dh,
				mkvIDDisplayUnit:   &dispUnit,
			}
			return walkEBML(r, e.data, e.end, func(c ebmlElem) error {
				if dst, ok := fields[c.id]; ok {
					v, err := c.uint(r)
					if err != nil {
						return err
					}
					*dst = v
				}
				return nil
			})
		}
		return nil
	})
	if err != nil || !isVideo {
		return 0, 0, err
	}

	// display size (in pixels) takes pixel aspect ratio into account
	if dispUnit == 0 && dw > 0 && dh > 0 {
		pw, ph = dw, dh
	}
	if pw > math.MaxInt32 || ph > math.MaxInt32 {
		return 0, 0, errors.New("invalid video size")
	}
	return int(pw), int(ph), nil
}
