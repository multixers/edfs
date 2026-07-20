package fuseimpl

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"eidos/fuse/core"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var myUID = uint32(os.Getuid())
var myGID = uint32(os.Getgid())

// SetOwner overrides the uid/gid the tree is reported as owned by. When root
// mounts for the sandbox user, set this to the sandbox uid/gid so the files
// appear owned by (and are usable by) that user.
func SetOwner(uid, gid uint32) {
	myUID = uid
	myGID = gid
}

// LocalRedirect isolates subtrees whose directory name is in Names (e.g.
// "node_modules", ".next") to a local directory instead of the cloud API — so
// heavy, regenerable dirs stay on local disk (fast) and never touch Storage.
// nil = no isolation (everything goes to the cloud).
type LocalRedirect struct {
	Root  string          // local base dir, e.g. /var/lib/edfs-local
	Names map[string]bool // dir names that trigger local passthrough
}

func (l *LocalRedirect) has(name string) bool {
	return l != nil && l.Names[name]
}

// NewRoot returns the root FUSE node backed by the Eidos API client. local may
// be nil (no isolation).
func NewRoot(client *core.Client, local *LocalRedirect) *Node {
	return &Node{client: client, local: local, path: ""}
}

// ── Node ──────────────────────────────────────────────────────────────────────

type Node struct {
	fs.Inode
	client *core.Client
	local  *LocalRedirect
	path   string
}

// localSubtree returns a go-fuse loopback inode for a locally-isolated dir,
// creating the backing dir when create is set. Everything under it is native
// local disk (no cloud, no per-op API call).
func (n *Node) localSubtree(ctx context.Context, childPath string, create bool, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	dir := filepath.Join(n.local.Root, childPath)
	if create {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("ERR local mkdir %s: %v", dir, err)
			return nil, syscall.EIO
		}
	} else if _, err := os.Stat(dir); err != nil {
		return nil, syscall.ENOENT
	}

	loop, err := fs.NewLoopbackRoot(dir)
	if err != nil {
		log.Printf("ERR loopback %s: %v", dir, err)
		return nil, syscall.EIO
	}
	if out != nil {
		out.EntryValid = 1
		out.AttrValid = 1
		out.Mode = syscall.S_IFDIR | 0755
	}
	ch := n.NewInode(ctx, loop, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: pathIno(childPath)})
	return ch, fs.OK
}

var _ fs.NodeGetattrer = (*Node)(nil)
var _ fs.NodeSetattrer = (*Node)(nil)
var _ fs.NodeStatfser = (*Node)(nil)
var _ fs.NodeReaddirer = (*Node)(nil)
var _ fs.NodeLookuper = (*Node)(nil)
var _ fs.NodeOpener = (*Node)(nil)
var _ fs.NodeCreater = (*Node)(nil)
var _ fs.NodeMkdirer = (*Node)(nil)
var _ fs.NodeUnlinker = (*Node)(nil)
var _ fs.NodeRmdirer = (*Node)(nil)
var _ fs.NodeRenamer = (*Node)(nil)
var _ fs.NodeReadlinker = (*Node)(nil)
var _ fs.NodeSymlinker = (*Node)(nil)

// mapErrno turns a core client error into the closest POSIX errno so tools see
// "no such file" / "permission denied" instead of a blanket I/O error.
func mapErrno(err error) syscall.Errno {
	switch {
	case err == nil:
		return fs.OK
	case errors.Is(err, core.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, core.ErrForbidden):
		return syscall.EACCES
	default:
		return syscall.EIO
	}
}

func (n *Node) childPath(name string) string {
	if n.path == "" {
		return name
	}
	return n.path + "/" + name
}

