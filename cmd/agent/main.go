// surfswarm-agent connects to a surfswarm server, waits for test commands, and
// generates web traffic when told to. It runs in the foreground by default and
// can install itself as a launchd daemon (macOS), systemd unit (Linux), or
// Windows service so it checks in on boot.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/shin2344234/surfswarm/internal/wifi"
)

var version = "dev"

const serviceName = "surfswarm-agent"

// config is what the installed service reads. Flags override any field.
type config struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	Name     string `json:"name"`
	StateDir string `json:"state_dir"`
}

func main() {
	cmd, args := splitCommand(os.Args[1:])
	fs := flag.NewFlagSet(serviceName, flag.ExitOnError)
	server := fs.String("server", "", "server endpoint such as ws://192.168.1.10:8080/agent (default ws://127.0.0.1:8080/agent)")
	token := fs.String("token", "", "token the server expects")
	name := fs.String("name", "", "display name for this agent (default: hostname)")
	stateDir := fs.String("state-dir", "", "directory for the persistent agent id (default: user config dir, or a system dir when installed)")
	configPath := fs.String("config", "", "JSON config file with server, token, name, state_dir; flags override it")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() { usage(fs) }
	_ = fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *server != "" {
		cfg.Server = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *name != "" {
		cfg.Name = *name
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	// Environment variables fill anything still unset, for containers.
	if cfg.Server == "" {
		cfg.Server = os.Getenv("SURFSWARM_SERVER")
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("SURFSWARM_TOKEN")
	}
	if cfg.Name == "" {
		cfg.Name = os.Getenv("SURFSWARM_NAME")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = os.Getenv("SURFSWARM_STATE_DIR")
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}

	switch cmd {
	case "", "run":
		if cfg.Server == "" {
			cfg.Server = "ws://127.0.0.1:8080/agent"
		}
		if cfg.StateDir == "" {
			cfg.StateDir = userStateDir(cfg.Name)
		}
		runAgent(cfg)
	case "install":
		if err := install(cfg, *configPath); err != nil {
			log.Fatalf("install: %v", err)
		}
	case "uninstall":
		if err := uninstall(); err != nil {
			log.Fatalf("uninstall: %v", err)
		}
	case "start", "stop", "restart":
		if err := control(cmd); err != nil {
			log.Fatalf("%s: %v", cmd, err)
		}
		fmt.Printf("%s: ok\n", cmd)
	case "status":
		status()
	case "wifi":
		printWifi()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(fs)
		os.Exit(2)
	}
}

// splitCommand peels a leading subcommand off the arguments, so both
// "agent install -server x" and "agent -server x" parse.
func splitCommand(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, `surfswarm-agent %s

Usage:
  surfswarm-agent [flags]              run in the foreground
  surfswarm-agent install [flags]      copy this binary into place, write the config, and
                                       install a service that starts on boot (needs root or admin);
                                       re-running install upgrades an existing install
  surfswarm-agent uninstall            stop and remove the service; config and binary are kept
  surfswarm-agent start|stop|restart|status
  surfswarm-agent wifi                 print the Wi-Fi link this device would report, and how long
                                       the reading took (run with sudo on macOS to see the SSID)

Flags:
`, version)
	fs.PrintDefaults()
}

// printWifi shows the telemetry reading for support and debugging.
func printWifi() {
	start := time.Now()
	w, err := wifi.Current()
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("error after %s: %v\n", took, err)
		os.Exit(1)
	}
	if w == nil {
		fmt.Printf("no wireless link (wired, Wi-Fi off, or not associated); read took %s\n", took)
		return
	}
	b, _ := json.MarshalIndent(w, "", "  ")
	fmt.Printf("%s\nread took %s\n", b, took)
}

// ---- service lifecycle ----

