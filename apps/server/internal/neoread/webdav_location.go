package neoread

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type syncLocation struct {
	ID     string `json:"id"`
	Folder string `json:"folder"`
}

func validSyncFolder(name string) bool {
	return name == strings.TrimSpace(name) && name != "" && name != "." && name != ".." &&
		utf8.RuneCountInString(name) <= 80 && !strings.ContainsAny(name, "/\\") &&
		!strings.ContainsFunc(name, unicode.IsControl)
}
func (s *Server) syncNamePath() string {
	return filepath.Join(s.Store.Root, "webdav-library-name.json")
}
func (s *Server) writeSyncConfig(config webdavConfig) error {
	config.PasswordSet = config.Password != ""
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return atomicFile(s.syncConfigPath(), body)
}
func (d *davClient) rootClient() *davClient {
	result := *d
	u := *d.base
	u.Path = "/dav/NeoRead/"
	result.base = &u
	return &result
}
func (d *davClient) setFolder(folder string) {
	u := *d.base
	u.Path = "/dav/NeoRead/" + folder + "/"
	d.base = &u
}
func readSyncLocation(ctx context.Context, dav *davClient, name string) (syncLocation, error) {
	body, err := dav.get(ctx, name, 4096)
	if err != nil {
		return syncLocation{}, err
	}
	var location syncLocation
	if decodeSyncJSON(body, &location) != nil || !syncID(location.ID) || !validSyncFolder(location.Folder) {
		return syncLocation{}, errors.New("云端书库目录信息无效")
	}
	return location, nil
}
func davNotFound(err error) bool {
	var remote *davError
	return errors.As(err, &remote) && remote.Status == 404
}

// Stable IDs in the small app catalog let already-connected devices find a
// renamed library without recreating its previous directory.
func (s *Server) resolveSyncLocation(ctx context.Context, config webdavConfig) (webdavConfig, *davClient, error) {
	dav := s.dav(config)
	root := dav.rootClient()
	location := syncLocation{ID: config.RemoteID, Folder: config.Folder}
	if location.ID == "" {
		if err := dav.prepare(ctx); err != nil {
			return config, nil, err
		}
		if err := root.mkdir(ctx, ".libraries"); err != nil {
			return config, nil, err
		}
		existing, err := readSyncLocation(ctx, dav, "library.json")
		if err == nil {
			location = existing
		} else if !davNotFound(err) {
			return config, nil, err
		} else {
			// Concurrent first connections choose the same available identity.
			// A name reused after a rename must get a different identity.
			for generation := 0; ; generation++ {
				seed := config.Folder
				if generation > 0 {
					seed += "\x00" + strconv.Itoa(generation)
				}
				location.ID = syncBytesHash([]byte(seed))[:32]
				registered, e := readSyncLocation(ctx, root, ".libraries/"+location.ID+".json")
				if davNotFound(e) || e == nil && registered.Folder == config.Folder {
					break
				}
				if e != nil {
					return config, nil, e
				}
			}
			body, _ := json.Marshal(location)
			if err = dav.put(ctx, "library.json", body); err != nil {
				return config, nil, err
			}
		}
		registered, err := readSyncLocation(ctx, root, ".libraries/"+location.ID+".json")
		if err == nil {
			location = registered
		} else if !davNotFound(err) {
			return config, nil, err
		} else {
			body, _ := json.Marshal(location)
			if err = root.put(ctx, ".libraries/"+location.ID+".json", body); err != nil {
				return config, nil, err
			}
		}
	} else {
		registered, err := readSyncLocation(ctx, root, ".libraries/"+location.ID+".json")
		if err != nil {
			return config, nil, err
		}
		if registered.ID != location.ID {
			return config, nil, errors.New("云端书库标识不一致")
		}
		location = registered
	}
	dav.setFolder(location.Folder)
	config.RemoteID = location.ID
	config.Folder = location.Folder
	return config, dav, nil
}

func (s *Server) prepareSyncLocation(ctx context.Context, config webdavConfig) (webdavConfig, *davClient, error) {
	config, dav, err := s.resolveSyncLocation(ctx, config)
	if err != nil {
		return config, nil, err
	}
	root := dav.rootClient()
	location := syncLocation{ID: config.RemoteID, Folder: config.Folder}
	name, err := os.ReadFile(s.syncNamePath())
	if err != nil && !os.IsNotExist(err) {
		return config, nil, err
	}
	desired := strings.TrimSpace(string(name))
	if desired != "" && desired != location.Folder {
		if !validSyncFolder(desired) {
			return config, nil, errors.New("书库名称不能用于坚果云文件夹")
		}
		destination := *dav.base
		destination.Path = "/dav/NeoRead/" + desired + "/"
		// MOVE is one server-side rename; never copy/delete an entire library.
		res, moveErr := dav.move(ctx, destination.String())
		if moveErr != nil {
			return config, nil, moveErr
		}
		if res.StatusCode == 201 || res.StatusCode == 204 {
			res.Body.Close()
		} else {
			status := res.StatusCode
			res.Body.Close()
			if status == 404 {
				target := *dav
				target.setFolder(desired)
				existing, e := readSyncLocation(ctx, &target, "library.json")
				if e != nil || existing.ID != location.ID {
					return config, nil, errors.New("云端书库更名尚未完成，请稍后重试")
				}
			} else if status == 412 || status == 409 {
				return config, nil, errors.New("坚果云已有同名文件夹，请为本地书库换一个名称")
			} else {
				return config, nil, &davError{Status: status}
			}
		}
		location.Folder = desired
		dav.setFolder(desired)
		body, _ := json.Marshal(location)
		if err = dav.put(ctx, "library.json", body); err != nil {
			return config, nil, err
		}
		if err = root.put(ctx, ".libraries/"+location.ID+".json", body); err != nil {
			return config, nil, err
		}
	}
	config.RemoteID = location.ID
	config.Folder = location.Folder
	config.LibraryConfigured = true
	if err = s.writeSyncConfig(config); err != nil {
		return config, nil, err
	}
	if desired != "" && desired == location.Folder {
		latest, _ := os.ReadFile(s.syncNamePath())
		if string(latest) == string(name) {
			if err = os.Remove(s.syncNamePath()); err != nil && !os.IsNotExist(err) {
				return config, nil, err
			}
		}
	}
	return config, dav, nil
}

func (d *davClient) move(ctx context.Context, destination string) (*http.Response, error) {
	// Share normal pacing/auth/redirect policy while adding MOVE's required
	// destination headers before the transport sends the request.
	wrapped := *d.http
	transport := wrapped.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	wrapped.Transport = syncMoveTransport{base: transport, destination: destination}
	client := *d
	client.http = &wrapped
	res, err := client.request(ctx, "MOVE", "", nil)
	return res, err
}

type syncMoveTransport struct {
	base        http.RoundTripper
	destination string
}

func (t syncMoveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Destination", t.destination)
	clone.Header.Set("Overwrite", "F")
	return t.base.RoundTrip(clone)
}
