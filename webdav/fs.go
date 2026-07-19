package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"eidos/fuse/core"

	"golang.org/x/net/webdav"
)

type EidosFS struct {
	client       *core.Client
	mu           sync.Mutex
	tempFiles    map[string][]byte   // path → content for .sb-* temp files
	tempDirs     map[string]struct{} // path → exists for .sb-* temp dirs
	deletedPaths map[string]struct{} // real paths backed up for safe-save; stat returns 404
}

var _ webdav.FileSystem = (*EidosFS)(nil)

func eidosPath(p string) string {
	return strings.Trim(p, "/")
}

// isMacOSTemp returns true if any path segment contains .sb-, meaning it is
// inside a macOS safe-save temp directory (e.g. text.txt.sb-XXXXXXXX-YYYY/).
func isMacOSTemp(p string) bool {
	return strings.Contains(p, ".sb-")
}

func (f *EidosFS) tempFileGet(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.tempFiles[path]
	return b, ok
}

func (f *EidosFS) tempFileSet(path string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFiles == nil {
		f.tempFiles = make(map[string][]byte)
	}
	f.tempFiles[path] = b
}

func (f *EidosFS) tempDirSet(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempDirs == nil {
		f.tempDirs = make(map[string]struct{})
	}
	f.tempDirs[path] = struct{}{}
}

func (f *EidosFS) tempClean(prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tempDirs, prefix)
	for k := range f.tempFiles {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			delete(f.tempFiles, k)
		}
	}
}

func (f *EidosFS) markDeleted(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletedPaths == nil {
		f.deletedPaths = make(map[string]struct{})
	}
	f.deletedPaths[p] = struct{}{}
}

func (f *EidosFS) unmarkDeleted(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.deletedPaths, p)
}

func (f *EidosFS) isDeleted(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.deletedPaths[p]
	return ok
}

func (f *EidosFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	p := eidosPath(name)
	if isMacOSTemp(p) {
		f.tempDirSet(p)
		return nil
	}
	return f.client.Mkdir(p)
}

func (f *EidosFS) RemoveAll(ctx context.Context, name string) error {
	p := eidosPath(name)
	if isMacOSTemp(p) {
		f.tempClean(p)
		return nil
	}
	// If we virtually-deleted this path for safe-save, it's already gone from Eidos.
	if f.isDeleted(p) {
		return os.ErrNotExist
	}
	return f.client.Delete(p)
}

func (f *EidosFS) Rename(ctx context.Context, oldName, newName string) error {
	oldP := eidosPath(oldName)
	newP := eidosPath(newName)

	// Case 1: temp → real  (safe-save commit: promote new content)
	if isMacOSTemp(oldP) {
		content, _ := f.tempFileGet(oldP)
		f.tempClean(oldP)
		f.unmarkDeleted(newP) // clear backup marker now that we're committing
		return f.client.Write(newP, content, core.MimeFromName(newP))
	}

	// Case 2: real → temp  (safe-save backup: park original in memory)
	// DO NOT call the Eidos API rename here — the API's normaliseWritePath would
	// mangle the .sb-* name and leave text.txt still accessible, so the subsequent
	// MOVE temp→real would see "file already exists" and fail.
	if isMacOSTemp(newP) {
		content, _ := f.client.Read(oldP) // best-effort; nil is fine if not found
		f.tempFileSet(newP, content)
		f.markDeleted(oldP) // make stat/RemoveAll return ErrNotExist for oldP
		return nil
	}

	return f.client.Rename(oldP, newP)
}