func (n *Node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.AttrValid = 5

	if n.path == "" {
		out.Uid = myUID
		out.Gid = myGID
		out.Mode = 0755 | syscall.S_IFDIR
		out.Nlink = 2
		out.Atime = uint64(time.Now().Unix())
		out.Mtime = uint64(time.Now().Unix())
		return fs.OK
	}

	meta, err := n.client.Stat(n.path)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			log.Printf("ERR getattr %s: %v", n.path, err)
		}
		return mapErrno(err)
	}

	fillAttr(&out.Attr, meta)
	return fs.OK
}

// Setattr backs chmod/chown/truncate/utimes. Size (truncate) and mode (chmod)
// are real and persisted; owner and timestamps are accepted but NOT persisted
// (Storage has no unix owner model and mtime is the record's updated_at).
// Accepting the latter (instead of ENOSYS/EPERM) is what lets `cp -p`, tar, git
// checkout and editors finish without spurious errors.
func (n *Node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok {
		if wh, isWrite := fh.(*WriteHandle); isWrite {
			wh.truncateBuf(int64(sz))
		} else if err := n.client.Truncate(n.path, int64(sz)); err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				log.Printf("ERR truncate %s: %v", n.path, err)
			}
			return mapErrno(err)
		}
	}
	if m, ok := in.GetMode(); ok {
		if err := n.client.Chmod(n.path, m); err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				log.Printf("ERR chmod %s: %v", n.path, err)
			}
			return mapErrno(err)
		}
	}
	return n.Getattr(ctx, fh, out)
}

// Statfs reports a large synthetic capacity so `df` renders and applications
// that check free space before writing don't refuse. Storage has no per-user
// quota yet; wire real figures here when it does.
func (n *Node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	const bsize = 4096
	const totalBytes = 1 << 50 // 1 PiB headroom
	out.Bsize = bsize
	out.Frsize = bsize
	out.Blocks = totalBytes / bsize
	out.Bfree = out.Blocks * 15 / 16
	out.Bavail = out.Bfree
	out.Files = 1 << 22
	out.Ffree = 1 << 21
	out.NameLen = 255
	return fs.OK
}

func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := n.childPath(name)

	// Isolated dirs (node_modules, ...) live on local disk, not the cloud.
	if n.local.has(name) {
		return n.localSubtree(ctx, childPath, false, out)
	}

	meta, err := n.client.Stat(childPath)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			log.Printf("ERR lookup %s: %v", childPath, err)
		}
		return nil, mapErrno(err)
	}

	out.EntryValid = 5
	out.AttrValid = 5
	fillAttr(&out.Attr, meta)

	child := &Node{client: n.client, local: n.local, path: childPath}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: pathIno(childPath)}
	return n.NewInode(ctx, child, stable), fs.OK
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.client.List(n.path)
	if err != nil {
		log.Printf("ERR readdir %s: %v", n.path, err)
		return nil, syscall.EIO
	}

	dirEntries := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		mode := uint32(0644)
		switch e.Type {
		case "folder":
			mode = 0755 | syscall.S_IFDIR
		case "symlink":
			mode = syscall.S_IFLNK | 0o777
		}
		dirEntries = append(dirEntries, fuse.DirEntry{
			Name: e.Name,
			Mode: mode,
			Ino:  pathIno(n.childPath(e.Name)),
		})
	}

	return fs.NewListDirStream(dirEntries), fs.OK
}

func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		// The existing bytes may still be needed (a partial write keeps them), but
		// they're fetched on demand rather than here: the kernel drops O_TRUNC from
		// a FUSE open and truncates immediately afterwards, so seeding at open
		// would read a whole file that's about to be thrown away.
		wh, err := newWriteHandle(n.client, n.path, core.MimeFromName(n.path), flags&syscall.O_TRUNC == 0)
		if err != nil {
			log.Printf("ERR open-tmp %s: %v", n.path, err)
			return nil, 0, syscall.EIO
		}
		return wh, fuse.FOPEN_DIRECT_IO, fs.OK
	}
	return &ReadHandle{client: n.client, path: n.path}, fuse.FOPEN_DIRECT_IO, fs.OK
}

