package up

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/fatih/color"
	"github.com/go-faster/errors"
	pw "github.com/jedib0t/go-pretty/v6/progress"

	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/pkg/prog"
	"github.com/iyear/tdl/pkg/utils"
)

type progress struct {
	ctx      context.Context // context of the whole batch, used to tell user cancellation from failures
	pw       pw.Writer
	trackers *sync.Map // map[tuple]*pw.Tracker
	journal  *journal
}

type tuple struct {
	name string
	to   int64
}

func newProgress(ctx context.Context, p pw.Writer, j *journal) *progress {
	return &progress{
		ctx:      ctx,
		pw:       p,
		trackers: &sync.Map{},
		journal:  j,
	}
}

func (p *progress) OnAdd(elem uploader.Elem) {
	tracker := prog.AppendTracker(p.pw, utils.Byte.FormatBinaryBytes, p.processMessage(elem), elem.File().Size())
	p.trackers.Store(p.tuple(elem), tracker)
}

func (p *progress) OnUpload(elem uploader.Elem, state uploader.ProgressState) {
	tracker, ok := p.trackers.Load(p.tuple(elem))
	if !ok {
		return
	}

	t := tracker.(*pw.Tracker)
	t.UpdateTotal(state.Total)
	t.SetValue(state.Uploaded)
}

func (p *progress) OnDone(elem uploader.Elem, err error) {
	e := elem.(*iterElem)
	closeErr := p.closeFile(e) // always close files, even if upload failed

	tracker, ok := p.trackers.Load(p.tuple(elem))
	if !ok {
		return
	}
	t := tracker.(*pw.Tracker)

	if err != nil {
		if p.ctx.Err() != nil { // canceled by user, it will be uploaded when resuming
			return
		}

		p.fail(t, elem, errors.Wrap(err, "upload"))
		p.journal.markFailed(e.file.File.Name(), err)
		return
	}

	// uploaded successfully, persist it at once, so it can be skipped even if tdl is killed later
	if err := p.journal.markDone(context.WithoutCancel(p.ctx), e.id); err != nil {
		p.pw.Log(color.YellowString("%s warn: save upload progress: %s", p.elemString(elem), err.Error()))
	}

	if closeErr != nil {
		p.fail(t, elem, closeErr)
		return
	}

	if e.remove {
		if err := os.Remove(e.file.File.Name()); err != nil {
			p.fail(t, elem, errors.Wrap(err, "remove file"))
			return
		}
	}
}

func (p *progress) closeFile(e *iterElem) error {
	if err := e.file.Close(); err != nil {
		return errors.Wrap(err, "close file")
	}

	if e.thumb != nil {
		if err := e.thumb.Close(); err != nil {
			return errors.Wrap(err, "close thumb")
		}
	}

	return nil
}

func (p *progress) fail(t *pw.Tracker, elem uploader.Elem, err error) {
	p.pw.Log(color.RedString("%s error: %s", p.elemString(elem), err.Error()))
	t.MarkAsErrored()
}

func (p *progress) tuple(elem uploader.Elem) tuple {
	return tuple{elem.(*iterElem).file.File.Name(), elem.(*iterElem).to.ID()}
}

func (p *progress) processMessage(elem uploader.Elem) string {
	return p.elemString(elem)
}

func (p *progress) elemString(elem uploader.Elem) string {
	e := elem.(*iterElem)
	return fmt.Sprintf("%s -> %s(%d)", e.file.File.Name(), e.to.VisibleName(), e.to.ID())
}
