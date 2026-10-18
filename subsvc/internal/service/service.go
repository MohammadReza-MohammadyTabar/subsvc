// Package service orchestrates manual refresh and test jobs. At most one job
// runs at a time, and a job only exists because someone asked for it through
// the API: there is no scheduler and no background polling.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"subsvc/internal/model"
	"subsvc/internal/proxy"
	"subsvc/internal/settings"
	"subsvc/internal/store"
	"subsvc/internal/subscription"
	"subsvc/internal/tester"
)

var (
	// ErrBusy is returned when a job is already running.
	ErrBusy = errors.New("a job is already running")
	// ErrNotFound is returned for unknown link IDs.
	ErrNotFound = store.ErrNotFound
)

// Fetcher downloads a subscription.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (*subscription.Result, error)
}

// FetcherFactory builds a Fetcher from the settings snapshot of a job, so that
// changes made in the UI apply to the next job.
type FetcherFactory func(s settings.Settings) Fetcher

// Prober tests one config through its own proxy.
type Prober interface {
	Test(ctx context.Context, n *proxy.Node, s settings.Settings, gate tester.Gate) model.TestResult
}

// Job describes a running or finished job.
type Job struct {
	ID          int        `json:"id"`
	Kind        string     `json:"kind"`  // refresh | test
	State       string     `json:"state"` // running | done | cancelled
	Phase       string     `json:"phase"` // fetching | testing | finished
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	LinksTotal  int        `json:"links_total"`
	LinksDone   int        `json:"links_done"`
	LinksFail   int        `json:"links_failed"`
	TestTotal   int        `json:"tests_total"`
	TestDone    int        `json:"tests_done"`
	TestOK      int        `json:"tests_reachable"` // general probe succeeded
	TestYT      int        `json:"tests_youtube_ok"`
	TestSpeed   int        `json:"tests_speed_done"` // speed downloads finished (ok or not)
	TestSpeedOK int        `json:"tests_speed_ok"`
	TestElig    int        `json:"tests_eligible"` // meet the success criteria
	Plan        *Plan      `json:"plan,omitempty"` // what this job tests
	Errors      []string   `json:"errors,omitempty"`
}

// Plan is the settings snapshot a job runs with.
type Plan struct {
	Concurrency      int  `json:"concurrency"`
	YouTube          bool `json:"youtube"`
	Speed            bool `json:"speed"`
	SpeedConcurrency int  `json:"speed_concurrency"`
}

// Status is returned by the status endpoint.
type Status struct {
	Job   *Job         `json:"job"`
	Stats model.Stats  `json:"stats"`
	Links []model.Link `json:"links"`
}

// Service runs jobs.
type Service struct {
	log   *slog.Logger
	st    *store.Store
	fetch FetcherFactory
	probe Prober
	cfg   *settings.Manager

	mu     sync.Mutex
	job    *Job // current or last job
	cancel context.CancelFunc
	nextID int
	wg     sync.WaitGroup
}

// New creates a Service.
func New(log *slog.Logger, st *store.Store, f FetcherFactory, p Prober, cfg *settings.Manager) *Service {
	return &Service{log: log, st: st, fetch: f, probe: p, cfg: cfg}
}

// Store exposes the underlying store (read access for the API layer).
func (s *Service) Store() *store.Store { return s.st }

// Status returns the current job (if any), totals and link states.
func (s *Service) Status() Status {
	s.mu.Lock()
	var j *Job
	if s.job != nil {
		c := *s.job
		c.Errors = append([]string(nil), s.job.Errors...)
		j = &c
	}
	s.mu.Unlock()
	return Status{Job: j, Stats: s.st.Stats(s.cfg.Get()), Links: s.st.Links()}
}

// Cancel stops the running job. It reports whether a job was running.
func (s *Service) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.State == "running" && s.cancel != nil {
		s.cancel()
		return true
	}
	return false
}

