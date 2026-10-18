// Package store keeps links and configs in memory and persists them as two
// JSON files (links.json, configs.json) using atomic writes. No database.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"subsvc/internal/model"
	"subsvc/internal/proxy"
	"subsvc/internal/settings"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("a link with this URL already exists")
)

type linksFile struct {
	Version int          `json:"version"`
	Links   []model.Link `json:"links"`
}

type configsFile struct {
	Version int           `json:"version"`
	Configs []model.Entry `json:"configs"`
}

// Store is safe for concurrent use.
type Store struct {
	dir string

	mu      sync.RWMutex
	links   []*model.Link
	entries map[string]*model.Entry // key = normalized URI
	dirty   bool                    // configs changed since last write
}

// Open loads (or creates) the store in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{dir: dir, entries: make(map[string]*model.Entry)}

	var lf linksFile
	if err := readJSON(filepath.Join(dir, "links.json"), &lf); err != nil {
		return nil, err
	}
	for i := range lf.Links {
		l := lf.Links[i]
		s.links = append(s.links, &l)
	}
	var cf configsFile
	if err := readJSON(filepath.Join(dir, "configs.json"), &cf); err != nil {
		return nil, err
	}
	for i := range cf.Configs {
		e := cf.Configs[i]
		e.Test.Normalize() // results stored by older versions have no General probe
		s.entries[e.Key] = &e
	}
	return s, nil
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s is corrupt (fix or remove it): %w", path, err)
	}
	return nil
}

// writeAtomic writes data to path via a temp file + rename so a crash can
// never leave a half-written file behind.
func writeAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil { // make the rename durable
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func (s *Store) saveLinksLocked() error {
	lf := linksFile{Version: 1, Links: make([]model.Link, 0, len(s.links))}
	for _, l := range s.links {
		lf.Links = append(lf.Links, *l)
	}
	return writeAtomic(filepath.Join(s.dir, "links.json"), lf)
}

func (s *Store) saveConfigsLocked() error {
	cf := configsFile{Version: 1, Configs: make([]model.Entry, 0, len(s.entries))}
	for _, e := range s.entries {
		cf.Configs = append(cf.Configs, *e)
	}
	sort.Slice(cf.Configs, func(i, j int) bool { return cf.Configs[i].Key < cf.Configs[j].Key })
	if err := writeAtomic(filepath.Join(s.dir, "configs.json"), cf); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Flush persists configs if they changed.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveConfigsLocked()
}

// ------------------------------------------------------------------- links

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func copyLink(l *model.Link) model.Link {
	c := *l
	if l.LastFetch != nil {
		f := *l.LastFetch
		c.LastFetch = &f
	}
	return c
}

// Links returns a copy of all links in creation order.
func (s *Store) Links() []model.Link {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Link, len(s.links))
	for i, l := range s.links {
		out[i] = copyLink(l)
	}
	return out
}

// Link returns one link.
func (s *Store) Link(id string) (model.Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, l := range s.links {
		if l.ID == id {
			return copyLink(l), nil
		}
	}
	return model.Link{}, ErrNotFound
}

// AddLink registers a new link.
func (s *Store) AddLink(name, url string) (model.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if l.URL == url {
			return model.Link{}, ErrDuplicate
		}
	}
	l := &model.Link{ID: newID(), Name: name, URL: url, Enabled: true, CreatedAt: time.Now().UTC()}
	s.links = append(s.links, l)
	if err := s.saveLinksLocked(); err != nil {
		s.links = s.links[:len(s.links)-1]
		return model.Link{}, err
	}
	return copyLink(l), nil
}

// LinkPatch holds optional changes for UpdateLink.
type LinkPatch struct {
	Name    *string
	URL     *string
	Enabled *bool
}

