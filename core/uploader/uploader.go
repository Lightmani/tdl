package uploader

import (
	"context"
	"io"

	"github.com/gabriel-vasile/mimetype"
	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/samber/lo"
	"golang.org/x/sync/errgroup"

	"github.com/iyear/tdl/core/util/fsutil"
	"github.com/iyear/tdl/core/util/mediautil"
)

// MaxPartSize refer to https://core.telegram.org/api/files#uploading-files
const MaxPartSize = 512 * 1024

type Uploader struct {
	opts Options
}

type Options struct {
	Client   *tg.Client
	Threads  int
	Iter     Iter
	Progress Progress
}

func New(o Options) *Uploader {
	return &Uploader{opts: o}
}

func (u *Uploader) Upload(ctx context.Context, limit int) error {
	wg, wgctx := errgroup.WithContext(ctx)
	wg.SetLimit(limit)

	for u.opts.Iter.Next(wgctx) {
		elem := u.opts.Iter.Value()

		wg.Go(func() error {
			u.opts.Progress.OnAdd(elem)

			err := u.safeUpload(wgctx, elem)
			// always report the real result, so the caller can log failures and must not treat them as success
			u.opts.Progress.OnDone(elem, err)

			// Only stop all uploads when the whole batch is canceled (e.g. by user).
			// A failure of one file is reported by Progress.OnDone and must not abort the others.
			if err != nil && ctx.Err() != nil {
				return errors.Wrap(err, "upload")
			}

			return nil
		})
	}

	// wait for in-flight uploads first, so that all of their results are reported before returning
	werr := wg.Wait()
	if err := u.opts.Iter.Err(); err != nil {
		return errors.Wrap(err, "iter")
	}

	return werr
}

// safeUpload isolates a panic caused by a single (malformed) file from the whole batch.
func (u *Uploader) safeUpload(ctx context.Context, elem Elem) (rerr error) {
	defer func() {
		if r := recover(); r != nil {
			rerr = errors.Errorf("panic: %v", r)
		}
	}()

	return u.upload(ctx, elem)
}

func (u *Uploader) upload(ctx context.Context, elem Elem) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	up := uploader.NewUploader(u.opts.Client).
		WithPartSize(MaxPartSize).
		WithThreads(u.opts.Threads).
		WithProgress(&wrapProcess{
			elem:    elem,
			process: u.opts.Progress,
		})

	f, err := up.Upload(ctx, uploader.NewUpload(elem.File().Name(), elem.File(), elem.File().Size()))
	if err != nil {
		return errors.Wrap(err, "upload file")
	}

	if _, err = elem.File().Seek(0, io.SeekStart); err != nil {
		return errors.Wrap(err, "seek file")
	}
	mime, err := mimetype.DetectReader(elem.File())
	if err != nil {
		return errors.Wrap(err, "detect mime")
	}

	// here convert underlying entities to formatters for message caption
	caption := styling.Custom(func(eb *entity.Builder) error {
		msg, entities := elem.Caption()
		eb.Format(msg, lo.Map(entities, func(item tg.MessageEntityClass, _ int) entity.Formatter {
			return func(_, _ int) tg.MessageEntityClass {
				return item
			}
		})...)
		return nil
	})

	doc := message.UploadedDocument(f, caption).MIME(mime.String()).Filename(elem.File().Name())
	// upload thumbnail TODO(iyear): maybe still unavailable
	if thumb, ok := elem.Thumb(); ok {
		if thumbFile, err := uploader.NewUploader(u.opts.Client).
			FromReader(ctx, thumb.Name(), thumb); err == nil {
			doc = doc.Thumb(thumbFile)
		}
	}

	var media message.MediaOption = doc

	switch {
	case mediautil.IsImage(mime.String()) && elem.AsPhoto():
		// webp should be uploaded as document
		if mime.String() == "image/webp" {
			break
		}
		// upload as photo
		media = message.UploadedPhoto(f, caption)
	case mediautil.IsVideo(mime.String()):
		// Any codec (H.264, HEVC, AV1, VP9...) in MP4/MOV/MKV/WebM is sent as a video, so it can be previewed.
		// #132. If the container can't be parsed (e.g. AVI, broken file), we still upload it, but as a file.
		if info, err := mediautil.GetVideoInfo(elem.File()); err == nil {
			video := doc.Video().
				Duration(info.Duration).
				Resolution(info.Width, info.Height)
			if info.Streamable {
				video = video.SupportsStreaming()
			}
			media = video
		}
	case mediautil.IsAudio(mime.String()):
		media = doc.Audio().Title(fsutil.GetNameWithoutExt(elem.File().Name()))
	}

	_, err = message.NewSender(u.opts.Client).
		WithUploader(up).
		To(elem.To()).
		Reply(elem.Thread()).
		Media(ctx, media)
	if err != nil {
		return errors.Wrap(err, "send message")
	}

	return nil
}
