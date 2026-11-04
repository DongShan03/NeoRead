package neoread

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// PrepareStartupSettings snapshots the theme before recovery can occupy the
// store's single database connection. Read errors must not become a wrong theme.
func (s *Server) PrepareStartupSettings() error {
	var value string
	err := s.Store.DB.QueryRow("SELECT value FROM settings WHERE key='neoread'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		value = "{}"
	} else if err != nil {
		return err
	}
	if !json.Valid([]byte(value)) {
		return errors.New("保存的主题设置无效")
	}
	s.startupSettings = json.RawMessage(value)
	return nil
}

var applicationPreferenceKeys = []string{"neoread", "preferences:library", "preferences:guide", "preferences:shortcuts"}
var globalPaperPreferenceKeys = []string{"subcategoryItems", "sort", "sortReverse", "layout", "showAuthors", "cardScale", "columns", "showTitle", "columnOrder", "columnWidths", "detailWidth", "autoLookup", "citationStyle", "citationOrder"}

// Reader sessions and document zooms remain in their owning library. Library
// organization is data; only display/default/import preferences are shared.
func globalPreference(key string, value map[string]any) map[string]any {
	global := map[string]any{}
	copyKeys := func(from, to map[string]any, keys []string) {
		for _, key := range keys {
			if v, ok := from[key]; ok {
				to[key] = v
			}
		}
	}
	switch key {
	case "neoread":
		for k, v := range value {
			if k != "readingSession" && k != "documentZooms" {
				global[k] = v
			}
		}
	case "preferences:library":
		copyKeys(value, global, []string{"crossrefEmail", "primaryMode"})
		if papers, ok := value["papers"].(map[string]any); ok {
			shared := map[string]any{}
			copyKeys(papers, shared, globalPaperPreferenceKeys)
			global["papers"] = shared
		}
		if books, ok := value["books"].(map[string]any); ok {
			shared := map[string]any{}
			copyKeys(books, shared, []string{"detailWidth", "cardScale"})
			global["books"] = shared
		}
	case "preferences:guide":
		copyKeys(value, global, []string{"onboarded", "seenVersion"})
	case "preferences:shortcuts":
		for k, v := range value {
			global[k] = v
		}
	}
	return global
}

func mergeApplicationPreference(key string, local, shared map[string]any) map[string]any {
	// Remove old library-local copies of global values before applying the
	// authoritative application profile, including values reset to defaults.
	for k, v := range globalPreference(key, local) {
		if nested, ok := v.(map[string]any); ok {
			if existing, ok := local[k].(map[string]any); ok {
				for name := range nested {
					delete(existing, name)
				}
			}
		} else {
			delete(local, k)
		}
	}
	for k, v := range shared {
		if nested, ok := v.(map[string]any); ok {
			existing, ok := local[k].(map[string]any)
			if !ok {
				existing = map[string]any{}
			}
			for name, value := range nested {
				existing[name] = value
			}
			local[k] = existing
		} else {
			local[k] = v
		}
	}
	return local
}

func readPreference(db *sql.DB, key string) (map[string]any, error) {
	var body string
	err := db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if json.Unmarshal([]byte(body), &value) != nil || value == nil {
		return nil, errors.New("保存的设置无效")
	}
	return value, nil
}

func (s *Server) applicationPreferences() (map[string]map[string]any, error) {
	body, err := os.ReadFile(s.applicationSettingsPath)
	if err != nil {
		return nil, err
	}
	var profile map[string]map[string]any
	if json.Unmarshal(body, &profile) != nil || profile == nil {
		return nil, errors.New("应用设置无法读取")
	}
	return profile, nil
}

func (s *Server) readApplicationPreference(key string) (map[string]any, error) {
	local, err := readPreference(s.Store.DB, key)
	if err != nil {
		return nil, err
	}
	if s.applicationSettingsPath == "" {
		return local, nil
	}
	profile, err := s.applicationPreferences()
	if err != nil {
		return nil, err
	}
	return mergeApplicationPreference(key, local, profile[key]), nil
}

func (s *Server) saveApplicationPreference(key string, value map[string]any) error {
	s.preferencesMu.Lock()
	defer s.preferencesMu.Unlock()
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if s.applicationSettingsPath == "" {
		_, err = s.Store.DB.Exec("INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body))
		return err
	}
	profile, err := s.applicationPreferences()
	if err != nil {
		return err
	}
	previous, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	profile[key] = globalPreference(key, value)
	shared, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body)); err != nil {
		return err
	}
	if err = atomicFile(s.applicationSettingsPath, shared); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		if restore := atomicFile(s.applicationSettingsPath, previous); restore != nil {
			s.diagnosticEvent(slog.LevelError, "settings.profile_restore_failed", "errorCode", "save")
		}
		return err
	}
	s.diagnosticEvent(slog.LevelInfo, "settings.application_saved", "phase", key)
	return nil
}

func (s *Server) SetApplicationSettingsPath(name, fallback string) error {
	s.applicationSettingsPath = name
	if _, err := os.Stat(name); os.IsNotExist(err) {
		profile := map[string]map[string]any{}
		seed := s.Store.DB
		if fallback != "" && fallback != s.Store.Root {
			file := filepath.Join(fallback, "neoread.sqlite")
			if _, err := os.Stat(file); err == nil {
				uri := strings.Replace(libraryDatabaseURI(file), "mode=rw", "mode=ro", 1)
				if db, err := sql.Open("sqlite", uri); err == nil {
					defer db.Close()
					seed = db
				}
			}
		}
		for _, key := range applicationPreferenceKeys {
			value, err := readPreference(seed, key)
			if err != nil {
				return err
			}
			profile[key] = globalPreference(key, value)
		}
		body, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		if err = atomicFile(name, body); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Mirror the shared values once before startup workers read legacy settings
	// directly from SQLite; user data and library-specific state stay untouched.
	for _, key := range applicationPreferenceKeys {
		value, err := s.readApplicationPreference(key)
		if err != nil {
			return err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err = s.Store.DB.Exec("INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(body)); err != nil {
			return err
		}
	}
	if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('application:preferences-scope','shared') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); err != nil {
		return err
	}
	s.diagnosticEvent(slog.LevelInfo, "settings.application_ready")
	return nil
}
