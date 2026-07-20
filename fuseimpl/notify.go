package fuseimpl

import (
	"strings"

	"github.com/hanwen/go-fuse/v2/fs"
)

// NotifyPath tells the kernel to forget what it cached about a path, so a change
// made somewhere else (web UI, agent, another machine) shows up on the next
// access instead of after the attribute timeout.
//
// Only paths the kernel currently holds can be invalidated — anything it never
// looked up has nothing to forget, so a miss is a no-op rather than an error.
func NotifyPath(root *fs.Inode, path string) {
	if root == nil {
		return
	}

	path = strings.Trim(path, "/")
	if path == "" {
		return
	}

	segments := strings.Split(path, "/")

	node := root
	for _, name := range segments[:len(segments)-1] {
		child := node.GetChild(name)
		if child == nil {
			return // an uncached ancestor means the leaf isn't cached either
		}
		node = child
	}

	name := segments[len(segments)-1]

	// Drop the parent's memory of this name — covers create, delete and rename.
	node.NotifyEntry(name)

	// And any cached contents of the file itself (0,0 = the whole thing).
	if child := node.GetChild(name); child != nil {
		child.NotifyContent(0, 0)
	}
}
