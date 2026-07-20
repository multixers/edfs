package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"eidos/fuse/core"
	"eidos/fuse/fuseimpl"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	apiURL := flag.String("api", "", "Eidos file API base URL, including its path (e.g. https://app.eidos.my/api/fuse)")
	token := flag.String("token", "", "Sanctum API token")
	mount := flag.String("mount", "", "Mount point (e.g. /Users/you/Eidos)")
	flag.Parse()

	if *apiURL == "" || *token == "" || *mount == "" {
		log.Fatal("--api, --token and --mount are required")
	}

	if err := os.MkdirAll(*mount, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", *mount, err)
	}

	client := core.NewClient(*apiURL, *token)

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:    "eidos",
			FsName:  "Eidos",
			Options: []string{"volname=Eidos"},
		},
	}

	root := fuseimpl.NewRoot(client, nil)

	server, err := fs.Mount(*mount, root, opts)
	if err != nil {
		log.Fatalf("mount %s: %v\n\nMake sure macFUSE is installed: https://osxfuse.github.io", *mount, err)
	}

	log.Printf("Eidos mounted at %s (unmount with: umount %s)", *mount, *mount)

	// Same change feed as the linux build: writes made elsewhere invalidate both
	// our cache and the kernel's, rather than waiting out the attribute timeout.
	ctx, stopWatching := context.WithCancel(context.Background())
	defer stopWatching()
	go client.WatchChanges(ctx, func(change core.Change) {
		fuseimpl.NotifyPath(root.EmbeddedInode(), change.Path)
		if change.From != "" {
			fuseimpl.NotifyPath(root.EmbeddedInode(), change.From)
		}
	})

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		stopWatching()

		// Unmount refuses while anything is still using the mount — a shell sitting
		// in it is enough. Say so instead of appearing to hang, and let a second
		// interrupt give up on the clean exit.
		if err := server.Unmount(); err != nil {
			log.Printf("unmount %s: %v", *mount, err)
			log.Printf("something is still using it; free it or run: umount -f %s", *mount)
		}

		<-sig
		log.Printf("exiting without a clean unmount; run: umount -f %s", *mount)
		os.Exit(1)
	}()

	server.Wait()
	log.Println("Eidos unmounted")
}
