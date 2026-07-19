package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"eidos/fuse/core"

	"golang.org/x/net/webdav"
)

// permissiveLS is a WebDAV LockSystem that always grants locks.
// macOS WebDAV uses concurrent connections that can deadlock on MemLS
// when both connections try to lock the same file (e.g. ._text.txt).
// Since Eidos is single-user, lock conflicts between concurrent macOS
// connections are safe to ignore.
type permissiveLS struct {
	mu sync.Mutex
	n  int64
}

func (ls *permissiveLS) Confirm(_ time.Time, _, _ string, _ ...webdav.Condition) (func(), error) {
	return func() {}, nil
}

func (ls *permissiveLS) Create(_ time.Time, _ webdav.LockDetails) (string, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.n++
	return fmt.Sprintf("urn:eidos:lock:%d", ls.n), nil
}

func (ls *permissiveLS) Refresh(_ time.Time, _ string, d time.Duration) (webdav.LockDetails, error) {
	return webdav.LockDetails{Duration: d}, nil
}

func (ls *permissiveLS) Unlock(_ time.Time, _ string) error { return nil }

func main() {
	apiURL := flag.String("api", "", "Eidos API base URL (e.g. https://app.eidos.my)")
	token := flag.String("token", "", "Sanctum API token")
	port := flag.String("port", "1900", "Local port to listen on")
	flag.Parse()

	if *apiURL == "" || *token == "" {
		log.Fatal("--api and --token are required")
	}

	client := core.NewClient(*apiURL, *token)

	dav := &webdav.Handler{
		FileSystem: &EidosFS{client: client},
		LockSystem: &permissiveLS{},
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("DAV  ERR  %s %s → %v", r.Method, r.URL.Path, err)
			} else {
				log.Printf("DAV  OK   %s %s", r.Method, r.URL.Path)
			}
		},
	}

	// Raw middleware: log every HTTP request before webdav sees it.
	// This reveals if MOVE/COPY are sent but panicking inside the handler.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dest := r.Header.Get("Destination")
		if dest != "" {
			log.Printf("RAW  %s %s → %s", r.Method, r.URL.Path, dest)
		} else {
			log.Printf("RAW  %s %s", r.Method, r.URL.Path)
		}
		dav.ServeHTTP(w, r)
	})

	addr := "127.0.0.1:" + *port
	log.Printf("Eidos WebDAV listening on %s", addr)
	log.Printf("Connect in Finder: Go → Connect to Server → http://%s", addr)

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}
