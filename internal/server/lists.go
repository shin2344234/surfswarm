package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shin2344234/surfswarm/internal/engine"
)

// listNameRe limits list names to something safe for a file name and a URL.
var listNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// ListInfo summarizes one URL list.
type ListInfo struct {
	Name      string    `json:"name"`
	Count     int       `json:"count"`
	Builtin   bool      `json:"builtin"`
	Modified  bool      `json:"modified"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// CheckJob is one run of the URL checker over a list. Results arrive in
// completion order while it runs.
type CheckJob struct {
	List      string               `json:"list"`
	StartedAt time.Time            `json:"started_at"`
	EndedAt   *time.Time           `json:"ended_at,omitempty"`
	Done      bool                 `json:"done"`
	Total     int                  `json:"total"`
	Completed int                  `json:"completed"`
	Summary   map[string]int       `json:"summary"`
	Results   []engine.CheckResult `json:"results"`
}

// ListStore holds the URL lists agents can be given: the bundled defaults
// plus any edits or custom lists, saved under dir as <name>.txt so they
// survive a restart. "mixed" is not a stored list; the test manager builds
// it from browse and download.
type ListStore struct {
	dir      string
	defaults map[string]string

	mu      sync.RWMutex
	texts   map[string]string
	updated map[string]time.Time
	checks  map[string]*CheckJob
}

// NewListStore loads saved lists from dir on top of the bundled defaults.
// An empty dir keeps everything in memory.
func NewListStore(dir string, defaults map[string]string) (*ListStore, error) {
	s := &ListStore{
		dir:      dir,
		defaults: defaults,
		texts:    map[string]string{},
		updated:  map[string]time.Time{},
		checks:   map[string]*CheckJob{},
	}
	for name, text := range defaults {
		s.texts[name] = text
	}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".txt")
		if !listNameRe.MatchString(name) || name == "mixed" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		s.texts[name] = string(b)
		if fi, err := e.Info(); err == nil {
			s.updated[name] = fi.ModTime()
		}
	}
	return s, nil
}

func listRank(name string) int {
	switch name {
	case "browse":
		return 0
	case "download":
		return 1
	case "max":
		return 2
	}
	return 3
}

func (s *ListStore) info(name string) ListInfo {
	text := s.texts[name]
	def, builtin := s.defaults[name]
	return ListInfo{
		Name:      name,
		Count:     len(parseURLList(text)),
		Builtin:   builtin,
		Modified:  builtin && text != def,
		UpdatedAt: s.updated[name],
	}
}

// Info lists every list, bundled ones first.
func (s *ListStore) Info() []ListInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.texts))
	for n := range s.texts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := listRank(names[i]), listRank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	out := make([]ListInfo, 0, len(names))
	for _, n := range names {
		out = append(out, s.info(n))
	}
	return out
}

// Text returns a list's raw text, comments included.
func (s *ListStore) Text(name string) (string, ListInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text, ok := s.texts[name]
	if !ok {
		return "", ListInfo{}, false
	}
	return text, s.info(name), true
}

// URLs returns a list's URLs with comments and blank lines stripped.
func (s *ListStore) URLs(name string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	text, ok := s.texts[name]
	if !ok {
		return nil, false
	}
	return parseURLList(text), true
}

// URLMap returns every list's URLs keyed by name.
func (s *ListStore) URLMap() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.texts))
	for name, text := range s.texts {
		out[name] = parseURLList(text)
	}
	return out
}

// Save validates and stores a list. Lines that are not http or https URLs
// are returned as problems, with nothing written.
func (s *ListStore) Save(name, text string) (ListInfo, []string, error) {
	if !listNameRe.MatchString(name) || name == "mixed" {
		return ListInfo{}, []string{"list names are 1 to 40 lowercase letters, digits, dashes or underscores, and \"mixed\" is reserved"}, nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var problems []string
	count := 0
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(line, " \t") {
			problems = append(problems, fmt.Sprintf("line %d is not an http or https URL: %s", i+1, line))
			continue
		}
		count++
	}
	if len(problems) > 0 {
		return ListInfo{}, problems, nil
	}
	if count == 0 {
		return ListInfo{}, []string{"the list has no URLs; add at least one line that is not a comment"}, nil
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		if err := os.WriteFile(filepath.Join(s.dir, name+".txt"), []byte(text), 0o644); err != nil {
			return ListInfo{}, nil, err
		}
	}
	s.texts[name] = text
	s.updated[name] = time.Now()
	return s.info(name), nil, nil
}

// Reset restores a bundled list to its shipped contents, or deletes a
// custom list. It reports whether the list existed.
func (s *ListStore) Reset(name string) (ListInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.texts[name]; !ok {
		return ListInfo{}, false, nil
	}
	if s.dir != "" {
		if err := os.Remove(filepath.Join(s.dir, name+".txt")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ListInfo{}, true, err
		}
	}
	delete(s.updated, name)
	delete(s.checks, name)
	if def, ok := s.defaults[name]; ok {
		s.texts[name] = def
		return s.info(name), true, nil
	}
	delete(s.texts, name)
	return ListInfo{Name: name}, true, nil
}

// StartCheck fetches every URL in a list once, in the background, reading
// at most 2 MB of each so download lists finish quickly. If a check is
// already running for the list, that one is returned instead.
func (s *ListStore) StartCheck(name string) (*CheckJob, bool) {
	s.mu.Lock()
	text, ok := s.texts[name]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	if j := s.checks[name]; j != nil && !j.Done {
		cp := copyJob(j)
		s.mu.Unlock()
		return cp, true
	}
	urls := parseURLList(text)
	job := &CheckJob{List: name, StartedAt: time.Now(), Total: len(urls), Summary: map[string]int{}}
	s.checks[name] = job
	s.mu.Unlock()
	go s.runCheck(job, urls)
	return copyJob(job), true
}

// Check returns the latest check for a list, if any.
func (s *ListStore) Check(name string) (*CheckJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j := s.checks[name]
	if j == nil {
		return nil, false
	}
	return copyJob(j), true
}

func (s *ListStore) runCheck(job *CheckJob, urls []string) {
	const workers = 8
	client := &http.Client{Transport: engine.NewTransport(workers)}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(u string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := engine.CheckURL(client, u, 15*time.Second, 2<<20)
			key := r.Class
			if key == "" {
				key = "ok"
			}
			s.mu.Lock()
			job.Results = append(job.Results, r)
			job.Summary[key]++
			job.Completed++
			s.mu.Unlock()
		}(u)
	}
	wg.Wait()
	client.CloseIdleConnections()
	end := time.Now()
	s.mu.Lock()
	job.EndedAt = &end
	job.Done = true
	s.mu.Unlock()
}

func copyJob(j *CheckJob) *CheckJob {
	cp := *j
	cp.Results = append([]engine.CheckResult(nil), j.Results...)
	cp.Summary = make(map[string]int, len(j.Summary))
	for k, v := range j.Summary {
		cp.Summary[k] = v
	}
	return &cp
}