func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	childPath := n.childPath(name)
	// A created file has no prior contents to preserve.
	fh, err := newWriteHandle(n.client, childPath, core.MimeFromName(name), false)
	if err != nil {
		log.Printf("ERR create-tmp %s: %v", childPath, err)
		return nil, nil, 0, syscall.EIO
	}

	out.EntryValid = 1
	out.AttrValid = 1
	out.Mode = mode | 0100000
	out.Size = 0

	child := &Node{client: n.client, local: n.local, path: childPath}
	stable := fs.StableAttr{Mode: out.Mode, Ino: pathIno(childPath)}
	return n.NewInode(ctx, child, stable), fh, fuse.FOPEN_DIRECT_IO, fs.OK
}

func (n *Node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := n.childPath(name)

	// Creating an isolated dir (node_modules, ...) → back it with local disk.
	if n.local.has(name) {
		return n.localSubtree(ctx, childPath, true, out)
	}

	if err := n.client.Mkdir(childPath); err != nil {
		log.Printf("ERR mkdir %s: %v", childPath, err)
		return nil, mapErrno(err)
	}

	out.EntryValid = 5
	out.AttrValid = 5
	out.Mode = 0755 | syscall.S_IFDIR
	out.Nlink = 2

	child := &Node{client: n.client, local: n.local, path: childPath}
	stable := fs.StableAttr{Mode: syscall.S_IFDIR | 0755, Ino: pathIno(childPath)}
	return n.NewInode(ctx, child, stable), fs.OK
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	p := n.childPath(name)
	if err := n.client.Delete(p); err != nil {
		log.Printf("ERR unlink %s: %v", p, err)
		return mapErrno(err)
	}
	return fs.OK
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	p := n.childPath(name)
	if err := n.client.Delete(p); err != nil {
		log.Printf("ERR rmdir %s: %v", p, err)
		return mapErrno(err)
	}
	return fs.OK
}

// Readlink returns a symlink's target (the raw stored string; the kernel
// resolves it). Backs readlink()/lstat on a symlink node.
func (n *Node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	meta, err := n.client.Stat(n.path)
	if err != nil {
		return nil, mapErrno(err)
	}
	return []byte(meta.Target), fs.OK
}

// Symlink creates a symbolic link `name` → `target` under this dir. Backs the
// symlink() syscall (used by pnpm, .venv, `ln -s`, ...).
func (n *Node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := n.childPath(name)
	if err := n.client.Symlink(childPath, target); err != nil {
		log.Printf("ERR symlink %s -> %s: %v", childPath, target, err)
		return nil, mapErrno(err)
	}

	out.EntryValid = 5
	out.AttrValid = 5
	out.Uid = myUID
	out.Gid = myGID
	out.Mode = syscall.S_IFLNK | 0o777
	out.Size = uint64(len(target))

	child := &Node{client: n.client, local: n.local, path: childPath}
	stable := fs.StableAttr{Mode: syscall.S_IFLNK, Ino: pathIno(childPath)}
	return n.NewInode(ctx, child, stable), fs.OK
}

func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	from := n.childPath(name)

	var toParent string
	if p, ok := newParent.(*Node); ok {
		toParent = p.path
	}

	var to string
	if toParent == "" {
		to = newName
	} else {
		to = toParent + "/" + newName
	}

	if err := n.client.Rename(from, to); err != nil {
		log.Printf("ERR rename %s -> %s: %v", from, to, err)
		return mapErrno(err)
	}
	return fs.OK
}

// ── ReadHandle ────────────────────────────────────────────────────────────────

// readWindow bounds how much of a file the driver holds at once. A small file
// (≤ window) is fetched whole on the first read (one round trip, as before); a
// large file is served through a sliding window so it never loads entirely into
// the container's memory. A window refetch happens when a read lands outside the
// currently cached span — cheap for sequential reads (players, cp, checksums).
const readWindow = 8 << 20 // 8 MiB

