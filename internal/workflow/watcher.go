package workflow

import (
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

var ErrWatcherAlreadyStarted = errors.New("watcher_already_started")

const (
	defaultPollInterval = 1 * time.Second
	defaultDebounce     = 200 * time.Millisecond
)

type ChangeEvent struct {
	Path   string
	Source string
	Op     fsnotify.Op
}

type Watcher struct {
	filePath string
	dirPath  string
	fileName string
	watcher  *fsnotify.Watcher

	mu      sync.Mutex
	started bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	lastPollStamp    filePollStamp
	hasLastPollStamp bool
}

type filePollStamp struct {
	modTimeUnixNano int64
	sizeBytes       int64
	contentHash     uint64
}

func NewWatcher(path string) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	cleanPath := filepath.Clean(path)
	return &Watcher{
		filePath: cleanPath,
		dirPath:  filepath.Dir(cleanPath),
		fileName: filepath.Base(cleanPath),
		watcher:  w,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}, nil
}

func (w *Watcher) Close() error {
	if w == nil || w.watcher == nil {
		return nil
	}

	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return w.watcher.Close()
	}
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
	w.mu.Unlock()

	<-w.doneCh
	return w.watcher.Close()
}

func (w *Watcher) Start(onChange func(ChangeEvent)) error {
	if w == nil || w.watcher == nil {
		return errors.New("nil watcher")
	}
	if onChange == nil {
		onChange = func(ChangeEvent) {}
	}

	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return ErrWatcherAlreadyStarted
	}
	w.started = true
	w.mu.Unlock()

	if err := w.watcher.Add(w.dirPath); err != nil {
		w.mu.Lock()
		w.started = false
		w.mu.Unlock()
		return err
	}

	go w.loop(onChange)
	return nil
}

func (w *Watcher) loop(onChange func(ChangeEvent)) {
	defer close(w.doneCh)

	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	var (
		debounceTimer *time.Timer
		pending       *ChangeEvent
	)

	stopDebounce := func() {
		if debounceTimer == nil {
			return
		}
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
	}

	schedule := func(event ChangeEvent) {
		pending = &event
		if debounceTimer == nil {
			debounceTimer = time.NewTimer(defaultDebounce)
			return
		}
		stopDebounce()
		debounceTimer.Reset(defaultDebounce)
	}

	flush := func() {
		if pending == nil {
			return
		}
		event := *pending
		pending = nil
		onChange(event)
	}

	for {
		var debounceC <-chan time.Time
		if debounceTimer != nil {
			debounceC = debounceTimer.C
		}

		select {
		case <-w.stopCh:
			stopDebounce()
			return
		case <-ticker.C:
			if w.fileChangedByPoll() {
				schedule(ChangeEvent{
					Path:   w.filePath,
					Source: "poll",
					Op:     fsnotify.Write,
				})
			}
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if !w.matchesTarget(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Chmod|fsnotify.Remove) == 0 {
				continue
			}
			// editors often replace files atomically; keeping dir watch handles it,
			// and polling catches missed transitions.
			schedule(ChangeEvent{
				Path:   w.filePath,
				Source: "fsnotify",
				Op:     event.Op,
			})
		case <-w.watcher.Errors:
			// keep running; polling fallback still detects changes.
		case <-debounceC:
			flush()
		}
	}
}

func (w *Watcher) matchesTarget(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	cleanName := filepath.Clean(name)
	return cleanName == w.filePath || filepath.Base(cleanName) == w.fileName
}

func (w *Watcher) fileChangedByPoll() bool {
	stat, err := os.Stat(w.filePath)
	if err != nil {
		return false
	}

	content, err := os.ReadFile(w.filePath)
	if err != nil {
		return false
	}

	hasher := fnv.New64a()
	_, _ = hasher.Write(content)
	stamp := filePollStamp{
		modTimeUnixNano: stat.ModTime().UTC().UnixNano(),
		sizeBytes:       stat.Size(),
		contentHash:     hasher.Sum64(),
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.started {
		return false
	}
	if !w.hasLastPollStamp {
		w.lastPollStamp = stamp
		w.hasLastPollStamp = true
		return false
	}
	if w.lastPollStamp == stamp {
		return false
	}
	w.lastPollStamp = stamp
	return true
}
