package config

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher hot-reloads the routing table when its file changes on disk.
//
// Two details make this work in practice rather than only in a demo:
//
//  1. It watches the *directory*, not the file. Editors and `kubectl`-managed
//     ConfigMaps do not write in place; they write a temp file and rename it
//     over the target, or swap a symlink. An inotify watch on the old inode
//     survives that rename and then never fires again, so a file watch appears
//     to work once and then goes silent forever.
//
//  2. A bad edit is rejected, not applied. The new table is parsed and
//     validated before it is published, so a typo leaves the gateway serving
//     the last known-good config instead of taking production down.
type Watcher struct {
	path     string
	log      *slog.Logger
	debounce time.Duration
	onChange func(*Routes)
	current  string // checksum of the table currently applied
}

// NewWatcher builds a watcher for path. onChange is called with each new valid
// table, and never with an invalid one.
func NewWatcher(path string, initial *Routes, log *slog.Logger, onChange func(*Routes)) *Watcher {
	w := &Watcher{
		path:     path,
		log:      log,
		debounce: 250 * time.Millisecond,
		onChange: onChange,
	}
	if initial != nil {
		w.current = initial.Checksum()
	}
	return w
}

// Run blocks until ctx is cancelled, reloading on every change.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	abs, err := filepath.Abs(w.path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	if err := fsw.Add(dir); err != nil {
		return err
	}
	w.log.Info("watching routing config for changes", "path", abs, "dir", dir)

	// A single save often produces several events (CREATE, WRITE, CHMOD). The
	// timer collapses that burst into one reload, and also avoids reading a
	// file an editor is still halfway through writing.
	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()

		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(event.Name) != abs {
				continue // some other file in the directory
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Chmod) == 0 {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(w.debounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
			}

		case <-timerC:
			w.reload()

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.log.Error("routing config watcher error", "error", err)
		}
	}
}

func (w *Watcher) reload() {
	next, err := LoadRoutes(w.path)
	if err != nil {
		// Keep serving the previous table. A broken edit must not be able to
		// black-hole live traffic.
		w.log.Error("routing config reload rejected, keeping previous config",
			"path", w.path, "error", err)
		return
	}
	if next.Checksum() == w.current {
		w.log.Debug("routing config unchanged, skipping reload", "checksum", next.Checksum()[:12])
		return
	}

	prev := w.current
	w.current = next.Checksum()
	w.onChange(next)

	prevShort := "none"
	if prev != "" {
		prevShort = prev[:12]
	}
	w.log.Info("routing config reloaded",
		"path", w.path,
		"providers", len(next.Providers),
		"routes", len(next.Routes),
		"from_checksum", prevShort,
		"to_checksum", w.current[:12],
	)
}