type ReadHandle struct {
	client *core.Client
	path   string
	mu     sync.Mutex
	size   int64
	sized  bool
	winOff int64
	winBuf []byte
	hasWin bool
}

var _ fs.FileReader = (*ReadHandle)(nil)

func (f *ReadHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.sized {
		meta, err := f.client.Stat(f.path)
		if err != nil {
			return nil, mapErrno(err)
		}
		f.size = meta.Size
		f.sized = true
	}
	if off >= f.size {
		return fuse.ReadResultData(nil), fs.OK
	}

	// (Re)fill the window if the offset isn't inside it.
	if !f.hasWin || off < f.winOff || off >= f.winOff+int64(len(f.winBuf)) {
		data, err := f.client.ReadRange(f.path, off, readWindow)
		if err != nil {
			return nil, mapErrno(err)
		}
		f.winOff = off
		f.winBuf = data
		f.hasWin = true
	}

	start := off - f.winOff
	if start >= int64(len(f.winBuf)) {
		return fuse.ReadResultData(nil), fs.OK
	}
	end := start + int64(len(dest))
	if end > int64(len(f.winBuf)) {
		end = int64(len(f.winBuf))
	}
	return fuse.ReadResultData(f.winBuf[start:end]), fs.OK
}

// ── WriteHandle ───────────────────────────────────────────────────────────────

// seedWindow bounds how much of an existing file is pulled at once when seeding
// the temp buffer for an in-place edit.
const seedWindow = 8 << 20 // 8 MiB

// WriteHandle backs a writable file with a host temp file rather than an in-RAM
// buffer, so writing a multi-GB file never balloons the process's memory. On
// flush the temp file is streamed to the chunked-encryption endpoint.
type WriteHandle struct {
	client *core.Client
	path   string
	mime   string
	mu     sync.Mutex
	tmp    *os.File
	size   int64 // logical size = highest offset written (or truncated to)

	// The file's existing bytes are loaded lazily — on the first write that could
	// leave part of them intact, never at open. The kernel strips O_TRUNC from a
	// FUSE open and truncates just *after* opening, so seeding eagerly would pull
	// (and, for a `> file` redirect, immediately discard) the whole file. Loading
	// on demand means an overwrite never reads what it's about to replace.
	pending bool

	// Nothing changed → nothing to upload. Without this, opening a file for
	// writing and closing it untouched would flush an empty buffer over it.
	dirty bool
}

var _ fs.FileWriter = (*WriteHandle)(nil)
var _ fs.FileFlusher = (*WriteHandle)(nil)
var _ fs.FileFsyncer = (*WriteHandle)(nil)
var _ fs.FileReleaser = (*WriteHandle)(nil)

// newWriteHandle opens a write buffer. existing says the file may already have
// contents worth preserving; they're fetched only if something actually needs
// them (see WriteHandle.pending).
func newWriteHandle(client *core.Client, path, mime string, existing bool) (*WriteHandle, error) {
	f, err := os.CreateTemp("", "edfs-w-")
	if err != nil {
		return nil, err
	}
	return &WriteHandle{client: client, path: path, mime: mime, tmp: f, pending: existing}, nil
}

// ensureLoaded copies the file's current bytes into the temp buffer, once,
// windowed so a large file isn't pulled whole into memory. Without it a partial
// write (open → seek → write a few bytes → close) would flush a buffer empty
// except for those bytes, dropping the rest of the file. Caller holds mu.
func (f *WriteHandle) ensureLoaded() error {
	if !f.pending {
		return nil
	}
	f.pending = false

	meta, err := f.client.Stat(f.path)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil // nothing there yet; a fresh write
		}
		return err
	}
	for off := int64(0); off < meta.Size; {
		data, err := f.client.ReadRange(f.path, off, seedWindow)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			break
		}
		if _, err := f.tmp.WriteAt(data, off); err != nil {
			return err
		}
		off += int64(len(data))
	}
	f.size = meta.Size
	return nil
}

