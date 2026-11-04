package neoread

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

type webdavLibrary struct {
	ID     string `json:"id"`
	Folder string `json:"folder"`
}

// Discovery reads only the small identity catalog, never document manifests or
// object chunks. Opening the switcher must not create or modify remote folders.
func (s *Server) remoteLibraries(ctx context.Context) ([]webdavLibrary, error) {
	config, err := s.readSyncConfig()
	if err != nil {
		return nil, err
	}
	if config.Account == "" || config.Password == "" {
		return nil, errors.New("请先配置坚果云连接")
	}
	root := s.dav(config).rootClient()
	files, err := root.manifests(ctx, ".libraries/")
	if davNotFound(err) {
		return []webdavLibrary{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []webdavLibrary{}
	for _, file := range files {
		location, err := readSyncLocation(ctx, root, ".libraries/"+file.Name)
		if err != nil {
			return nil, err
		}
		if location.ID != strings.TrimSuffix(file.Name, ".json") {
			return nil, errors.New("云端书库标识不一致")
		}
		if !location.Deleted {
			result = append(result, webdavLibrary{location.ID, location.Folder})
		}
	}
	slices.SortFunc(result, func(a, b webdavLibrary) int { return strings.Compare(a.Folder, b.Folder) })
	return result, nil
}

func (s *Server) remoteLibraryConfig(ctx context.Context, remoteID string) (webdavConfig, error) {
	config, err := s.readSyncConfig()
	if err != nil {
		return config, err
	}
	if config.Account == "" || config.Password == "" {
		return config, errors.New("请先配置坚果云连接")
	}
	config.RemoteID = remoteID
	config, _, err = s.resolveSyncLocation(ctx, config)
	config.Enabled, config.LibraryConfigured = true, true
	config.IntervalMinutes = 15
	return config, err
}

func (s *Server) registerRemoteLibraries(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sync/webdav/libraries", func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		libraries, err := s.remoteLibraries(r.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.diagnosticEvent(slog.LevelInfo, "sync.catalog_cancelled", "durationMs", time.Since(started).Milliseconds())
				return
			}
			s.diagnosticEvent(slog.LevelError, "sync.catalog_failed", "errorCode", "discovery", "durationMs", time.Since(started).Milliseconds())
			fail(w, 502, err.Error())
			return
		}
		s.diagnosticEvent(slog.LevelInfo, "sync.catalog_loaded", "count", len(libraries), "durationMs", time.Since(started).Milliseconds())
		respond(w, 200, libraries)
	})
	mux.HandleFunc("GET /api/sync/webdav/libraries/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !syncID(r.PathValue("id")) {
			fail(w, 400, "云端书库编号无效")
			return
		}
		config, err := s.remoteLibraryConfig(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		respond(w, 200, publicSyncConfig(config))
	})
}