func (f *EidosFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	p := eidosPath(name)
	if p == "" {
		return &fileInfo{name: "/", isDir: true}, nil
	}

	if isMacOSTemp(p) {
		f.mu.Lock()
		_, isDir := f.tempDirs[p]
		content, isFile := f.tempFiles[p]
		f.mu.Unlock()
		if isDir {
			return &fileInfo{name: filepath.Base(p), isDir: true}, nil
		}
		if isFile {
			return &fileInfo{name: filepath.Base(p), size: int64(len(content))}, nil
		}
		return nil, os.ErrNotExist
	}

	// Path was backed-up in memory for safe-save; treat as gone until committed.
	if f.isDeleted(p) {
		return nil, os.ErrNotExist
	}

	meta, err := f.client.Stat(p)
	if err != nil {
		if err == core.ErrNotFound {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return metaToFileInfo(meta), nil
}

func (f *EidosFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	p := eidosPath(name)

	if isMacOSTemp(p) {
		if flag&os.O_CREATE != 0 {
			return &tempFile{fs: f, path: p}, nil
		}
		f.mu.Lock()
		_, isDir := f.tempDirs[p]
		content, isFile := f.tempFiles[p]
		// For directories, collect direct children from tempFiles.
		var entries []core.FileMeta
		if isDir {
			prefix := p + "/"
			for k, v := range f.tempFiles {
				if strings.HasPrefix(k, prefix) {
					child := k[len(prefix):]
					if !strings.Contains(child, "/") {
						entries = append(entries, core.FileMeta{
							Name: child,
							Type: "file",
							Size: int64(len(v)),
						})
					}
				}
			}
		}
		f.mu.Unlock()
		if isDir {
			return &dirFile{name: filepath.Base(p), entries: entries}, nil
		}
		if isFile {
			return &tempReadFile{name: filepath.Base(p), content: content}, nil
		}
		return nil, os.ErrNotExist
	}

	if flag&os.O_CREATE != 0 {
		return &writeFile{client: f.client, path: p, mime: core.MimeFromName(p)}, nil
	}

	if p == "" {
		entries, err := f.client.List("")
		if err != nil {
			return nil, err
		}
		return &dirFile{name: "/", entries: entries}, nil
	}

	meta, err := f.client.Stat(p)
	if err != nil {
		if err == core.ErrNotFound {
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	if meta.Type == "folder" {
		entries, err := f.client.List(p)
		if err != nil {
			return nil, err
		}
		return &dirFile{name: meta.Name, entries: entries}, nil
	}

	return &readFile{client: f.client, path: p, meta: meta}, nil
}

// ── tempFile ──────────────────────────────────────────────────────────────────
// Buffers writes in memory; on Close stores to EidosFS.tempFiles.

type tempFile struct {
	fs     *EidosFS
	path   string
	mu     sync.Mutex
	buf    []byte
	offset int64
}

func (f *tempFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	end := int(f.offset) + len(p)
	if end > len(f.buf) {
		nb := make([]byte, end)
		copy(nb, f.buf)
		f.buf = nb
	}
	copy(f.buf[f.offset:], p)
	f.offset += int64(len(p))
	return len(p), nil
}

func (f *tempFile) Close() error {
	f.fs.tempFileSet(f.path, f.buf)
	return nil
}

func (f *tempFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		f.offset = int64(len(f.buf)) + offset
	}
	return f.offset, nil
}

func (f *tempFile) Read([]byte) (int, error)          { return 0, os.ErrInvalid }
func (f *tempFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (f *tempFile) Stat() (os.FileInfo, error)         { return &fileInfo{name: filepath.Base(f.path)}, nil }

// ── tempReadFile ──────────────────────────────────────────────────────────────

type tempReadFile struct {
	name    string
	content []byte
	offset  int64
}

func (f *tempReadFile) Read(p []byte) (int, error) {
	if f.offset >= int64(len(f.content)) {
		return 0, io.EOF
	}
	n := copy(p, f.content[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *tempReadFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = int64(len(f.content)) + offset
	}
	if abs < 0 {
		return 0, os.ErrInvalid
	}
	f.offset = abs
	return abs, nil
}

func (f *tempReadFile) Close() error                             { return nil }
func (f *tempReadFile) Write([]byte) (int, error)                { return 0, os.ErrInvalid }
func (f *tempReadFile) Readdir(int) ([]os.FileInfo, error)       { return nil, os.ErrInvalid }
func (f *tempReadFile) Stat() (os.FileInfo, error) {
	return &fileInfo{name: f.name, size: int64(len(f.content))}, nil
}

// ── readFile ──────────────────────────────────────────────────────────────────

type readFile struct {
	client  *core.Client
	path    string
	meta    *core.FileMeta
	once    sync.Once
	content []byte
	err     error
	offset  int64
}

func (f *readFile) load() {
	f.once.Do(func() { f.content, f.err = f.client.Read(f.path) })
}

func (f *readFile) Read(p []byte) (int, error) {
	f.load()
	if f.err != nil {
		return 0, f.err
	}
	if f.offset >= int64(len(f.content)) {
		return 0, io.EOF
	}
	n := copy(p, f.content[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *readFile) Seek(offset int64, whence int) (int64, error) {
	f.load()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = int64(len(f.content)) + offset
	}
	if abs < 0 {
		return 0, os.ErrInvalid
	}
	f.offset = abs
	return abs, nil
}

func (f *readFile) Close() error                             { return nil }
func (f *readFile) Write(p []byte) (int, error)              { return 0, os.ErrInvalid }
func (f *readFile) Readdir(int) ([]os.FileInfo, error)       { return nil, os.ErrInvalid }
func (f *readFile) Stat() (os.FileInfo, error)               { return metaToFileInfo(f.meta), nil }

// ── writeFile ─────────────────────────────────────────────────────────────────

type writeFile struct {
	client *core.Client
	path   string
	mime   string
	mu     sync.Mutex
	buf    []byte
	offset int64
}

func (f *writeFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	end := int(f.offset) + len(p)
	if end > len(f.buf) {
		newBuf := make([]byte, end)
		copy(newBuf, f.buf)
		f.buf = newBuf
	}
	copy(f.buf[f.offset:], p)
	f.offset += int64(len(p))
	return len(p), nil
}

func (f *writeFile) Close() error {
	return f.client.Write(f.path, f.buf, f.mime)
}

func (f *writeFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.offset = offset
	case io.SeekCurrent:
		f.offset += offset
	case io.SeekEnd:
		f.offset = int64(len(f.buf)) + offset
	}
	return f.offset, nil
}

func (f *writeFile) Read([]byte) (int, error)          { return 0, os.ErrInvalid }
func (f *writeFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (f *writeFile) Stat() (os.FileInfo, error)         { return &fileInfo{name: filepath.Base(f.path)}, nil }

// ── dirFile ───────────────────────────────────────────────────────────────────

type dirFile struct {
	name    string
	entries []core.FileMeta
}

func (f *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	infos := make([]os.FileInfo, len(f.entries))
	for i, e := range f.entries {
		infos[i] = metaToFileInfo(&e)
	}
	return infos, nil
}

func (f *dirFile) Stat() (os.FileInfo, error)          { return &fileInfo{name: f.name, isDir: true}, nil }
func (f *dirFile) Close() error                        { return nil }
func (f *dirFile) Read([]byte) (int, error)            { return 0, os.ErrInvalid }
func (f *dirFile) Write([]byte) (int, error)           { return 0, os.ErrInvalid }
func (f *dirFile) Seek(int64, int) (int64, error)      { return 0, os.ErrInvalid }

// ── fileInfo ──────────────────────────────────────────────────────────────────

type fileInfo struct {
	name    string
	size    int64
	isDir   bool
	modTime time.Time
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) IsDir() bool        { return fi.isDir }
func (fi *fileInfo) ModTime() time.Time { return fi.modTime }
func (fi *fileInfo) Sys() any           { return nil }
func (fi *fileInfo) Mode() os.FileMode {
	if fi.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}

func metaToFileInfo(meta *core.FileMeta) *fileInfo {
	fi := &fileInfo{
		name:  meta.Name,
		size:  meta.Size,
		isDir: meta.Type == "folder",
	}
	if t, err := time.Parse(time.RFC3339, meta.UpdatedAt); err == nil {
		fi.modTime = t
	}
	return fi
}
