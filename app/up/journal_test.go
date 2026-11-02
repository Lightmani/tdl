package up

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/expr-lang/expr"
	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram/peers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/pkg/prog"
	"github.com/iyear/tdl/pkg/utils"
)

type memStorage struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemStorage() *memStorage { return &memStorage{m: map[string][]byte{}} }

func (s *memStorage) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return v, nil
}

func (s *memStorage) Set(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func (s *memStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func createFiles(t *testing.T, names ...string) []*File {
	t.Helper()
	dir := t.TempDir()

	files := make([]*File, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(name), 0o644))
		files = append(files, &File{File: path})
	}
	return files
}

func paths(files []*File) []string {
	r := make([]string, 0, len(files))
	for _, f := range files {
		r = append(r, filepath.Base(f.File))
	}
	return r
}

func TestResume(t *testing.T) {
	ctx := context.Background()
	opts := Options{Paths: []string{"/videos"}, Chat: "channel", Continue: true}

	t.Run("no previous upload", func(t *testing.T) {
		jr := newJournal(newMemStorage(), batchFingerprint(opts))
		files := createFiles(t, "a.mp4", "b.mp4")

		pending, err := resume(ctx, jr, files, opts)
		require.NoError(t, err)
		assert.Equal(t, []string{"a.mp4", "b.mp4"}, paths(pending))
	})

	t.Run("skip uploaded files of the last unfinished upload", func(t *testing.T) {
		kvd := newMemStorage()
		files := createFiles(t, "a.mp4", "b.mp4", "c.mp4")

		// last run: a and c are uploaded, b failed and tdl exited
		last := newJournal(kvd, batchFingerprint(opts))
		require.NoError(t, last.markDone(ctx, fileID(files[0].File)))
		require.NoError(t, last.markDone(ctx, fileID(files[2].File)))

		jr := newJournal(kvd, batchFingerprint(opts))
		pending, err := resume(ctx, jr, files, opts)
		require.NoError(t, err)
		assert.Equal(t, []string{"b.mp4"}, paths(pending))
	})

	t.Run("modified file is uploaded again", func(t *testing.T) {
		kvd := newMemStorage()
		files := createFiles(t, "a.mp4")

		last := newJournal(kvd, batchFingerprint(opts))
		require.NoError(t, last.markDone(ctx, fileID(files[0].File)))

		require.NoError(t, os.WriteFile(files[0].File, []byte("re-encoded content"), 0o644))
		require.NoError(t, os.Chtimes(files[0].File, time.Now(), time.Now().Add(time.Hour)))

		pending, err := resume(ctx, newJournal(kvd, batchFingerprint(opts)), files, opts)
		require.NoError(t, err)
		assert.Len(t, pending, 1)
	})

	t.Run("restart forgets the last upload", func(t *testing.T) {
		kvd := newMemStorage()
		files := createFiles(t, "a.mp4", "b.mp4")

		last := newJournal(kvd, batchFingerprint(opts))
		require.NoError(t, last.markDone(ctx, fileID(files[0].File)))

		o := opts
		o.Continue, o.Restart = false, true
		pending, err := resume(ctx, newJournal(kvd, batchFingerprint(o)), files, o)
		require.NoError(t, err)
		assert.Len(t, pending, 2)
		assert.Empty(t, kvd.m, "resume record should be deleted")
	})

	t.Run("different destination is another batch", func(t *testing.T) {
		kvd := newMemStorage()
		files := createFiles(t, "a.mp4")

		last := newJournal(kvd, batchFingerprint(opts))
		require.NoError(t, last.markDone(ctx, fileID(files[0].File)))

		o := opts
		o.Chat = "another"
		pending, err := resume(ctx, newJournal(kvd, batchFingerprint(o)), files, o)
		require.NoError(t, err)
		assert.Len(t, pending, 1)
	})
}

func TestBatchFingerprint(t *testing.T) {
	a := batchFingerprint(Options{Paths: []string{"/a", "/b"}, Chat: "c"})
	assert.Equal(t, a, batchFingerprint(Options{Paths: []string{"/b", "/a"}, Chat: "c"}), "order of paths doesn't matter")
	assert.NotEqual(t, a, batchFingerprint(Options{Paths: []string{"/a", "/b"}, Chat: "d"}))
	assert.NotEqual(t, a, batchFingerprint(Options{Paths: []string{"/a", "/b"}, Chat: "c", Thread: 2}))
	assert.NotEqual(t, a, batchFingerprint(Options{Paths: []string{"/a"}, Chat: "c"}))
}

