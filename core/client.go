package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("not found")

// ErrForbidden is returned when the server rejects an operation as out of the
// grant's scope or on a read-only session (HTTP 403). The FUSE layer maps this
// to EACCES rather than EIO so tools report "permission denied" correctly.
var ErrForbidden = errors.New("forbidden")

const cacheTTL = 5 * time.Second

type cacheEntry[T any] struct {
	value   T
	expires time.Time
}

type cache[T any] struct {
	mu      sync.Mutex
	entries map[string]cacheEntry[T]
}

func (c *cache[T]) get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		var zero T
		return zero, false
	}
	return e.value, true
}

func (c *cache[T]) set(key string, value T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]cacheEntry[T])
	}
	c.entries[key] = cacheEntry[T]{value: value, expires: time.Now().Add(cacheTTL)}
}

func (c *cache[T]) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *cache[T]) purgeExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
}

type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	statCache cache[*FileMeta]
	listCache cache[[]FileMeta]
}

type FileMeta struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // "file" | "folder"
	Size      int64  `json:"size"`
	UpdatedAt string `json:"updated_at"`
}

func NewClient(baseURL, token string) *Client {
	c := &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	go c.cacheJanitor()
	return c
}

func (c *Client) cacheJanitor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.statCache.purgeExpired()
		c.listCache.purgeExpired()
	}
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.http.Do(req)
}

func (c *Client) Stat(filePath string) (*FileMeta, error) {
	if v, ok := c.statCache.get(filePath); ok {
		return v, nil
	}
	resp, err := c.do("GET", "/api/fuse/stat?path="+url.QueryEscape(filePath), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode == 403 {
		return nil, ErrForbidden
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("stat %s: HTTP %d", filePath, resp.StatusCode)
	}
	var meta FileMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, err
	}
	c.statCache.set(filePath, &meta)
	return &meta, nil
}

func (c *Client) List(filePath string) ([]FileMeta, error) {
	if v, ok := c.listCache.get(filePath); ok {
		return v, nil
	}
	resp, err := c.do("GET", "/api/fuse/list?path="+url.QueryEscape(filePath), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 {
		return nil, ErrForbidden
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("list %s: HTTP %d", filePath, resp.StatusCode)
	}
	var entries []FileMeta
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}
	c.listCache.set(filePath, entries)

	// Pre-populate stat cache so walkFS-style callers (WebDAV PROPFIND depth=1)
	// don't make a second API call per child and risk getting a stale 404.
	prefix := filePath
	if prefix != "" {
		prefix += "/"
	}
	for i := range entries {
		c.statCache.set(prefix+entries[i].Name, &entries[i])
	}

	return entries, nil
}

// Read returns a file's bytes, fetched on demand (lazy). Scope — which files
// are visible at all — is enforced server-side by the token, not here.
func (c *Client) Read(filePath string) ([]byte, error) {
	resp, err := c.do("GET", "/api/fuse/read?path="+url.QueryEscape(filePath), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode == 403 {
		return nil, ErrForbidden
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("read %s: HTTP %d", filePath, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ReadRange returns bytes [off, off+length) of a file. length <= 0 means "to
// the end". It lets the driver serve reads through a bounded window instead of
// pulling a whole file into memory — so a large file no longer OOMs the
// container. NOTE: under the server's whole-blob AES-CBC encryption a range
// still costs a full server-side decrypt; this bounds *client* memory, not
// server work. Cheap server-side ranges need chunked/CTR encryption (deferred).
func (c *Client) ReadRange(filePath string, off int64, length int) ([]byte, error) {
	q := "/api/fuse/read?path=" + url.QueryEscape(filePath) + "&offset=" + strconv.FormatInt(off, 10)
	if length > 0 {
		q += "&length=" + strconv.Itoa(length)
	}
	resp, err := c.do("GET", q, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode == 403 {
		return nil, ErrForbidden
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("read %s @%d: HTTP %d", filePath, off, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// Truncate sets a file's length to size (shrink cuts, grow zero-fills),
// server-side. Backs ftruncate()/Setattr(size) and truncating opens.
func (c *Client) Truncate(filePath string, size int64) error {
	resp, err := c.do("POST", "/api/fuse/truncate", map[string]any{
		"path": filePath,
		"size": size,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return ErrNotFound
	}
	if resp.StatusCode == 403 {
		return ErrForbidden
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("truncate %s: HTTP %d: %s", filePath, resp.StatusCode, body)
	}
	c.statCache.delete(filePath)
	c.listCache.delete(parent(filePath))
	return nil
}

func (c *Client) Write(filePath string, content []byte, mime string) error {
	if mime == "" {
		mime = "text/plain"
	}
	resp, err := c.do("POST", "/api/fuse/write", map[string]string{
		"path":    filePath,
		"content": base64.StdEncoding.EncodeToString(content),
		"mime":    mime,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("write %s: HTTP %d: %s", filePath, resp.StatusCode, body)
	}
	// Pre-populate stat cache so the next PROPFIND sees the file immediately
	// without hitting the API again (which may still be processing the document).
	c.statCache.set(filePath, &FileMeta{
		Name:      filepath.Base(filePath),
		Type:      "file",
		Size:      int64(len(content)),
		UpdatedAt: time.Now().Format(time.RFC3339),
	})
	c.listCache.delete(parent(filePath))
	return nil
}

func (c *Client) Mkdir(filePath string) error {
	resp, err := c.do("POST", "/api/fuse/mkdir", map[string]string{"path": filePath})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("mkdir %s: HTTP %d", filePath, resp.StatusCode)
	}
	c.listCache.delete(parent(filePath))
	return nil
}

func (c *Client) Delete(filePath string) error {
	resp, err := c.do("DELETE", "/api/fuse/delete?path="+url.QueryEscape(filePath), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("delete %s: HTTP %d", filePath, resp.StatusCode)
	}
	c.statCache.delete(filePath)
	c.listCache.delete(parent(filePath))
	return nil
}

func (c *Client) Rename(from, to string) error {
	resp, err := c.do("POST", "/api/fuse/rename", map[string]string{"from": from, "to": to})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("rename %s -> %s: HTTP %d", from, to, resp.StatusCode)
	}
	c.statCache.delete(from)
	c.statCache.delete(to)
	c.listCache.delete(parent(from))
	c.listCache.delete(parent(to))
	return nil
}

func parent(filePath string) string {
	i := len(filePath) - 1
	for i >= 0 && filePath[i] != '/' {
		i--
	}
	if i <= 0 {
		return ""
	}
	return filePath[:i]
}

func MimeFromName(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return "text/plain"
	}
	t := mime.TypeByExtension(ext)
	if t == "" {
		return "application/octet-stream"
	}
	// Strip parameters like "; charset=utf-8" — PHP extFromMime needs a bare type.
	if i := strings.Index(t, ";"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}
