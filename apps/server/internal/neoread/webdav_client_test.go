package neoread

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

func testDAV(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	directory := t.TempDir()
	handler := &webdav.Handler{Prefix: "/dav", FileSystem: webdav.Dir(directory), LockSystem: webdav.NewMemLS()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "reader@example.com" || password != "application-password" {
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, directory
}
func testDAVClient(config webdavConfig, server *httptest.Server) *davClient {
	d := newDAVClient(config)
	d.base, _ = url.Parse(server.URL + "/dav/NeoRead/" + config.Folder + "/")
	d.interval = 0
	return d
}
func TestDAVCreatesApplicationAndLibraryFolders(t *testing.T) {
	server, directory := testDAV(t)
	config := webdavConfig{Account: "reader@example.com", Password: "application-password", Folder: "论文库"}
	d := testDAVClient(config, server)
	if err := d.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"NeoRead/论文库/devices", "NeoRead/论文库/objects"} {
		if info, err := os.Stat(filepath.Join(directory, folder)); err != nil || !info.IsDir() {
			t.Fatalf("missing %s: %v", folder, err)
		}
	}
	device := strings.Repeat("a", 32) + ".json"
	if err := d.put(context.Background(), "devices/"+device, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	list, err := d.devices(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != device || list[0].ETag == "" {
		t.Fatalf("listing: %+v %v", list, err)
	}
	if err = d.prepare(context.Background()); err != nil {
		t.Fatalf("repeat folder creation: %v", err)
	}
}
func TestDAVDoesNotForwardApplicationPasswordToRedirect(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	d := newDAVClient(webdavConfig{Account: "reader@example.com", Password: "application-password", Folder: "论文库"})
	d.base, _ = url.Parse(redirect.URL + "/dav/NeoRead/论文库/")
	d.interval = 0
	_, err := d.get(context.Background(), "devices/a.json", 100)
	var remote *davError
	if !errors.As(err, &remote) || remote.Status != 307 || forwarded.Load() != 0 {
		t.Fatalf("redirect: %v, forwarded %d", err, forwarded.Load())
	}
}
func TestDAVPreservesRateLimitDelayAndHidesServerBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("server-private-response application-password"))
	}))
	defer server.Close()
	d := testDAVClient(webdavConfig{Folder: "论文库"}, server)
	_, err := d.get(context.Background(), "devices/a.json", 100)
	var remote *davError
	if !errors.As(err, &remote) || remote.Retry != 37*time.Second || strings.Contains(err.Error(), "private") {
		t.Fatalf("rate limit: %v", err)
	}
}
func TestDAVCancellationStopsPacing(t *testing.T) {
	d := newDAVClient(webdavConfig{Folder: "论文库"})
	d.last = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.request(ctx, "GET", "devices/a.json", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
