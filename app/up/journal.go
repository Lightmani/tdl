package up

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/go-faster/errors"

	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/pkg/key"
)

// journal records which files of an upload batch have been uploaded successfully,
// so that an interrupted or partially failed batch can be resumed without
// uploading the same files twice. It also collects failures for the final summary.
//
// The record is persisted after every successful upload (not only on exit),
// so it survives crashes and killed processes, and is deleted once the whole batch succeeds.
type journal struct {
	kvd storage.Storage
	key string

	mu     sync.Mutex
	done   map[string]struct{}
	failed []failure
}

type failure struct {
	path string
	err  error
}

func newJournal(kvd storage.Storage, fingerprint string) *journal {
	return &journal{
		kvd:  kvd,
		key:  key.UploadResume(fingerprint),
		done: make(map[string]struct{}),
	}
}

func (j *journal) load(ctx context.Context) error {
	b, err := j.kvd.Get(ctx, j.key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return errors.Wrap(err, "get upload progress")
	}

	ids := make([]string, 0)
	if err = json.Unmarshal(b, &ids); err != nil {
		return errors.Wrap(err, "unmarshal upload progress")
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	for _, id := range ids {
		j.done[id] = struct{}{}
	}
	return nil
}

func (j *journal) isDone(id string) bool {
	if id == "" {
		return false
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	_, ok := j.done[id]
	return ok
}

// markDone records a successfully uploaded file and persists the record immediately.
func (j *journal) markDone(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	j.done[id] = struct{}{}

	ids := make([]string, 0, len(j.done))
	for k := range j.done {
		ids = append(ids, k)
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return errors.Wrap(err, "marshal upload progress")
	}

	return j.kvd.Set(ctx, j.key, b)
}

func (j *journal) markFailed(path string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.failed = append(j.failed, failure{path: path, err: err})
}

func (j *journal) failures() []failure {
	j.mu.Lock()
	defer j.mu.Unlock()

	failed := make([]failure, len(j.failed))
	copy(failed, j.failed)
	sort.Slice(failed, func(a, b int) bool { return failed[a].path < failed[b].path })
	return failed
}

// clear forgets all uploaded files of this batch.
func (j *journal) clear(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.done = make(map[string]struct{})
	if err := j.kvd.Delete(ctx, j.key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return errors.Wrap(err, "delete upload progress")
	}
	return nil
}

// batchFingerprint identifies an upload batch by its sources and destination, but not by its file list,
// so that adding new files to a directory or removing uploaded ones (--rm) doesn't break resuming.
func batchFingerprint(opts Options) string {
	paths := make([]string, 0, len(opts.Paths))
	for _, p := range opts.Paths {
		paths = append(paths, absPath(p))
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	// destination
	h.Write([]byte{1})
	h.Write([]byte(opts.Chat))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(opts.Thread)))
	h.Write([]byte{0})
	h.Write([]byte(opts.To))

	return hex.EncodeToString(h.Sum(nil))
}

// fileID identifies a file by its path, size and modification time,
// so a file replaced with a different content will be uploaded again.
// Empty string is returned if the file can't be accessed.
func fileID(path string) string {
	stat, err := os.Stat(path)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", absPath(path), stat.Size(), stat.ModTime().UnixNano())))
	return hex.EncodeToString(sum[:16])
}

func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
