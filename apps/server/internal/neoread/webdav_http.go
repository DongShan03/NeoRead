package neoread

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (s *Server) syncInput(w http.ResponseWriter, r *http.Request) (webdavConfig, bool) {
	config := webdavConfig{IntervalMinutes: 15}
	if !decode(w, r, &config) {
		return config, false
	}
	config.Account = strings.TrimSpace(config.Account)
	config.Folder = strings.TrimSpace(config.Folder)
	config.DeviceName = strings.TrimSpace(config.DeviceName)
	old, err := s.readSyncConfig()
	if err != nil {
		fail(w, 500, err.Error())
		return config, false
	}
	if config.Password == "" && config.Account == old.Account {
		config.Password = old.Password
	}
	config.RemoteID = ""
	if config.Account == old.Account {
		config.RemoteID = old.RemoteID
	}
	if config.Folder == "" {
		config.Folder = "论文库"
	}
	if config.DeviceName == "" {
		config.DeviceName = "这台设备"
	}
	address, err := mail.ParseAddress(config.Account)
	if err != nil || address.Address != config.Account || len(config.Account) > 254 {
		fail(w, 400, "请填写坚果云账号邮箱")
		return config, false
	}
	if config.Password == "" || len(config.Password) > 1024 {
		fail(w, 400, "请填写坚果云第三方应用密码")
		return config, false
	}
	if !validSyncFolder(config.Folder) {
		fail(w, 400, "书库名称须为 1–80 个字符，不能包含斜杠或控制字符")
		return config, false
	}
	if config.IntervalMinutes < 1 || config.IntervalMinutes > 525600 {
		fail(w, 400, "同步间隔须为 1–525600 的整数分钟")
		return config, false
	}
	if utf8.RuneCountInString(config.DeviceName) > 80 || strings.ContainsFunc(config.DeviceName, unicode.IsControl) {
		fail(w, 400, "设备名称须为 1–80 个字符")
		return config, false
	}
	return config, true
}
func publicSyncConfig(config webdavConfig) webdavConfig {
	config.PasswordSet = config.Password != ""
	config.Password = ""
	return config
}
func (s *Server) registerWebDAV(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sync/webdav/config", func(w http.ResponseWriter, r *http.Request) {
		config, err := s.readSyncConfig()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if name, err := os.ReadFile(s.syncNamePath()); err == nil && validSyncFolder(string(name)) {
			config.Folder = string(name)
		}
		respond(w, 200, publicSyncConfig(config))
	})
	mux.HandleFunc("PUT /api/sync/webdav/config", func(w http.ResponseWriter, r *http.Request) {
		config, ok := s.syncInput(w, r)
		if !ok {
			return
		}
		if !s.webdav.operation.TryLock() {
			fail(w, 409, "请等待同步完成或先停止同步")
			return
		}
		defer s.webdav.operation.Unlock()
		old, err := s.readSyncConfig()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if config.RemoteID != "" && config.Folder != old.Folder {
			if err = atomicFile(s.syncNamePath(), []byte(config.Folder)); err != nil {
				fail(w, 500, "无法保存书库名称")
				return
			}
			config.Folder = old.Folder
		}
		config, _, err = s.prepareSyncLocation(r.Context(), config)
		if err != nil {
			s.diagnosticEvent(slog.LevelError, "sync.config_failed", "errorCode", "connection")
			fail(w, 502, err.Error())
			return
		}
		if old.Account != config.Account || old.RemoteID != config.RemoteID {
			if _, err = s.Store.DB.Exec("DELETE FROM settings WHERE key IN ('sync:synced-revision','sync:last-result')"); err != nil {
				fail(w, 500, "无法更新同步状态")
				return
			}
			s.webdav.mu.Lock()
			s.webdav.status = webdavStatus{Phase: "idle"}
			s.webdav.mu.Unlock()
		}
		if err = s.saveSyncConnection(config); err != nil {
			fail(w, 500, "无法保存共享 WebDAV 连接")
			return
		}
		s.webdav.mu.Lock()
		s.webdav.next = time.Now().Add(time.Duration(config.IntervalMinutes) * time.Minute)
		s.webdav.mu.Unlock()
		s.diagnosticEvent(slog.LevelInfo, "sync.config_saved", "enabled", config.Enabled, "intervalMinutes", config.IntervalMinutes)
		respond(w, 200, publicSyncConfig(config))
	})
	mux.HandleFunc("DELETE /api/sync/webdav/config", func(w http.ResponseWriter, r *http.Request) {
		if !s.webdav.operation.TryLock() {
			fail(w, 409, "请先停止同步")
			return
		}
		defer s.webdav.operation.Unlock()
		config, err := s.readSyncConfig()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		// Disabling pauses replication. The baseline and device identity are
		// needed to propagate edits and deletions made while sync is disabled.
		config.Enabled = false
		if config.LibraryConfigured {
			if err = s.writeSyncConfig(config); err != nil {
				fail(w, 500, "无法关闭此书库同步")
				return
			}
		}
		s.diagnosticEvent(slog.LevelInfo, "sync.disconnected")
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/sync/webdav/test", func(w http.ResponseWriter, r *http.Request) {
		config, ok := s.syncInput(w, r)
		if !ok {
			return
		}
		if !s.webdav.operation.TryLock() {
			fail(w, 409, "同步正在进行")
			return
		}
		defer s.webdav.operation.Unlock()
		dav := s.dav(config)
		err := dav.prepare(r.Context())
		if err == nil {
			name := ".connection-test-" + id()
			err = dav.put(r.Context(), name, []byte("NeoRead"))
			if err == nil {
				body, readErr := dav.get(r.Context(), name, 32)
				if readErr != nil {
					err = readErr
				} else if string(body) != "NeoRead" {
					err = errors.New("坚果云读写校验失败")
				}
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
				response, deleteErr := dav.request(cleanup, "DELETE", name, nil)
				cancel()
				if response != nil {
					response.Body.Close()
					if response.StatusCode != 204 && response.StatusCode != 200 {
						deleteErr = davStatus(response)
					}
				}
				if err == nil {
					err = deleteErr
				}
			}
		}
		if err != nil {
			s.diagnosticEvent(slog.LevelError, "sync.connection_failed", "errorCode", "connection")
			fail(w, 502, err.Error())
			return
		}
		s.diagnosticEvent(slog.LevelInfo, "sync.connection_verified")
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/sync/webdav/status", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.syncStatus()) })
	mux.HandleFunc("POST /api/sync/webdav/run", func(w http.ResponseWriter, r *http.Request) {
		if err := s.beginWebDAVSync(context.Background(), true); err != nil {
			fail(w, 409, err.Error())
			return
		}
		respond(w, 202, s.syncStatus())
	})
	mux.HandleFunc("POST /api/sync/webdav/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.webdav.mu.Lock()
		if s.webdav.cancel != nil {
			s.webdav.cancel()
		}
		s.webdav.mu.Unlock()
		respond(w, 202, s.syncStatus())
	})
	mux.HandleFunc("POST /api/sync/webdav/apply", func(w http.ResponseWriter, r *http.Request) {
		var value struct {
			Token string `json:"token"`
			Saved *bool  `json:"saved,omitempty"`
		}
		if !decode(w, r, &value) {
			return
		}
		s.webdav.mu.Lock()
		if value.Token == "" || value.Token != s.webdav.status.ApplyToken || s.webdav.applyReady == nil {
			s.webdav.mu.Unlock()
			fail(w, 409, "待应用的同步结果已变化")
			return
		}
		ready, done := s.webdav.applyReady, s.webdav.applyDone
		s.webdav.applyFailed = value.Saved != nil && !*value.Saved
		s.webdav.applyReady = nil
		close(ready)
		s.webdav.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-done:
			respond(w, 200, s.syncStatus())
		}
	})
	mux.HandleFunc("POST /api/sync/webdav/resolve", func(w http.ResponseWriter, r *http.Request) {
		var choice struct {
			Key      string `json:"key"`
			Conflict string `json:"conflict"`
			Version  string `json:"version"`
			KeepBoth bool   `json:"keepBoth"`
		}
		if !decode(w, r, &choice) {
			return
		}
		if !s.webdav.operation.TryLock() {
			fail(w, 409, "请等待同步完成")
			return
		}
		valid := false
		for _, conflict := range s.syncStatus().Conflicts {
			if conflict.Key == choice.Key && conflict.ID == choice.Conflict {
				if choice.KeepBoth && choice.Version == "" && conflict.CanKeepBoth {
					valid = true
				}
				for _, version := range conflict.Versions {
					if !choice.KeepBoth && version.ID == choice.Version {
						valid = true
					}
				}
			}
		}
		if !valid {
			s.webdav.operation.Unlock()
			fail(w, 409, "冲突版本已变化，请重新同步后选择")
			return
		}
		config, err := s.readSyncConfig()
		var state *syncLocalState
		if err == nil {
			state, err = s.loadSyncState(config)
		}
		if err == nil {
			state.Choices[choice.Key] = syncChoice{Conflict: choice.Conflict, Version: choice.Version, KeepBoth: choice.KeepBoth}
			err = s.saveSyncState(config, state)
		}
		s.webdav.operation.Unlock()
		if err != nil {
			fail(w, 500, "无法保存冲突处理结果")
			return
		}
		s.webdav.mu.Lock()
		s.webdav.status.Conflicts = slices.DeleteFunc(slices.Clone(s.webdav.status.Conflicts), func(conflict webdavConflict) bool {
			return conflict.Key == choice.Key && conflict.ID == choice.Conflict
		})
		s.webdav.mu.Unlock()
		if err = s.beginWebDAVSync(context.Background(), true); err != nil {
			fail(w, 409, err.Error())
			return
		}
		respond(w, 202, s.syncStatus())
	})
	mux.HandleFunc("POST /api/sync/webdav/rename", func(w http.ResponseWriter, r *http.Request) {
		var value struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &value) {
			return
		}
		config, err := s.readSyncConfig()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		if config.LibraryConfigured && config.Account != "" {
			if !validSyncFolder(value.Name) {
				fail(w, 400, "书库名称不能包含斜杠或控制字符")
				return
			}
			if err = atomicFile(s.syncNamePath(), []byte(value.Name)); err != nil {
				fail(w, 500, "无法保存云端书库新名称")
				return
			}
			s.webdav.mu.Lock()
			s.webdav.next = time.Time{}
			s.webdav.mu.Unlock()
		}
		respond(w, 200, map[string]bool{"queued": config.LibraryConfigured && config.Account != ""})
	})
}
