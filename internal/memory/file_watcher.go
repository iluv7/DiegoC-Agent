package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileWatcher monitors a directory of markdown memory files (*.md) and
// automatically updates a FileStore index when files are added, modified,
// or removed.
//
// Detection is based on polling (comparing mtimes) with a debounce window
// to avoid repeated reindexing during rapid writes.
type FileWatcher struct {
	watchDir  string
	fileStore *FileStore
	pollDelay time.Duration
	debounce  time.Duration

	mu         sync.Mutex
	mtimes     map[string]time.Time // relPath -> last indexed mtime
	lastChange map[string]time.Time // relPath -> last detected change (debounce)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewFileWatcher creates a FileWatcher that monitors watchDir for *.md files.
// Default pollDelay and debounce are both 2 seconds.
func NewFileWatcher(watchDir string, fileStore *FileStore) *FileWatcher {
	return &FileWatcher{
		watchDir:   filepath.Clean(watchDir),
		fileStore:  fileStore,
		pollDelay:  2 * time.Second,
		debounce:   2 * time.Second,
		mtimes:     make(map[string]time.Time),
		lastChange: make(map[string]time.Time),
	}
}

// Start performs an initial scan of existing *.md files, then starts a
// background goroutine that polls for changes. Stop() must be called to
// clean up the background goroutine.
func (fw *FileWatcher) Start(ctx context.Context) error {
	fw.mu.Lock()
	fw.ctx, fw.cancel = context.WithCancel(ctx)
	fw.mu.Unlock()

	// Initial scan indexes all existing files.
	if err := fw.initialScan(fw.ctx); err != nil {
		fw.cancel()
		return fmt.Errorf("filewatcher: initial scan: %w", err)
	}

	fw.wg.Add(1)
	go fw.pollLoop()

	return nil
}

// Stop cancels the background polling goroutine and waits for it to exit.
// Safe to call multiple times.
func (fw *FileWatcher) Stop() {
	fw.mu.Lock()
	if fw.cancel != nil {
		fw.cancel()
	}
	fw.mu.Unlock()
	fw.wg.Wait()
}

// ---- internal ----

// initialScan indexes all *.md files currently in watchDir.
func (fw *FileWatcher) initialScan(ctx context.Context) error {
	if err := os.MkdirAll(fw.watchDir, 0755); err != nil {
		return err
	}

	pattern := filepath.Join(fw.watchDir, "*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("filewatcher: glob %s: %w", pattern, err)
	}

	for _, fullPath := range matches {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// indexFileUnlocked doesn't touch fw.mu — safe to call here.
		if info, err := fw.indexFileUnlocked(ctx, fullPath); err != nil {
			_ = err // one bad file shouldn't block the scan
		} else if info != nil {
			fw.mu.Lock()
			fw.mtimes[fw.relPath(fullPath)] = info.ModTime()
			fw.mu.Unlock()
		}
	}
	return nil
}

// indexFileUnlocked reads a markdown file, extracts frontmatter, and indexes it
// via the FileStore. It does NOT touch fw.mu. It returns the os.FileInfo so the
// caller can record the mtime under its own lock.
func (fw *FileWatcher) indexFileUnlocked(ctx context.Context, fullPath string) (os.FileInfo, error) {
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("filewatcher: read %s: %w", fullPath, err)
	}

	convDate, body := ParseFrontmatter(string(data))
	relPath := fw.relPath(fullPath)

	if err := fw.fileStore.IndexFileWithDate(ctx, relPath, body, convDate); err != nil {
		return nil, fmt.Errorf("filewatcher: index %s: %w", relPath, err)
	}

	info, statErr := os.Stat(fullPath)
	if statErr != nil {
		return nil, nil
	}
	return info, nil
}

// pollLoop runs the poll-fn on each ticker tick until the context is cancelled.
func (fw *FileWatcher) pollLoop() {
	defer fw.wg.Done()

	ticker := time.NewTicker(fw.pollDelay)
	defer ticker.Stop()

	for {
		select {
		case <-fw.ctx.Done():
			return
		case <-ticker.C:
			fw.poll(fw.ctx)
		}
	}
}

// poll compares the current filesystem state against the cached mtimes and
// triggers index updates for additions, modifications, and deletions.
//
// Lock ordering: fw.mu is acquired first, then fw.fileStore.mu (indirectly via
// IndexFileWithDate / RemoveFile / HasChanged). This is safe because the
// FileStore mu is a different lock.
func (fw *FileWatcher) poll(ctx context.Context) {
	pattern := filepath.Join(fw.watchDir, "*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	// Build the set of currently-present relative paths (no lock needed).
	currentPaths := make(map[string]bool, len(matches))
	fullByRel := make(map[string]string, len(matches))
	for _, fullPath := range matches {
		rel := fw.relPath(fullPath)
		currentPaths[rel] = true
		fullByRel[rel] = fullPath
	}

	fw.mu.Lock()
	defer fw.mu.Unlock()

	now := time.Now()

	// 1. Detect additions and modifications.
	for relPath, fullPath := range fullByRel {
		info, statErr := os.Stat(fullPath)
		if statErr != nil {
			continue
		}
		currentMtime := info.ModTime()

		cachedMtime, known := fw.mtimes[relPath]

		if !known {
			// New file — index it (no lock needed inside).
			if idxInfo, idxErr := fw.indexFileUnlocked(ctx, fullPath); idxErr == nil && idxInfo != nil {
				fw.mtimes[relPath] = idxInfo.ModTime()
			}
			continue
		}

		// Mtime changed — check content hash + debounce before re-indexing.
		if !currentMtime.Equal(cachedMtime) {
			data, readErr := os.ReadFile(fullPath)
			if readErr != nil {
				continue
			}

			if !fw.fileStore.HasChanged(relPath, string(data)) {
				// Content hash unchanged — just update mtime and skip.
				fw.mtimes[relPath] = currentMtime
				continue
			}

			// Debounce: skip if we recently handled a change for this file.
			if last, ok := fw.lastChange[relPath]; ok && now.Sub(last) < fw.debounce {
				continue
			}
			fw.lastChange[relPath] = now

			// Remove old chunks, then re-index.
			_ = fw.fileStore.RemoveFile(ctx, relPath)
			if idxInfo, idxErr := fw.indexFileUnlocked(ctx, fullPath); idxErr == nil && idxInfo != nil {
				fw.mtimes[relPath] = idxInfo.ModTime()
			}
		}
	}

	// 2. Detect deletions — files in mtimes but not on disk any more.
	for relPath := range fw.mtimes {
		if currentPaths[relPath] {
			continue
		}
		_ = fw.fileStore.RemoveFile(ctx, relPath)
		delete(fw.mtimes, relPath)
		delete(fw.lastChange, relPath)
	}
}

// relPath returns the path relative to watchDir for use as a FileStore key.
func (fw *FileWatcher) relPath(fullPath string) string {
	rel, err := filepath.Rel(fw.watchDir, fullPath)
	if err != nil {
		return filepath.Base(fullPath)
	}
	return rel
}