func (f *WriteHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// A write that doesn't start at 0 leaves earlier bytes in place, so the old
	// contents have to be underneath it.
	if err := f.ensureLoaded(); err != nil {
		log.Printf("ERR write-load %s: %v", f.path, err)
		return 0, mapErrno(err)
	}

	n, err := f.tmp.WriteAt(data, off)
	if err != nil {
		log.Printf("ERR write-buf %s: %v", f.path, err)
		return 0, syscall.EIO
	}
	if end := off + int64(n); end > f.size {
		f.size = end
	}
	f.dirty = true
	return uint32(n), fs.OK
}

// truncateBuf resizes the pending temp buffer (shrink cuts, grow zero-fills, via
// the sparse temp file). Backs an ftruncate() on an open write handle.
func (f *WriteHandle) truncateBuf(size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if size == 0 {
		// Everything is being discarded, so there's nothing worth fetching — this
		// is what makes `> file` cost one upload instead of a full download first.
		f.pending = false
	} else if err := f.ensureLoaded(); err != nil {
		log.Printf("ERR truncate-load %s: %v", f.path, err)
		return
	}

	if err := f.tmp.Truncate(size); err != nil {
		log.Printf("ERR truncate-buf %s: %v", f.path, err)
		return
	}
	f.size = size
	f.dirty = true
}

func (f *WriteHandle) flush() syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.dirty {
		return fs.OK // opened for writing, never written to
	}
	if err := f.ensureLoaded(); err != nil {
		log.Printf("ERR flush-load %s: %v", f.path, err)
		return mapErrno(err)
	}
	if _, err := f.tmp.Seek(0, io.SeekStart); err != nil {
		log.Printf("ERR write-seek %s: %v", f.path, err)
		return syscall.EIO
	}
	if err := f.client.WriteStream(f.path, f.tmp, f.size, f.mime); err != nil {
		log.Printf("ERR write %s: %v", f.path, err)
		return mapErrno(err)
	}
	return fs.OK
}

func (f *WriteHandle) Flush(ctx context.Context) syscall.Errno { return f.flush() }

// Fsync durably persists on fsync(), not only on close, so a program that fsyncs
// to guarantee its data is saved actually gets that guarantee.
func (f *WriteHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno { return f.flush() }

// Release drops the temp file when the last fd closes.
func (f *WriteHandle) Release(ctx context.Context) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tmp != nil {
		name := f.tmp.Name()
		_ = f.tmp.Close()
		_ = os.Remove(name)
		f.tmp = nil
	}
	return fs.OK
}

// ── helpers ───────────────────────────────────────────────────────────────────

func fillAttr(out *fuse.Attr, meta *core.FileMeta) {
	out.Uid = myUID
	out.Gid = myGID
	switch meta.Type {
	case "folder":
		perm := uint32(0755)
		if meta.Mode != nil {
			perm = *meta.Mode & 0o7777
		}
		out.Mode = perm | syscall.S_IFDIR
		out.Nlink = 2
	case "symlink":
		out.Mode = syscall.S_IFLNK | 0o777 // symlinks carry all perms; the target governs access
		out.Size = uint64(len(meta.Target))
		out.Nlink = 1
	default: // file
		perm := uint32(0644)
		if meta.Mode != nil {
			perm = *meta.Mode & 0o7777
		}
		out.Mode = perm
		out.Size = uint64(meta.Size)
		out.Nlink = 1
	}
	if t, err := time.Parse(time.RFC3339, meta.UpdatedAt); err == nil {
		out.Mtime = uint64(t.Unix())
		out.Atime = uint64(t.Unix())
		out.Ctime = uint64(t.Unix())
	}
}

func pathIno(p string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(p); i++ {
		h ^= uint64(p[i])
		h *= 1099511628211
	}
	return h
}