// program adapts the agent to the service lifecycle. Foreground runs use the
// same path: service.Run calls Start, waits for SIGINT or SIGTERM, then Stop.
type program struct {
	cfg    config
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	a := newAgent(p.cfg)
	go func() {
		defer close(p.done)
		a.runForever(ctx)
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

func serviceConfig(args []string) *service.Config {
	opts := service.KeyValue{
		"KeepAlive": true,      // launchd: relaunch if it exits
		"RunAtLoad": true,      // launchd: start at boot
		"Restart":   "always",  // systemd
		"OnFailure": "restart", // windows
		"StartType": "automatic",
	}
	if runtime.GOOS == "darwin" {
		opts["LogOutput"] = true
		opts["LogDirectory"] = "/var/log"
	}
	return &service.Config{
		Name:        serviceName,
		DisplayName: "surfswarm agent",
		Description: "Generates HTTP/HTTPS traffic on command from a surfswarm server.",
		Arguments:   args,
		Option:      opts,
	}
}

func runAgent(cfg config) {
	if runtime.GOOS == "windows" && !service.Interactive() {
		// A Windows service has no stderr anyone can see.
		if f, err := openLogFile(cfg.StateDir); err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}
	s, err := service.New(&program{cfg: cfg}, serviceConfig(nil))
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
	log.Printf("agent exiting")
}

func install(cfg config, configPath string) error {
	if cfg.Server == "" {
		return errors.New("-server is required, for example -server ws://192.168.1.10:8080/agent")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = systemStateDir()
	}
	if configPath == "" {
		configPath = defaultConfigPath()
	}

	// Re-running install upgrades: stop and remove the existing service first.
	probe, err := service.New(&program{}, serviceConfig(nil))
	if err != nil {
		return err
	}
	if _, err := probe.Status(); err == nil {
		fmt.Printf("replacing existing %s service\n", serviceName)
		_ = probe.Stop()
		if err := probe.Uninstall(); err != nil {
			return fmt.Errorf("removing existing service: %w (run with sudo or as administrator)", err)
		}
	}

	exe, err := installBinary()
	if err != nil {
		return err
	}
	if err := writeConfig(configPath, cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}

	sc := serviceConfig([]string{"-config", configPath})
	sc.Executable = exe
	s, err := service.New(&program{}, sc)
	if err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		return fmt.Errorf("%w (run with sudo or as administrator)", err)
	}
	state := "installed and started"
	if err := s.Start(); err != nil {
		state = "installed but not started: " + err.Error()
	}
	fmt.Printf("%s: %s\n  binary:  %s\n  config:  %s\n  state:   %s\n  logs:    %s\n  check:   %s status\n",
		serviceName, state, exe, configPath, cfg.StateDir, logHint(cfg.StateDir), exe)
	return nil
}

func uninstall() error {
	s, err := service.New(&program{}, serviceConfig(nil))
	if err != nil {
		return err
	}
	if _, err := s.Status(); errors.Is(err, service.ErrNotInstalled) {
		fmt.Printf("%s is not installed\n", serviceName)
		return nil
	}
	_ = s.Stop()
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("%w (run with sudo or as administrator)", err)
	}
	fmt.Printf("%s removed; %s and %s were left in place\n", serviceName, defaultConfigPath(), installPath())
	return nil
}

func control(action string) error {
	s, err := service.New(&program{}, serviceConfig(nil))
	if err != nil {
		return err
	}
	return service.Control(s, action)
}

func status() {
	s, err := service.New(&program{}, serviceConfig(nil))
	if err != nil {
		log.Fatal(err)
	}
	st, err := s.Status()
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Printf("%s: not installed\n", serviceName)
	case err != nil:
		log.Fatalf("status: %v (run with sudo or as administrator)", err)
	case st == service.StatusRunning:
		fmt.Printf("%s: running\n", serviceName)
	case st == service.StatusStopped:
		fmt.Printf("%s: stopped\n", serviceName)
	default:
		fmt.Printf("%s: unknown\n", serviceName)
	}
}

// installBinary copies the running executable to the system location unless
// it is already there. It returns the installed path.
func installBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	target := installPath()
	if sameFile(self, target) {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("cannot create %s: %w (run with sudo or as administrator)", filepath.Dir(target), err)
	}
	src, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp := target + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("cannot write %s: %w (run with sudo or as administrator)", target, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return target, nil
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func loadConfig(path string) (config, error) {
	var cfg config
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func writeConfig(path string, cfg config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file holds the token.
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func openLogFile(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(stateDir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// ---- platform paths ----

func programData() string {
	if v := os.Getenv("ProgramData"); v != "" {
		return v
	}
	return `C:\ProgramData`
}

func programFiles() string {
	if v := os.Getenv("ProgramFiles"); v != "" {
		return v
	}
	return `C:\Program Files`
}

func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(programData(), "surfswarm", "agent.json")
	}
	return "/etc/surfswarm/agent.json"
}

func systemStateDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/surfswarm"
	case "windows":
		return filepath.Join(programData(), "surfswarm", "state")
	default:
		return "/var/lib/surfswarm"
	}
}

func installPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(programFiles(), "surfswarm", "surfswarm-agent.exe")
	}
	return "/usr/local/bin/surfswarm-agent"
}

func userStateDir(name string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "surfswarm", sanitize(name))
}

func logHint(stateDir string) string {
	switch runtime.GOOS {
	case "darwin":
		return "/var/log/surfswarm-agent.err.log"
	case "windows":
		return filepath.Join(stateDir, "agent.log")
	default:
		return "journalctl -u surfswarm-agent -f"
	}
}

// ---- identity ----

// loadOrCreateID returns a stable id for this agent, creating and persisting
// one on first run. Falls back to an ephemeral id if the state dir is unusable.
func loadOrCreateID(stateDir string) string {
	path := filepath.Join(stateDir, "agent-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	id := randomHex(16)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Printf("warning: cannot create state dir %s (%v); using an ephemeral id", stateDir, err)
		return id
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		log.Printf("warning: cannot write %s (%v); using an ephemeral id", path, err)
	}
	return id
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func localIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipn, ok := addr.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
