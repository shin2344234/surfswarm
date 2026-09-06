package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testDefaults = map[string]string{
	"browse":   "# bundled\nhttps://a.example/\nhttps://b.example/\n",
	"download": "https://big.example/100MB.bin\n",
}

func newStore(t *testing.T) (*ListStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewListStore(dir, testDefaults)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSaveRejectsBadLinesAndNames(t *testing.T) {
	s, _ := newStore(t)
	_, problems, err := s.Save("custom", "https://ok.example/\nnot a url\nftp://x.example/\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "line 2") || !strings.Contains(problems[1], "line 3") {
		t.Fatalf("problems: %v", problems)
	}
	if _, problems, _ := s.Save("Mixed", "https://ok.example/\n"); len(problems) != 1 {
		t.Fatalf("bad name accepted: %v", problems)
	}
	if _, problems, _ := s.Save("mixed", "https://ok.example/\n"); len(problems) != 1 {
		t.Fatalf("reserved name accepted: %v", problems)
	}
	if _, problems, _ := s.Save("empty", "# nothing here\n"); len(problems) != 1 {
		t.Fatalf("empty list accepted: %v", problems)
	}
	if _, ok := s.URLs("custom"); ok {
		t.Fatal("rejected save must not create the list")
	}
}

func TestSaveResetAndPersistence(t *testing.T) {
	s, dir := newStore(t)
	info, problems, err := s.Save("custom", "https://c.example/\n# note\nhttps://d.example/")
	if err != nil || len(problems) != 0 {
		t.Fatalf("save: %v %v", err, problems)
	}
	if info.Count != 2 || info.Builtin || info.Modified {
		t.Fatalf("info: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, "custom.txt")); err != nil {
		t.Fatalf("file not written: %v", err)
	}

	// Edit a bundled list, then confirm it reads as modified.
	info, problems, err = s.Save("browse", "https://a.example/\n")
	if err != nil || len(problems) != 0 || !info.Builtin || !info.Modified || info.Count != 1 {
		t.Fatalf("edit bundled: %+v %v %v", info, problems, err)
	}

	// A fresh store on the same directory sees both.
	again, err := NewListStore(dir, testDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if urls, ok := again.URLs("custom"); !ok || len(urls) != 2 {
		t.Fatalf("custom not reloaded: %v %v", urls, ok)
	}
	if urls, _ := again.URLs("browse"); len(urls) != 1 {
		t.Fatalf("browse edit not reloaded: %v", urls)
	}

	// Reset restores the bundled text and deletes the custom list.
	info, existed, err := again.Reset("browse")
	if err != nil || !existed || info.Modified || info.Count != 2 {
		t.Fatalf("reset bundled: %+v %v %v", info, existed, err)
	}
	if _, existed, _ := again.Reset("custom"); !existed {
		t.Fatal("custom should have existed")
	}
	if _, ok := again.URLs("custom"); ok {
		t.Fatal("custom should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "custom.txt")); !os.IsNotExist(err) {
		t.Fatal("custom file should be removed")
	}
	if _, existed, _ := again.Reset("nope"); existed {
		t.Fatal("unknown list should not exist")
	}
}

func TestInfoOrderAndURLMap(t *testing.T) {
	s, _ := newStore(t)
	_, _, _ = s.Save("zeta", "https://z.example/\n")
	_, _, _ = s.Save("alpha", "https://a.example/\n")
	var names []string
	for _, li := range s.Info() {
		names = append(names, li.Name)
	}
	want := "browse,download,alpha,zeta"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	m := s.URLMap()
	if len(m["browse"]) != 2 || len(m["zeta"]) != 1 {
		t.Fatalf("url map: %v", m)
	}
}

func TestResolveList(t *testing.T) {
	s, _ := newStore(t)
	tm := NewTestManager(NewHub(func() {}), newSubscribers(), s)
	name, urls, err := tm.resolveList("")
	if err != nil || name != "browse" || len(urls) != 2 {
		t.Fatalf("default: %v %v %v", name, urls, err)
	}
	name, urls, err = tm.resolveList("mixed")
	if err != nil || name != "mixed" || len(urls) != 3 {
		t.Fatalf("mixed: %v %v %v", name, urls, err)
	}
	if _, _, err := tm.resolveList("nope"); err == nil {
		t.Fatal("unknown list should error")
	}
}
