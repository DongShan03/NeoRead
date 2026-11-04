package neoread

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectionAutoCopySettingPersistsBothStates(t *testing.T) {
	s := testServer(t)
	s.Token = "test-secret"
	for _, enabled := range []bool{true, false} {
		body, _ := json.Marshal(map[string]any{"autoCopySelection": enabled, "appearance": "dark"})
		if w := request(t, s, "PUT", "/api/settings", strings.NewReader(string(body))); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		// Verify persisted storage, as well as the settings response used on startup.
		var stored string
		if err := s.Store.DB.QueryRow("SELECT value FROM settings WHERE key='neoread'").Scan(&stored); err != nil {
			t.Fatal(err)
		}
		response := request(t, s, "GET", "/api/settings", nil)
		for _, data := range []string{stored, response.Body.String()} {
			var saved map[string]any
			if err := json.Unmarshal([]byte(data), &saved); err != nil || saved["autoCopySelection"] != enabled || saved["appearance"] != "dark" {
				t.Fatalf("selection copy setting not retained: %s", data)
			}
		}
	}
}

func TestApplicationPreferencesShareBasicsButPreserveLibrarySessionsAndCategories(t *testing.T) {
	a, _ := processingFixture(t)
	b, _ := processingFixture(t)
	for _, s := range []*Server{a, b} {
		if _, err := s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('neoread',?),('preferences:library',?)", `{"appearance":"dark","fontSize":18,"readingSession":{"documentIds":["doc"],"activeId":"doc"},"documentZooms":{"doc":2}}`, `{"papers":{"categories":["本库分类"],"showAuthors":true}}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Store.DB.Exec("UPDATE settings SET value=? WHERE key='neoread'", `{"appearance":"sepia","fontSize":12,"readingSession":{"documentIds":["other"],"activeId":"other"},"documentZooms":{"other":3}}`); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(t.TempDir(), "application-settings.json")
	if err := a.SetApplicationSettingsPath(shared, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.SetApplicationSettingsPath(shared, ""); err != nil {
		t.Fatal(err)
	}
	value, err := b.readApplicationPreference("neoread")
	if err != nil {
		t.Fatal(err)
	}
	if value["appearance"] != "dark" || value["fontSize"] != float64(18) || value["readingSession"].(map[string]any)["activeId"] != "other" || value["documentZooms"].(map[string]any)["other"] != float64(3) {
		t.Fatal("global defaults or local reading state changed", value)
	}
	value["fontSize"] = float64(21)
	if err := b.saveApplicationPreference("neoread", value); err != nil {
		t.Fatal(err)
	}
	other, err := a.readApplicationPreference("neoread")
	if err != nil || other["fontSize"] != float64(21) || other["readingSession"].(map[string]any)["activeId"] != "doc" {
		t.Fatal(other, err)
	}
	prefs, err := b.readApplicationPreference("preferences:library")
	if err != nil {
		t.Fatal(err)
	}
	if prefs["papers"].(map[string]any)["categories"].([]any)[0] != "本库分类" {
		t.Fatal("library organization was lost")
	}
	profile, err := b.applicationPreferences()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := profile["neoread"]["readingSession"]; ok {
		t.Fatal("reading session leaked into global profile")
	}
	if _, ok := profile["preferences:library"]["papers"].(map[string]any)["categories"]; ok {
		t.Fatal("categories leaked into global profile")
	}
}
