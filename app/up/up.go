package up

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/fatih/color"
	"github.com/gabriel-vasile/mimetype"
	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/peers"
	"github.com/spf13/viper"
	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/logctx"
	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/core/tclient"
	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/core/util/tutil"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/iyear/tdl/pkg/prog"
	"github.com/iyear/tdl/pkg/texpr"
	"github.com/iyear/tdl/pkg/utils"
)

// DefaultCaption is the file name, so each uploaded video shows its name below it.
const DefaultCaption = `FileName + FileExt`

type Options struct {
	Chat     string
	Thread   int
	To       string
	Paths    []string
	Includes []string
	Excludes []string
	Remove   bool
	Photo    bool
	Caption  string

	// resume opts, if both false then ask user
	Continue, Restart bool
}

type Env struct {
	FilePath  string `comment:"File path"`
	FileName  string `comment:"File name"`
	FileExt   string `comment:"File extension"`
	ThumbPath string `comment:"Thumbnail path"`
	MIME      string `comment:"File mime type"`
}

func Run(ctx context.Context, c *telegram.Client, kvd storage.Storage, opts Options) (rerr error) {
	if opts.To == "-" || opts.Caption == "-" {
		fg := texpr.NewFieldsGetter(nil)

		fields, err := fg.Walk(exprEnv(context.Background(), nil))
		if err != nil {
			return fmt.Errorf("failed to walk fields: %w", err)
		}

		fmt.Print(fg.Sprint(fields, true))
		return nil
	}

	files, err := walk(opts.Paths, opts.Includes, opts.Excludes)
	if err != nil {
		return err
	}

	color.Blue("Files count: %d", len(files))

	pool := dcpool.NewPool(c,
		int64(viper.GetInt(consts.FlagPoolSize)),
		tclient.NewDefaultMiddlewares(ctx, viper.GetDuration(consts.FlagReconnectTimeout))...)
	defer multierr.AppendInvoke(&rerr, multierr.Close(pool))

	manager := peers.Options{Storage: storage.NewPeers(kvd)}.Build(pool.Default(ctx))

	to, err := resolveDest(ctx, manager, opts.To)
	if err != nil {
		return errors.Wrap(err, "get target peer")
	}

	caption, err := resolveCaption(ctx, opts.Caption)
	if err != nil {
		return errors.Wrap(err, "get caption")
	}

	// resolve the fixed chat once, so a wrong chat fails fast instead of failing every file
	var chat peers.Peer
	if opts.Chat != "" {
		if chat, err = tutil.GetInputPeer(ctx, manager, opts.Chat); err != nil {
			return errors.Wrap(err, "resolve chat")
		}
	}

	jr := newJournal(kvd, batchFingerprint(opts))
	if files, err = resume(ctx, jr, files, opts); err != nil {
		return errors.Wrap(err, "resume")
	}
	if len(files) == 0 {
		color.Green("All files have been uploaded")
		return jr.clear(ctx)
	}

	upProgress := prog.New(utils.Byte.FormatBinaryBytes)
	upProgress.SetNumTrackersExpected(len(files))
	if !viper.GetBool(consts.FlagDisableProgressPS) {
		prog.EnablePS(ctx, upProgress)
	}

	onError := func(f *File, err error) {
		upProgress.Log(color.RedString("%s error: %s", f.File, err.Error()))
		jr.markFailed(f.File, err)
	}

	options := uploader.Options{
		Client:   pool.Default(ctx),
		Threads:  viper.GetInt(consts.FlagThreads),
		Iter:     newIter(files, to, caption, chat, opts.Thread, opts.Photo, opts.Remove, viper.GetDuration(consts.FlagDelay), manager, onError),
		Progress: newProgress(ctx, upProgress, jr),
	}

	up := uploader.New(options)

	go upProgress.Render()

	uerr := up.Upload(ctx, viper.GetInt(consts.FlagLimit))
	prog.Wait(ctx, upProgress)

	return summarize(ctx, jr, uerr)
}

// resume filters out files uploaded by the last unfinished batch.
func resume(ctx context.Context, jr *journal, files []*File, opts Options) ([]*File, error) {
	for _, f := range files {
		f.id = fileID(f.File)
	}

	if opts.Restart {
		color.Yellow("Restart upload by 'restart' flag")
		return files, jr.clear(ctx)
	}

	if err := jr.load(ctx); err != nil {
		return nil, err
	}

	pending := make([]*File, 0, len(files))
	for _, f := range files {
		if !jr.isDone(f.id) {
			pending = append(pending, f)
		}
	}

	skipped := len(files) - len(pending)
	if skipped == 0 {
		return files, nil
	}

	resumeStr := fmt.Sprintf("Found unfinished upload, %d/%d files have been uploaded", skipped, len(files))
	confirm := true
	if opts.Continue {
		color.Yellow(resumeStr + ", skip them")
	} else if err := survey.AskOne(&survey.Confirm{
		Message: color.YellowString(resumeStr + ", skip them and continue?"),
		Default: true,
	}, &confirm); err != nil {
		return nil, err
	}

	if !confirm {
		return files, jr.clear(ctx)
	}

	logctx.From(ctx).Debug("Resume upload",
		zap.Int("skipped", skipped),
		zap.Int("pending", len(pending)))
	return pending, nil
}

// summarize reports failed files and keeps the progress for resuming unless all files are uploaded.
func summarize(ctx context.Context, jr *journal, uerr error) error {
	failed := jr.failures()
	if uerr == nil && len(failed) == 0 {
		return jr.clear(ctx)
	}

	if len(failed) > 0 {
		color.Red("%d file(s) failed to upload:", len(failed))
		for _, f := range failed {
			color.Red("  %s: %s", f.path, f.err.Error())
		}
	}
	color.Yellow("Uploaded files are recorded. Run the same command again to upload the rest, " +
		"add '--continue' to skip the confirmation, or '--restart' to upload all files again.")

	if uerr != nil {
		return uerr
	}
	return errors.Errorf("%d file(s) failed to upload", len(failed))
}

func resolveDest(ctx context.Context, manager *peers.Manager, input string) (*vm.Program, error) {
	compile := func(i string) (*vm.Program, error) {
		return expr.Compile(i, expr.Env(exprEnv(ctx, nil)))
	}

	if input == "" {
		return compile(`""`)
	}

	if exp, err := os.ReadFile(input); err == nil {
		return compile(string(exp))
	}

	if _, err := tutil.GetInputPeer(ctx, manager, input); err == nil {
		return compile(fmt.Sprintf(`"%s"`, input))
	}

	return compile(input)
}

func resolveCaption(ctx context.Context, input string) (*vm.Program, error) {
	compile := func(i string) (*vm.Program, error) {
		// we pass empty peer and message to enable type checking
		return expr.Compile(i, expr.Env(exprEnv(ctx, nil)), expr.AsKind(reflect.String))
	}

	// default
	if input == "" {
		return compile(`""`)
	}

	// file
	if exp, err := os.ReadFile(input); err == nil {
		return compile(string(exp))
	}

	// text
	return compile(input)
}

func exprEnv(ctx context.Context, file *File) Env {
	if file == nil {
		return Env{}
	}

	extension := filepath.Ext(file.File)
	filename := strings.TrimSuffix(filepath.Base(file.File), extension)
	mime, err := mimetype.DetectFile(file.File)
	if err != nil {
		mime = &mimetype.MIME{}
		logctx.From(ctx).Error("detect file mime", zap.Error(err))
	}

	return Env{
		FilePath:  file.File,
		FileName:  filename,
		FileExt:   extension,
		ThumbPath: file.Thumb,
		MIME:      mime.String(),
	}
}