// Shutdown cancels the running job and waits for it to finish.
func (s *Service) Shutdown(ctx context.Context) {
	s.Cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// begin registers a new job or fails with ErrBusy.
func (s *Service) begin(kind string) (*Job, context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.State == "running" {
		return nil, nil, ErrBusy
	}
	s.nextID++
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.job = &Job{ID: s.nextID, Kind: kind, State: "running", Phase: "fetching", StartedAt: time.Now().UTC()}
	if kind == "test" {
		s.job.Phase = "testing"
	}
	s.wg.Add(1)
	return s.job, ctx, nil
}

func (s *Service) update(fn func(j *Job)) {
	s.mu.Lock()
	fn(s.job)
	s.mu.Unlock()
}

func (s *Service) finish(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.job.FinishedAt = &now
	s.job.Phase = "finished"
	if ctx.Err() != nil {
		s.job.State = "cancelled"
	} else {
		s.job.State = "done"
	}
	s.cancel()
	s.wg.Done()
}

func (s *Service) addError(msg string) {
	s.update(func(j *Job) {
		if len(j.Errors) < 50 {
			j.Errors = append(j.Errors, msg)
		}
	})
}

// StartRefresh starts a job that downloads subscriptions (all enabled links,
// or just linkID) and optionally tests the resulting configs.
func (s *Service) StartRefresh(linkID string, test bool) (Job, error) {
	links, err := s.selectLinks(linkID)
	if err != nil {
		return Job{}, err
	}
	job, ctx, err := s.begin("refresh")
	if err != nil {
		return Job{}, err
	}
	set := s.snapshot()
	snap := *job
	go func() {
		defer s.finish(ctx)
		defer s.recoverJob()
		s.update(func(j *Job) { j.LinksTotal = len(links) })
		s.fetchAll(ctx, links, set)
		if test && ctx.Err() == nil {
			ids := make([]string, len(links))
			for i, l := range links {
				ids[i] = l.ID
			}
			s.runTests(ctx, s.st.EntriesForLinks(ids), false, set)
		}
	}()
	return snap, nil
}

// StartTest starts a job that tests already stored configs. linkID limits it to
// one link; onlyUntested skips configs that already have a result.
func (s *Service) StartTest(linkID string, onlyUntested bool) (Job, error) {
	if linkID != "" {
		if _, err := s.st.Link(linkID); err != nil {
			return Job{}, err
		}
	}
	job, ctx, err := s.begin("test")
	if err != nil {
		return Job{}, err
	}
	set := s.snapshot()
	snap := *job
	go func() {
		defer s.finish(ctx)
		defer s.recoverJob()
		s.runTests(ctx, s.st.Entries(linkID), onlyUntested, set)
	}()
	return snap, nil
}

// snapshot takes the settings a job runs with and records the plan in the job,
// so a change made in the UI never alters a job that is already running.
func (s *Service) snapshot() settings.Settings {
	set := s.cfg.Get()
	s.update(func(j *Job) {
		j.Plan = &Plan{Concurrency: set.TestConcurrency, YouTube: set.YouTubeEnabled,
			Speed: set.SpeedEnabled, SpeedConcurrency: set.SpeedConcurrency}
	})
	return set
}

func (s *Service) recoverJob() {
	if r := recover(); r != nil {
		s.log.Error("job panicked", "panic", fmt.Sprint(r))
		s.addError(fmt.Sprintf("internal error: %v", r))
	}
}

func (s *Service) selectLinks(linkID string) ([]model.Link, error) {
	if linkID != "" {
		l, err := s.st.Link(linkID)
		if err != nil {
			return nil, err
		}
		return []model.Link{l}, nil
	}
	var out []model.Link
	for _, l := range s.st.Links() {
		if l.Enabled {
			out = append(out, l)
		}
	}
	return out, nil
}

// fetchAll downloads the given links with limited concurrency. A failing link
// is recorded and skipped; its previously stored configs are left untouched.
func (s *Service) fetchAll(ctx context.Context, links []model.Link, set settings.Settings) {
	fetcher := s.fetch(set)
	sem := make(chan struct{}, set.FetchConcurrency)
	var wg sync.WaitGroup
	for _, l := range links {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(l model.Link) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("fetch panicked", "link", l.ID, "panic", fmt.Sprint(r))
					s.update(func(j *Job) { j.LinksDone++; j.LinksFail++ })
				}
			}()
			s.fetchOne(ctx, fetcher, l)
		}(l)
	}
	wg.Wait()
}

