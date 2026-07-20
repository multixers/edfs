package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"eidos/fuse/core"
	"eidos/fuse/fuseimpl"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	apiURL := flag.String("api", "", "Eidos file API base URL, including its path (e.g. https://app.eidos.my/api/fuse)")
	token := flag.String("token", "", "Sanctum API token")
	mount := flag.String("mount", "", "Mount point (e.g. /mnt/eidos)")
	allowOther := flag.Bool("allow-other", false, "allow users other than the mounter to access the mount (needed when root mounts for the sandbox user)")
	uid := flag.Int("uid", -1, "report this uid as the owner of the tree (default: the mounting process's uid)")
	gid := flag.Int("gid", -1, "report this gid as the owner of the tree (default: the mounting process's gid)")
	localDirs := flag.String("local-dirs", "", "comma-separated dir names to isolate onto local disk (e.g. node_modules,.next,dist)")
	localRoot := flag.String("local-root", "/var/lib/edfs-local", "local base dir backing isolated subtrees")
	flag.Parse()

	if *apiURL == "" || *token == "" || *mount == "" {
		log.Fatal("--api, --token and --mount are required")
	}

	if err := os.MkdirAll(*mount, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", *mount, err)
	}

	if *uid >= 0 && *gid >= 0 {
		fuseimpl.SetOwner(uint32(*uid), uint32(*gid))
	}

	var local *fuseimpl.LocalRedirect
	if names := parseNames(*localDirs); len(names) > 0 {
		if err := os.MkdirAll(*localRoot, 0755); err != nil {
			log.Fatalf("mkdir local-root %s: %v", *localRoot, err)
		}
		local = &fuseimpl.LocalRedirect{Root: *localRoot, Names: names}
	}

	client := core.NewClient(*apiURL, *token)

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:       "edfs",
			FsName:     "edfs",
			AllowOther: *allowOther,
			Debug:      false,
		},
	}

	root := fuseimpl.NewRoot(client, local)

	server, err := fs.Mount(*mount, root, opts)
	if err != nil {
		log.Fatalf("mount %s: %v", *mount, err)
	}

	log.Printf("edfs mounted at %s", *mount)

	// Follow the server's change feed so writes made elsewhere (web UI, agent,
	// another machine) drop both our cache and the kernel's, instead of lingering
	// until the attribute timeout. Best-effort: if the feed is off or unreachable
	// the mount still works, just with TTL-bound freshness.
	ctx, stopWatching := context.WithCancel(context.Background())
	defer stopWatching()
	go client.WatchChanges(ctx, func(change core.Change) {
		fuseimpl.NotifyPath(root.EmbeddedInode(), change.Path)
		if change.From != "" {
			fuseimpl.NotifyPath(root.EmbeddedInode(), change.From)
		}
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		stopWatching()
		server.Unmount()
	}()

	server.Wait()
	log.Println("FUSE unmounted")
}

func parseNames(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}
