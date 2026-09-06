package main

import (
	"path/filepath"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cmd, rest := splitCommand([]string{"install", "-server", "ws://x"})
	if cmd != "install" || len(rest) != 2 {
		t.Fatalf("got %q %v", cmd, rest)
	}
	cmd, rest = splitCommand([]string{"-server", "ws://x"})
	if cmd != "" || len(rest) != 2 {
		t.Fatalf("got %q %v", cmd, rest)
	}
	if cmd, rest := splitCommand(nil); cmd != "" || len(rest) != 0 {
		t.Fatalf("got %q %v", cmd, rest)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	in := config{Server: "ws://h:8080/agent", Token: "tok", Name: "pi-1", StateDir: "/var/lib/surfswarm"}
	if err := writeConfig(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file should error")
	}
	if cfg, err := loadConfig(""); err != nil || cfg != (config{}) {
		t.Fatalf("empty path: %+v %v", cfg, err)
	}
}

func TestLoadOrCreateIDIsStable(t *testing.T) {
	dir := t.TempDir()
	a := loadOrCreateID(dir)
	b := loadOrCreateID(dir)
	if a == "" || a != b {
		t.Fatalf("ids %q %q", a, b)
	}
	if c := loadOrCreateID(t.TempDir()); c == a {
		t.Fatal("different state dirs must give different ids")
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("Seth's MacBook Pro"); got != "Seth_s_MacBook_Pro" {
		t.Fatalf("got %q", got)
	}
	if got := sanitize(""); got != "default" {
		t.Fatalf("got %q", got)
	}
}

func TestOutboxOrderAndDropOldest(t *testing.T) {
	o := newOutbox(3)
	for _, s := range []string{"a", "b", "c", "d"} {
		o.push([]byte(s))
	}
	if o.len() != 3 || o.dropped != 1 {
		t.Fatalf("len %d dropped %d", o.len(), o.dropped)
	}
	got := ""
	for {
		b, ok := o.pop()
		if !ok {
			break
		}
		got += string(b)
	}
	if got != "bcd" {
		t.Fatalf("order %q", got)
	}
	o.push([]byte("x"))
	b, _ := o.pop()
	o.unpop(b)
	o.push([]byte("y"))
	b1, _ := o.pop()
	b2, _ := o.pop()
	if string(b1) != "x" || string(b2) != "y" {
		t.Fatalf("unpop order %q %q", b1, b2)
	}
	select {
	case <-o.wake:
	default:
		t.Fatal("push should signal wake")
	}
}
