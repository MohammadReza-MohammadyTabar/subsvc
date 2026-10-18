package store

import (
	"os"
	"path/filepath"
	"testing"

	"subsvc/internal/settings"
)

// Results written by the previous version have only the flat fields. They must
// be upgraded on load, and must not count as eligible while YouTube is required.
func TestLegacyResultsAreUpgraded(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"configs":[
	 {"key":"k1","uri":"vless://a","protocol":"vless","sources":["l1"],"first_seen":"2026-01-01T00:00:00Z",
	  "test":{"tested":true,"reachable":true,"latency_ms":80,"connect_ms":50,"response_ms":30,"status_code":204}},
	 {"key":"k2","uri":"vless://b","protocol":"vless","sources":["l1"],"first_seen":"2026-01-01T00:00:00Z",
	  "test":{"tested":true,"reachable":false,"error":"timeout after 5000ms"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "configs.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	set := settings.Default()
	byKey := map[string]settings.EntryView{}
	for _, v := range st.Views("", set) {
		byKey[v.Key] = v
	}
	if g := byKey["k1"].Test.General; g == nil || !g.OK || g.LatencyMS != 80 || g.StatusCode != 204 {
		t.Errorf("k1 general: %+v", g)
	}
	if g := byKey["k2"].Test.General; g == nil || g.OK || g.Error == "" {
		t.Errorf("k2 general: %+v", g)
	}
	if byKey["k1"].Eligible {
		t.Error("a legacy result has no YouTube verdict: not eligible while require_youtube is on")
	}
	set.YouTubeEnabled, set.RequireYouTube, set.SpeedEnabled = false, false, false
	if !st.Views("", set)[0].Eligible && !byKey["k1"].Eligible {
		// k1 sorts first by key
		t.Error("with YouTube optional the legacy reachable config is eligible")
	}
	if got := st.Stats(set); got.Reachable != 1 || got.Tested != 2 || got.Eligible != 1 {
		t.Errorf("stats: %+v", got)
	}
}

func TestDeleteFiltersFollowTheCriteria(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"configs":[
	 {"key":"ok","uri":"vless://a","protocol":"vless","sources":["l1"],"test":{"tested":true,"reachable":true}},
	 {"key":"bad","uri":"vless://b","protocol":"vless","sources":["l1"],"test":{"tested":true}},
	 {"key":"new","uri":"vless://c","protocol":"vless","sources":["l1"],"test":{}},
	 {"key":"uns","uri":"hysteria2://d","protocol":"hysteria2","unsupported":"x","sources":["l1"],"test":{}}]}`
	os.WriteFile(filepath.Join(dir, "configs.json"), []byte(legacy), 0o600)
	st, _ := Open(dir)
	set := settings.Default()
	set.YouTubeEnabled, set.RequireYouTube, set.SpeedEnabled = false, false, false

	for filter, want := range map[string]int{"failed": 1, "untested": 1, "unsupported": 1} {
		n, err := st.DeleteEntries(filter, "", set)
		if err != nil || n != want {
			t.Errorf("%s: deleted %d (%v), want %d", filter, n, err, want)
		}
	}
	if left := st.Entries(""); len(left) != 1 || left[0].Key != "ok" {
		t.Errorf("only the healthy config must remain: %+v", left)
	}
	if _, err := st.DeleteEntries("bogus", "", set); err == nil {
		t.Error("unknown filter must be rejected")
	}
}