// UpdateLink applies p to the link with the given id.
func (s *Store) UpdateLink(id string, p LinkPatch) (model.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var l *model.Link
	for _, c := range s.links {
		if c.ID == id {
			l = c
		}
	}
	if l == nil {
		return model.Link{}, ErrNotFound
	}
	if p.URL != nil {
		for _, c := range s.links {
			if c.ID != id && c.URL == *p.URL {
				return model.Link{}, ErrDuplicate
			}
		}
	}
	old := *l
	if p.Name != nil {
		l.Name = *p.Name
	}
	if p.URL != nil {
		l.URL = *p.URL
	}
	if p.Enabled != nil {
		l.Enabled = *p.Enabled
	}
	if err := s.saveLinksLocked(); err != nil {
		*l = old
		return model.Link{}, err
	}
	return copyLink(l), nil
}

// DeleteLink removes a link and every config that came only from it.
func (s *Store) DeleteLink(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, l := range s.links {
		if l.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	s.links = append(s.links[:idx], s.links[idx+1:]...)
	for k, e := range s.entries {
		e.Sources = removeStr(e.Sources, id)
		if len(e.Sources) == 0 {
			delete(s.entries, k)
		}
	}
	s.dirty = true
	if err := s.saveLinksLocked(); err != nil {
		return err
	}
	return s.saveConfigsLocked()
}

// ------------------------------------------------------------------ fetches

func addStr(l []string, v string) []string {
	for _, x := range l {
		if x == v {
			return l
		}
	}
	return append(l, v)
}

func removeStr(l []string, v string) []string {
	out := l[:0]
	for _, x := range l {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// ApplyFetch replaces the configs contributed by linkID with nodes and records
// the fetch status. Configs that appear in several links are stored once.
// Existing test results are kept for configs that are still present.
func (s *Store) ApplyFetch(linkID string, nodes []*proxy.Node, st model.FetchStatus) (added int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var link *model.Link
	for _, l := range s.links {
		if l.ID == linkID {
			link = l
		}
	}
	if link == nil {
		return 0, ErrNotFound
	}
	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		seen[n.Key] = struct{}{}
		e := s.entries[n.Key]
		if e == nil {
			e = &model.Entry{Key: n.Key, FirstSeen: now}
			if !n.Supported() {
				e.Test.Error = n.Unsupported
			}
			s.entries[n.Key] = e
			added++
		}
		e.URI, e.Name, e.Protocol = n.Raw, n.Name, n.Protocol
		e.Host, e.Port, e.Unsupported = n.Host, n.Port, n.Unsupported
		e.Sources = addStr(e.Sources, linkID)
	}
	for k, e := range s.entries {
		if _, ok := seen[k]; ok {
			continue
		}
		e.Sources = removeStr(e.Sources, linkID)
		if len(e.Sources) == 0 {
			delete(s.entries, k)
		}
	}
	st.New = added
	link.LastFetch = &st
	s.dirty = true
	if err := s.saveLinksLocked(); err != nil {
		return added, err
	}
	return added, s.saveConfigsLocked()
}

// SetFetchError records a failed fetch without touching stored configs.
func (s *Store) SetFetchError(linkID string, st model.FetchStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if l.ID == linkID {
			l.LastFetch = &st
			return s.saveLinksLocked()
		}
	}
	return ErrNotFound
}

// ------------------------------------------------------------------ configs

// SetTest stores a test result. Call Flush to persist.
func (s *Store) SetTest(key string, tr model.TestResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		e.Test = tr
		s.dirty = true
	}
}

func copyEntry(e *model.Entry) model.Entry {
	c := *e
	c.Sources = append([]string(nil), e.Sources...)
	return c
}

