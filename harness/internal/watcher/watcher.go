package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/konveyor/migration-harness/internal/logging"
)

// DefaultQuietPeriod is the debounce window: after the last relevant
// filesystem event, the watcher waits this long before pushing.
const DefaultQuietPeriod = 30 * time.Second

type PushFn func() error

type Watcher struct {
	dir         string
	pushFn      PushFn
	fsw         *fsnotify.Watcher
	quietPeriod time.Duration
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	stopOnce    sync.Once
}

func New(dir string, pushFn PushFn) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		dir:         dir,
		pushFn:      pushFn,
		fsw:         fsw,
		quietPeriod: DefaultQuietPeriod,
	}, nil
}

// WithQuietPeriod sets a custom debounce period (for testing).
func (w *Watcher) WithQuietPeriod(d time.Duration) *Watcher {
	w.quietPeriod = d
	return w
}

func (w *Watcher) Start(ctx context.Context) error {
	if err := w.addDirRecursive(w.dir); err != nil {
		return err
	}

	ctx, w.cancel = context.WithCancel(ctx)
	w.wg.Add(1)
	go w.loop(ctx)
	logging.Info("filesystem watcher started (quiet period: %s)", w.quietPeriod)
	return nil
}

func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
		w.wg.Wait()
		w.fsw.Close()
	})
}

func (w *Watcher) loop(ctx context.Context) {
	defer w.wg.Done()
	timer := time.NewTimer(w.quietPeriod)
	timer.Stop()
	dirty := false

	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			rel, err := filepath.Rel(w.dir, event.Name)
			if err != nil {
				continue
			}
			if !isRelevantChange(rel) {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := w.addDirRecursive(event.Name); err != nil {
						logging.Warn("watch new directory %s: %v", event.Name, err)
					}
				}
			}
			dirty = true
			timer.Reset(w.quietPeriod)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			logging.Warn("watcher error: %v", err)
		case <-timer.C:
			if dirty {
				w.doPush()
				dirty = false
			}
		}
	}
}

func isRelevantChange(relPath string) bool {
	// Check if the path itself or any of its directory components are excluded
	parts := strings.Split(relPath, string(filepath.Separator))
	for _, part := range parts {
		if excludeDirs[part] {
			return false
		}
	}

	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	return !excludeExts[ext]
}

func (w *Watcher) doPush() {
	if err := w.pushFn(); err != nil {
		logging.Warn("watcher push: %v", err)
	} else {
		logging.Ok("watcher push: success")
	}
}

func (w *Watcher) addDirRecursive(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if excludeDirs[name] {
				return filepath.SkipDir
			}
			if err := w.fsw.Add(path); err != nil {
				logging.Warn("watcher: skipping %s: %v", path, err)
				return nil
			}
			return nil
		}
		return nil
	})
}
