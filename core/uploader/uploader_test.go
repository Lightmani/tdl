package uploader

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/go-faster/errors"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type invokeFunc func(ctx context.Context, input bin.Encoder, output bin.Decoder) error

func (f invokeFunc) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return f(ctx, input, output)
}

func respond(output bin.Decoder, obj bin.Encoder) error {
	var b bin.Buffer
	if err := obj.Encode(&b); err != nil {
		return err
	}
	return output.Decode(&b)
}

type testFile struct {
	*bytes.Reader
	name  string
	panic bool
}

func (f *testFile) Read(p []byte) (int, error) {
	if f.panic {
		panic("malformed file")
	}
	return f.Reader.Read(p)
}
func (f *testFile) Name() string { return f.name }
func (f *testFile) Size() int64  { return f.Reader.Size() }

type testElem struct{ file *testFile }

func (e *testElem) File() File                                 { return e.file }
func (e *testElem) Thumb() (File, bool)                        { return nil, false }
func (e *testElem) Caption() (string, []tg.MessageEntityClass) { return e.file.name, nil }
func (e *testElem) To() tg.InputPeerClass                      { return &tg.InputPeerSelf{} }
func (e *testElem) Thread() int                                { return 0 }
func (e *testElem) AsPhoto() bool                              { return false }

func newElem(name string, data []byte) *testElem {
	return &testElem{file: &testFile{Reader: bytes.NewReader(data), name: name}}
}

type testIter struct {
	elems []Elem
	cur   Elem
}

func (i *testIter) Next(context.Context) bool {
	if len(i.elems) == 0 {
		return false
	}
	i.cur, i.elems = i.elems[0], i.elems[1:]
	return true
}
func (i *testIter) Value() Elem { return i.cur }
func (i *testIter) Err() error  { return nil }

type testProgress struct {
	mu   sync.Mutex
	done map[string]error
}

func (p *testProgress) OnAdd(Elem)                   {}
func (p *testProgress) OnUpload(Elem, ProgressState) {}
func (p *testProgress) OnDone(elem Elem, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done[elem.File().Name()] = err
}

func run(ctx context.Context, t *testing.T, invoker tg.Invoker, elems ...Elem) (map[string]error, error) {
	t.Helper()

	p := &testProgress{done: map[string]error{}}
	err := New(Options{
		Client:   tg.NewClient(invoker),
		Threads:  1,
		Iter:     &testIter{elems: elems},
		Progress: p,
	}).Upload(ctx, 2)

	return p.done, err
}

// fakeServer accepts all uploads and records sent media
type fakeServer struct {
	mu    sync.Mutex
	media []tg.InputMediaClass
	fail  map[string]error // fail sending media whose caption is the key
}

func (s *fakeServer) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch req := input.(type) {
	case *tg.UploadSaveFilePartRequest:
		return respond(output, &tg.BoolTrue{})
	case *tg.MessagesSendMediaRequest:
		if err := s.fail[req.Message]; err != nil {
			return err
		}
		s.mu.Lock()
		s.media = append(s.media, req.Media)
		s.mu.Unlock()
		return respond(output, &tg.Updates{})
	default:
		return errors.Errorf("unexpected request %T", input)
	}
}

func TestUpload_FailureIsReportedAndOthersContinue(t *testing.T) {
	srv := &fakeServer{fail: map[string]error{"b.bin": errors.New("FILE_PART_MISSING")}}

	done, err := run(context.Background(), t, srv,
		newElem("a.bin", []byte("aaa")),
		newElem("b.bin", []byte("bbb")),
		newElem("c.bin", []byte("ccc")),
	)
	require.NoError(t, err, "one failed file must not fail the whole batch")

	require.Len(t, done, 3)
	assert.NoError(t, done["a.bin"])
	assert.ErrorContains(t, done["b.bin"], "FILE_PART_MISSING", "failure must be reported, not treated as success")
	assert.NoError(t, done["c.bin"])
	assert.Len(t, srv.media, 2)
}

func TestUpload_PanicIsIsolated(t *testing.T) {
	broken := newElem("broken.mp4", []byte("xxx"))
	broken.file.panic = true

	done, err := run(context.Background(), t, &fakeServer{}, broken, newElem("ok.bin", []byte("ok")))
	require.NoError(t, err)

	assert.ErrorContains(t, done["broken.mp4"], "panic")
	assert.NoError(t, done["ok.bin"])
}

func TestUpload_InternalCancelDoesNotStopBatch(t *testing.T) {
	// e.g. a dead connection may surface as context.Canceled while the batch itself is alive
	srv := &fakeServer{fail: map[string]error{"a.bin": errors.Wrap(context.Canceled, "conn closed")}}

	done, err := run(context.Background(), t, srv, newElem("a.bin", []byte("a")), newElem("b.bin", []byte("b")))
	require.NoError(t, err)
	assert.Error(t, done["a.bin"])
	assert.NoError(t, done["b.bin"])
}

func TestUpload_UserCancelStopsBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := invokeFunc(func(ctx context.Context, _ bin.Encoder, _ bin.Decoder) error {
		cancel() // user presses Ctrl+C during uploading
		<-ctx.Done()
		return ctx.Err()
	})

	_, err := run(ctx, t, blocked, newElem("a.bin", []byte("a")))
	assert.ErrorIs(t, err, context.Canceled)
}

// minimal HEVC-agnostic mp4: the old parser only accepted H.264 tracks, so this was sent as a plain file
func testMP4() []byte {
	box := func(typ string, payload ...[]byte) []byte {
		body := bytes.Join(payload, nil)
		b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
		return append(append(b, typ...), body...)
	}
	u32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

	matrix := make([]byte, 0, 36)
	for _, v := range []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		matrix = binary.BigEndian.AppendUint32(matrix, v)
	}

	return bytes.Join([][]byte{
		box("ftyp", []byte("isom"), u32(512), []byte("isomiso2mp41")),
		box("moov",
			box("mvhd", u32(0), u32(0), u32(0), u32(1000), u32(3000), make([]byte, 80)),
			box("trak",
				box("tkhd", u32(0), make([]byte, 36), matrix, u32(1280<<16), u32(720<<16)),
				box("mdia", box("hdlr", u32(0), u32(0), []byte("vide"), make([]byte, 13))),
			),
		),
		box("mdat", make([]byte, 128)),
	}, nil)
}

func TestUpload_VideoIsSentAsVideo(t *testing.T) {
	srv := &fakeServer{}

	done, err := run(context.Background(), t, srv, newElem("movie.mp4", testMP4()))
	require.NoError(t, err)
	require.NoError(t, done["movie.mp4"])
	require.Len(t, srv.media, 1)

	doc, ok := srv.media[0].(*tg.InputMediaUploadedDocument)
	require.True(t, ok, "got %T", srv.media[0])
	assert.Equal(t, "video/mp4", doc.MimeType)

	var video *tg.DocumentAttributeVideo
	for _, attr := range doc.Attributes {
		if v, ok := attr.(*tg.DocumentAttributeVideo); ok {
			video = v
		}
	}
	require.NotNil(t, video, "video must be uploaded as video, not as file")
	assert.Equal(t, 1280, video.W)
	assert.Equal(t, 720, video.H)
	assert.InDelta(t, 3.0, video.Duration, 0.001)
	assert.True(t, video.SupportsStreaming)
}