// Entries returns copies of all entries sorted by key. If linkID is not empty
// only entries that belong to that link are returned.
func (s *Store) Entries(linkID string) []model.Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if linkID != "" && !contains(e.Sources, linkID) {
			continue
		}
		out = append(out, copyEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// EntriesForLinks returns entries that belong to at least one of ids.
func (s *Store) EntriesForLinks(ids []string) []model.Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Entry
	for _, e := range s.entries {
		for _, id := range ids {
			if contains(e.Sources, id) {
				out = append(out, copyEntry(e))
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Views returns every entry with its derived eligibility and score, sorted by
// key. linkID limits the result to one link.
func (s *Store) Views(linkID string, set settings.Settings) []settings.EntryView {
	all := s.Entries(linkID)
	out := make([]settings.EntryView, len(all))
	for i, e := range all {
		out[i] = set.View(e)
	}
	return out
}

// Selected returns the entries that meet the success criteria (the content of
// the output), best first according to the ranking settings.
func (s *Store) Selected(linkID string, set settings.Settings) []settings.EntryView {
	views := s.Views(linkID, set)
	out := views[:0]
	for _, v := range views {
		if v.Eligible {
			out = append(out, v)
		}
	}
	set.Rank(out)
	return out
}

// Stats returns summary counters. Eligible depends on the success criteria.
func (s *Store) Stats(set settings.Settings) model.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := model.Stats{Links: len(s.links), Configs: len(s.entries)}
	for _, e := range s.entries {
		if e.Unsupported != "" {
			st.Unsupported++
		}
		if !e.Test.Tested {
			continue
		}
		st.Tested++
		if g := e.Test.General; g != nil && g.OK {
			st.Reachable++
		}
		if y := e.Test.YouTube; y != nil && y.OK {
			st.YouTubeOK++
		}
		if sp := e.Test.Speed; sp != nil && sp.Tested {
			st.SpeedTested++
		}
		if set.Eligible(e.Test) {
			st.Eligible++
		}
	}
	return st
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// -------------------------------------------------------- delete operations

// DeleteEntry removes a single config by its normalized key.
// Returns ErrNotFound if the key does not exist.
func (s *Store) DeleteEntry(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; !ok {
		return ErrNotFound
	}
	delete(s.entries, key)
	s.dirty = true
	return s.saveConfigsLocked()
}

// DeleteEntries removes configs matching filter (and optionally only those
// belonging to linkID). Returns the number of fully removed entries.
//
// If linkID is set, only that link's membership is removed from each config.
// A config is deleted only when it no longer belongs to any link.
//
// Supported filters:
//   - "failed":      tested but not meeting the success criteria
//   - "untested":    not tested yet and supported (Unsupported == "")
//   - "unsupported": Unsupported != ""
//   - "unreachable": not meeting the success criteria (failed + untested + unsupported)
func (s *Store) DeleteEntries(filter, linkID string, set settings.Settings) (int, error) {
	switch filter {
	case "failed", "untested", "unsupported", "unreachable", "all":
	default:
		return 0, fmt.Errorf("unknown filter %q (want failed|untested|unsupported|unreachable|all)", filter)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	changed := false
	for k, e := range s.entries {
		if linkID != "" && !contains(e.Sources, linkID) {
			continue
		}
		if !matchDeleteFilter(e, filter, set) {
			continue
		}
		changed = true
		if linkID != "" {
			e.Sources = removeStr(e.Sources, linkID)
			if len(e.Sources) > 0 {
				continue
			}
		}
		delete(s.entries, k)
		removed++
	}
	if !changed {
		return 0, nil
	}
	s.dirty = true
	if err := s.saveConfigsLocked(); err != nil {
		return removed, err
	}
	return removed, nil
}

// matchDeleteFilter: "failed" = tested but not meeting the success criteria;
// "unreachable" = everything that is not in the output (failed + untested +
// unsupported).
func matchDeleteFilter(e *model.Entry, filter string, set settings.Settings) bool {
	switch filter {
	case "failed":
		return e.Test.Tested && !set.Eligible(e.Test)
	case "untested":
		return !e.Test.Tested && e.Unsupported == ""
	case "unsupported":
		return e.Unsupported != ""
	case "unreachable":
		return !set.Eligible(e.Test)
	case "all":
		return true
	}
	return false
}
