package main

import (
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
	apiURL := flag.String("api", "", "Eidos API base URL (e.g. https://app.eidos.my)")
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

	server, err := fs.Mount(*mount, fuseimpl.NewRoot(client, nil), opts)
	if err != nil {
		log.Fatalf("mount %s: %v\n\nMake sure macFUSE is installed: https://osxfuse.github.io", *mount, err)
	}

	log.Printf("Eidos mounted at %s (unmount with: umount %s)", *mount, *mount)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		server.Unmount()
	}()

	server.Wait()
	log.Println("Eidos unmounted")
}
