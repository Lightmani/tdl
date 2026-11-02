package up

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/expr-lang/expr/vm"
	"github.com/gabriel-vasile/mimetype"
	"github.com/go-faster/errors"
	"github.com/go-viper/mapstructure/v2"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/peers"

	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/core/util/mediautil"
	"github.com/iyear/tdl/core/util/tutil"
	"github.com/iyear/tdl/pkg/texpr"
)

type File struct {
	File  string
	Thumb string

	id string // identity for resuming, see fileID
}

type dest struct {
	Peer   string
	Thread int
}

type iter struct {
	files   []*File
	to      *vm.Program
	caption *vm.Program
	chat    peers.Peer // pre-resolved --chat, nil if not set
	topic   int
	photo   bool
	remove  bool
	delay   time.Duration
	manager *peers.Manager
	// onError is called when a file can't be prepared for uploading. The file is skipped, others go on.
	onError func(file *File, err error)

	cur  int
	err  error
	file uploader.Elem
}

func newIter(files []*File, to, caption *vm.Program, chat peers.Peer, topic int, photo, remove bool, delay time.Duration, manager *peers.Manager, onError func(file *File, err error)) *iter {
	return &iter{
		files:   files,
		to:      to,
		caption: caption,
		chat:    chat,
		topic:   topic,
		photo:   photo,
		remove:  remove,
		delay:   delay,
		manager: manager,
		onError: onError,

		cur:  0,
		err:  nil,
		file: nil,
	}
}

func (i *iter) Next(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			i.err = ctx.Err()
			return false
		default:
		}

		if i.cur >= len(i.files) || i.err != nil {
			return false
		}

		// if delay is set, sleep for a while for each iteration
		if i.delay > 0 && i.cur > 0 { // skip first delay
			time.Sleep(i.delay)
		}

		cur := i.files[i.cur]
		i.cur++

		file, err := i.next(ctx, cur)
		if err != nil {
			if ctx.Err() != nil { // canceled, stop the whole batch
				i.err = ctx.Err()
				return false
			}

			// one broken file must not stop the whole batch, report it and go on
			if i.onError != nil {
				i.onError(cur, err)
			}
			continue
		}

		i.file = file
		return true
	}
}

func (i *iter) next(ctx context.Context, cur *File) (*iterElem, error) {
	env := exprEnv(ctx, cur)

	to, thread, err := i.resolveDest(ctx, env)
	if err != nil {
		return nil, errors.Wrap(err, "resolve destination")
	}

	caption, err := i.resolveCaption(env)
	if err != nil {
		return nil, errors.Wrap(err, "resolve caption")
	}

	// open files at last, so they don't leak when other resolving fails
	file, err := i.resolveFile(cur.File)
	if err != nil {
		return nil, errors.Wrap(err, "resolve file")
	}

	thumb, err := i.resolveThumb(cur.Thumb)
	if err != nil {
		_ = file.Close()
		return nil, errors.Wrap(err, "resolve thumbnail")
	}

	return &iterElem{
		id:      cur.id,
		file:    file,
		thumb:   thumb,
		to:      to,
		caption: caption,
		thread:  thread,

		asPhoto: i.photo,
		remove:  i.remove,
	}, nil
}

func (i *iter) resolveFile(path string) (*uploaderFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "open file")
	}

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "stat file")
	}

	return &uploaderFile{
		File: f,
		size: stat.Size(),
	}, nil
}

func (i *iter) resolveDest(ctx context.Context, env Env) (peers.Peer, int, error) {
	if i.chat != nil { // compatible with old version
		return i.chat, i.topic, nil
	}

	// message routing
	result, err := texpr.Run(i.to, env)
	if err != nil {
		return nil, 0, errors.Wrap(err, "parse expression")
	}

	var (
		to     peers.Peer
		thread int
	)

	switch r := result.(type) {
	case string:
		// pure chat, no reply to, which is a compatible with old version
		// and a convenient way to send message to self
		to, err = i.resolvePeer(ctx, r)
	case map[string]interface{}:
		// chat with reply to topic or message
		var d dest

		if err = mapstructure.WeakDecode(r, &d); err != nil {
			return nil, 0, errors.Wrapf(err, "decode dest: %v", result)
		}

		to, err = i.resolvePeer(ctx, d.Peer)
		thread = d.Thread
	default:
		return nil, 0, errors.Errorf("message router must return string or dest: %T", result)
	}

	if err != nil {
		return nil, 0, errors.Wrap(err, "resolve peer")
	}

	return to, thread, nil
}

func (i *iter) resolvePeer(ctx context.Context, peer string) (peers.Peer, error) {
	if peer == "" { // self
		return i.manager.Self(ctx)
	}

	return tutil.GetInputPeer(ctx, i.manager, peer)
}

func (i *iter) resolveCaption(env Env) (*entity.Builder, error) {
	// parse caption
	captionStr, err := texpr.Run(i.caption, env)
	if err != nil {
		return nil, errors.Wrap(err, "parse caption")
	}

	r, ok := captionStr.(string)
	if !ok {
		return nil, errors.Errorf("caption must return string, got %T", captionStr)
	}

	caption := &entity.Builder{}
	if len(r) > 0 {
		if err = html.HTML(strings.NewReader(r), caption, html.Options{
			UserResolver:          nil,
			DisableTelegramEscape: false,
		}); err != nil {
			return nil, errors.Wrap(err, "parse caption HTML")
		}
	}

	return caption, nil
}

func (i *iter) resolveThumb(path string) (*uploaderFile, error) {
	if path == "" {
		return nil, nil
	}

	// has thumbnail
	mime, err := mimetype.DetectFile(path)
	if err != nil || !mediautil.IsImage(mime.String()) { // TODO(iyear): jpg only
		return nil, errors.Wrapf(err, "invalid thumbnail file: %v", path)
	}

	thumb, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "open thumbnail file")
	}

	return &uploaderFile{
		File: thumb,
		size: 0,
	}, nil
}

func (i *iter) Value() uploader.Elem {
	return i.file
}

func (i *iter) Err() error {
	return i.err
}