func TestSummarize(t *testing.T) {
	ctx := context.Background()

	t.Run("all uploaded clears record", func(t *testing.T) {
		kvd := newMemStorage()
		jr := newJournal(kvd, "fp")
		require.NoError(t, jr.markDone(ctx, "id"))

		require.NoError(t, summarize(ctx, jr, nil))
		assert.Empty(t, kvd.m)
	})

	t.Run("failures keep record and return error", func(t *testing.T) {
		kvd := newMemStorage()
		jr := newJournal(kvd, "fp")
		require.NoError(t, jr.markDone(ctx, "id"))
		jr.markFailed("/videos/b.mp4", errors.New("FLOOD_WAIT"))

		err := summarize(ctx, jr, nil)
		assert.ErrorContains(t, err, "1 file(s) failed")
		assert.NotEmpty(t, kvd.m)
	})

	t.Run("interrupted keeps record", func(t *testing.T) {
		kvd := newMemStorage()
		jr := newJournal(kvd, "fp")
		require.NoError(t, jr.markDone(ctx, "id"))

		assert.ErrorIs(t, summarize(ctx, jr, context.Canceled), context.Canceled)
		assert.NotEmpty(t, kvd.m)
	})
}

type fakePeer struct{ peers.Peer }

func (fakePeer) ID() int64           { return 1 }
func (fakePeer) VisibleName() string { return "chat" }

func TestIter_SkipBrokenFile(t *testing.T) {
	files := createFiles(t, "a.mp4", "c.mp4")
	files = []*File{
		files[0],
		{File: filepath.Join(t.TempDir(), "missing.mp4")},                   // removed after walking
		{File: files[1].File, Thumb: filepath.Join(t.TempDir(), "c.thumb")}, // broken thumbnail
		{File: files[1].File},
	}

	caption, err := expr.Compile(DefaultCaption, expr.Env(Env{}))
	require.NoError(t, err)

	var failed []string
	it := newIter(files, nil, caption, fakePeer{}, 0, false, false, 0, nil, func(f *File, err error) {
		failed = append(failed, filepath.Base(f.File))
	})

	var got []string
	for it.Next(context.Background()) {
		e := it.Value().(*iterElem)
		msg, _ := e.Caption()
		got = append(got, msg)
		require.NoError(t, e.file.Close())
	}

	require.NoError(t, it.Err(), "broken files must not stop the batch")
	assert.Equal(t, []string{"a.mp4", "c.mp4"}, got, "caption is the file name by default")
	assert.Equal(t, []string{"missing.mp4", "c.mp4"}, failed)
}

// --rm must only remove files which are really uploaded.
func TestProgress_RemoveOnlyUploaded(t *testing.T) {
	ctx := context.Background()
	files := createFiles(t, "failed.mp4", "ok.mp4")
	for _, f := range files {
		f.id = fileID(f.File)
	}

	kvd := newMemStorage()
	jr := newJournal(kvd, "fp")
	p := newProgress(ctx, prog.New(utils.Byte.FormatBinaryBytes), jr)

	it := newIter(files, nil, nil, fakePeer{}, 0, false, true, 0, nil, nil)
	it.caption, _ = expr.Compile(DefaultCaption, expr.Env(Env{}))

	require.True(t, it.Next(ctx))
	failed := it.Value()
	p.OnAdd(failed)
	p.OnDone(failed, errors.New("upload file: FILE_PARTS_INVALID"))

	require.True(t, it.Next(ctx))
	ok := it.Value()
	p.OnAdd(ok)
	p.OnDone(ok, nil)

	assert.FileExists(t, files[0].File, "failed upload must keep the local file")
	assert.NoFileExists(t, files[1].File)

	assert.False(t, jr.isDone(files[0].id))
	assert.True(t, jr.isDone(files[1].id))
	require.Len(t, jr.failures(), 1)
	assert.Equal(t, files[0].File, jr.failures()[0].path)
}