func (s *Service) fetchOne(ctx context.Context, fetcher Fetcher, l model.Link) {
	start := time.Now()
	res, err := fetcher.Fetch(ctx, l.URL)
	st := model.FetchStatus{At: start.UTC(), DurationMS: time.Since(start).Milliseconds()}
	if res != nil {
		st.HTTPStatus, st.Bytes = res.HTTPStatus, res.Bytes
		st.Total, st.Invalid, st.Duplicates, st.Valid = res.Total, res.Invalid, res.Duplicates, len(res.Nodes)
	}
	if err != nil {
		st.Error = err.Error()
		s.log.Warn("fetch failed", "link", l.ID, "name", l.Name, "error", st.Error)
		if serr := s.st.SetFetchError(l.ID, st); serr != nil && !errors.Is(serr, store.ErrNotFound) {
			s.log.Error("save fetch status", "error", serr)
		}
		s.addError(fmt.Sprintf("link %s (%s): %s", l.ID, l.Name, st.Error))
		s.update(func(j *Job) { j.LinksDone++; j.LinksFail++ })
		return
	}
	st.OK = true
	added, aerr := s.st.ApplyFetch(l.ID, res.Nodes, st)
	if aerr != nil {
		s.log.Error("store fetch result", "link", l.ID, "error", aerr)
		s.addError(fmt.Sprintf("link %s (%s): %v", l.ID, l.Name, aerr))
		s.update(func(j *Job) { j.LinksDone++; j.LinksFail++ })
		return
	}
	s.log.Info("fetched", "link", l.ID, "name", l.Name, "valid", st.Valid,
		"invalid", st.Invalid, "dups", st.Duplicates, "new", added, "ms", st.DurationMS)
	s.update(func(j *Job) { j.LinksDone++ })
}

// runTests tests entries with at most set.TestConcurrency configs in parallel
// (one core process each) and set.SpeedConcurrency speed downloads at a time.
func (s *Service) runTests(ctx context.Context, entries []model.Entry, onlyUntested bool, set settings.Settings) {
	var todo []model.Entry
	for _, e := range entries {
		if e.Unsupported != "" {
			continue // cannot be tested; never part of the output
		}
		if onlyUntested && e.Test.TestedAt != nil {
			continue
		}
		todo = append(todo, e)
	}
	s.update(func(j *Job) { j.Phase = "testing"; j.TestTotal = len(todo) })
	if len(todo) == 0 {
		return
	}

	gate := tester.NewGate(set.SpeedConcurrency)
	ch := make(chan model.Entry)
	var wg sync.WaitGroup
	workers := set.TestConcurrency
	if workers > len(todo) {
		workers = len(todo)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range ch {
				s.testOne(ctx, e, set, gate)
			}
		}()
	}
feed:
	for _, e := range todo {
		select {
		case ch <- e:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if err := s.st.Flush(); err != nil {
		s.log.Error("save test results", "error", err)
		s.addError("save test results: " + err.Error())
	}
}

func (s *Service) testOne(ctx context.Context, e model.Entry, set settings.Settings, gate tester.Gate) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("test panicked", "panic", fmt.Sprint(r))
			s.update(func(j *Job) { j.TestDone++ })
		}
	}()
	if ctx.Err() != nil {
		return
	}
	var res model.TestResult
	n, err := proxy.Parse(e.URI)
	if err != nil {
		now := time.Now().UTC()
		res = model.TestResult{Tested: true, TestedAt: &now,
			General: &model.Probe{Tested: true, Error: "invalid config: " + err.Error(), ErrorKind: model.ErrKindOther}}
		res.SyncLegacy()
	} else {
		res = s.probe.Test(ctx, n, set, gate)
	}
	if ctx.Err() != nil {
		return // cancelled mid-test: a partial result must not replace the previous one
	}
	s.st.SetTest(e.Key, res)

	g, y, sp := res.General, res.YouTube, res.Speed
	s.log.Debug("tested", "host", e.Host, "port", e.Port, "protocol", e.Protocol,
		"general", probeLog(g), "youtube", probeLog(y), "speed", speedLog(sp))
	s.update(func(j *Job) {
		j.TestDone++
		if g != nil && g.OK {
			j.TestOK++
		}
		if y != nil && y.OK {
			j.TestYT++
		}
		if sp != nil && sp.Tested {
			j.TestSpeed++
			if sp.OK {
				j.TestSpeedOK++
			}
		}
		if set.Eligible(res) {
			j.TestElig++
		}
	})
	if s.progress()%20 == 0 {
		if err := s.st.Flush(); err != nil {
			s.log.Error("save test results", "error", err)
		}
	}
}

func probeLog(p *model.Probe) string {
	switch {
	case p == nil:
		return "-"
	case p.OK:
		return fmt.Sprintf("ok %dms", p.LatencyMS)
	default:
		return p.ErrorKind
	}
}

func speedLog(sp *model.Speed) string {
	switch {
	case sp == nil || !sp.Tested:
		return "-"
	case sp.OK:
		return fmt.Sprintf("%.2fMbps", sp.Mbps)
	default:
		return sp.ErrorKind
	}
}

func (s *Service) progress() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.job.TestDone
}
