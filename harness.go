// Package integrationtesting provides an integration test harness for Slidebolt plugins.
// It boots a complete stack (NATS + gateway + your plugin) via the launcher using
// LAUNCHER_API_PORT=0 / LAUNCHER_NATS_PORT=0 so the OS picks free ports, then
// reads the runtime.json the gateway writes to discover the actual URLs.
//
// Parallel-safe: every Suite gets its own temp dir and OS-assigned ports, so
// multiple plugin test packages can run concurrently without any coordination.
//
// Usage:
//
//	func TestIntegration(t *testing.T) {
//	    s := integrationtesting.New(t, "plugin-test-clean", ".")
//	    s.RequirePlugin("plugin-test-clean")
//	    // s.APIURL(), s.NATSURL() ready
//	}
package integrationtesting

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slidebolt/sdk-types"
)

const internalHealthRoute = "/_internal/health"

// Suite manages a running Slidebolt stack for one test.
type Suite struct {
	t       *testing.T
	dir     string
	logDir  string
	logFile *os.File
	cmd     *exec.Cmd
	apiURL  string
	natsURL string
	env     []string
}

// APIURL returns the base URL of the running gateway (e.g. http://127.0.0.1:PORT).
func (s *Suite) APIURL() string { return s.apiURL }

// NATSURL returns the NATS URL used by this stack instance.
func (s *Suite) NATSURL() string { return s.natsURL }

// DataDir returns the root data directory used by plugins in this stack instance.
// Plugin data lives at {DataDir()}/{plugin-id}/...
func (s *Suite) DataDir() string { return filepath.Join(s.dir, ".build", "data") }

// --- binary build cache ---

var (
	infraOnce sync.Once
	infraGW   string
	infraLC   string
	infraErr  error

	pluginMu   sync.Mutex
	pluginBins = map[string]string{} // module path → binary path
)

func buildInfra(t *testing.T) {
	t.Helper()
	infraOnce.Do(func() {
		// Makefile can pre-build these and export the paths to skip compilation.
		if gw := firstEnv("SDK_INTEGRATION_TESTING_GATEWAY_BIN", "PLUGINHARNESS_GATEWAY_BIN"); gw != "" {
			if lc := firstEnv("SDK_INTEGRATION_TESTING_LAUNCHER_BIN", "PLUGINHARNESS_LAUNCHER_BIN"); lc != "" {
				infraGW, infraLC = gw, lc
				return
			}
		}
		dir, err := os.MkdirTemp("", "sdk-integration-testing-infra-*")
		if err != nil {
			infraErr = fmt.Errorf("create infra dir: %w", err)
			return
		}
		env := goworkEnv()
		gw := filepath.Join(dir, "gateway")
		if out, err := gobuild(env, gw, "github.com/slidebolt/gateway"); err != nil {
			infraErr = fmt.Errorf("build gateway: %w\n%s", err, out)
			return
		}
		lc := filepath.Join(dir, "launcher")
		if out, err := gobuild(env, lc, "github.com/slidebolt/launcher"); err != nil {
			infraErr = fmt.Errorf("build launcher: %w\n%s", err, out)
			return
		}
		infraGW, infraLC = gw, lc
	})
}

func buildPlugin(t *testing.T, module, srcDir string) (string, error) {
	t.Helper()
	pluginMu.Lock()
	if bin, ok := pluginBins[module]; ok {
		pluginMu.Unlock()
		return bin, nil
	}
	pluginMu.Unlock()

	tmp, err := os.MkdirTemp("", "sdk-integration-testing-plugin-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(tmp, filepath.Base(module))
	out, err := gobuilddir(goworkEnv(), bin, srcDir)
	if err != nil {
		return "", fmt.Errorf("build %s: %w\n%s", module, err, out)
	}
	pluginMu.Lock()
	pluginBins[module] = bin
	pluginMu.Unlock()
	return bin, nil
}

// --- New / Stop ---

// New builds all required binaries (once per test binary run), boots a full
// Slidebolt stack, and blocks until the gateway writes runtime.json.
// The stack is stopped automatically via t.Cleanup.
//
// pluginModule is the Go module path, e.g. "github.com/slidebolt/plugin-test-clean".
// pluginDir is the directory containing the plugin's go.mod; pass "." when calling
// from inside the plugin's own test files.
func New(t *testing.T, pluginModule, pluginDir string) *Suite {
	t.Helper()

	buildInfra(t)
	if infraErr != nil {
		t.Fatalf("integrationtesting: %v", infraErr)
	}

	pluginBin, err := buildPlugin(t, pluginModule, pluginDir)
	if err != nil {
		t.Fatalf("integrationtesting: %v", err)
	}

	dir, err := os.MkdirTemp("", "sdk-integration-testing-run-*")
	if err != nil {
		t.Fatalf("integrationtesting: create run dir: %v", err)
	}

	binDir := filepath.Join(dir, ".build", "bin")
	for _, sub := range []string{
		binDir,
		filepath.Join(dir, ".build", "pids"),
		filepath.Join(dir, ".build", "logs"),
		filepath.Join(dir, ".build", "data"),
	} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("integrationtesting: mkdir: %v", err)
		}
	}

	copyBin(t, infraGW, filepath.Join(binDir, "gateway"))
	copyBin(t, pluginBin, filepath.Join(binDir, filepath.Base(pluginModule)))
	writeManifest(t, dir, "gateway")

	env := filterEnv(os.Environ(), func(k string) bool {
		return !strings.HasPrefix(k, "LAUNCHER_")
	})
	env = append(env,
		"LAUNCHER_PREBUILT=true",
		"LAUNCHER_API_PORT=0",
		"LAUNCHER_NATS_PORT=0",
		"LAUNCHER_BUILD_DIR="+filepath.Join(dir, ".build"),
	)

	logDir := filepath.Join(dir, ".build", "logs")
	s := &Suite{t: t, dir: dir, logDir: logDir, env: env}
	s.start()

	// Block until the gateway writes runtime.json with the actual ports.
	s.waitForRuntime()

	t.Cleanup(s.Stop)
	return s
}

func (s *Suite) start() {
	logFile, err := os.Create(filepath.Join(s.dir, "launcher.log"))
	if err != nil {
		s.t.Fatalf("integrationtesting: create launcher log: %v", err)
	}
	cmd := exec.Command(infraLC, "up")
	cmd.Dir = s.dir
	cmd.Env = s.env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		s.t.Fatalf("integrationtesting: start launcher: %v", err)
	}
	s.logFile = logFile
	s.cmd = cmd
}

func (s *Suite) waitForRuntime() {
	runtimePath := filepath.Join(s.dir, ".build", "runtime.json")
	if !waitForFile(runtimePath, 30*time.Second) {
		dumpLog(s.t, filepath.Join(s.dir, "launcher.log"))
		dumpLog(s.t, filepath.Join(s.logDir, "plugin-test-flaky.log"))
		s.stop(false)
		s.t.Fatalf("integrationtesting: runtime.json not written within 30s (stack failed to start)")
	}
	rt, err := readRuntime(runtimePath)
	if err != nil {
		s.stop(false)
		s.t.Fatalf("integrationtesting: read runtime.json: %v", err)
	}
	s.apiURL = rt.APIBaseURL
	s.natsURL = rt.NATSURL
}

func (s *Suite) stop(removeDir bool) {
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	if s.cmd == nil || s.cmd.Process == nil {
		if removeDir {
			os.RemoveAll(s.dir)
		}
		return
	}
	s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
	s.apiURL = ""
	s.natsURL = ""
	if s.t != nil && s.t.Failed() {
		s.t.Logf("integrationtesting: preserving failed run artifacts at %s", s.dir)
		return
	}
	if removeDir {
		os.RemoveAll(s.dir)
	}
}

// Restart stops the current stack without removing its run directory, then
// starts it again so persisted plugin data under .build/data survives.
func (s *Suite) Restart() {
	s.t.Helper()
	s.stop(false)
	_ = os.Remove(filepath.Join(s.dir, ".build", "runtime.json"))
	s.start()
	s.waitForRuntime()
}

// Stop sends SIGTERM to the launcher and waits up to 10s for clean exit.
func (s *Suite) Stop() {
	s.stop(true)
}

// --- Plugin assertions ---

// RequirePlugin waits for pluginID to register with the gateway and become healthy.
// Fails (not skips) the test if it doesn't happen in time.
func (s *Suite) RequirePlugin(id string) {
	s.t.Helper()
	if !s.WaitFor(30*time.Second, func() bool { return s.isRegistered(id) }) {
		dumpLog(s.t, filepath.Join(s.dir, "launcher.log"))
		dumpLog(s.t, filepath.Join(s.logDir, id+".log"))
		s.t.Fatalf("integrationtesting: plugin %q did not register within 30s", id)
	}
	if !s.WaitFor(15*time.Second, func() bool { return s.isHealthy(id) }) {
		dumpLog(s.t, filepath.Join(s.dir, "launcher.log"))
		dumpLog(s.t, filepath.Join(s.logDir, id+".log"))
		s.t.Fatalf("integrationtesting: plugin %q not healthy within 15s of registration", id)
	}
}

// SkipUnlessPlugin is like RequirePlugin but skips instead of failing.
// Use for tests that depend on hardware or optional configuration.
func (s *Suite) SkipUnlessPlugin(id string) {
	s.t.Helper()
	if !s.WaitFor(20*time.Second, func() bool { return s.isRegistered(id) }) {
		s.t.Skipf("integrationtesting: plugin %q not registered; skipping", id)
	}
}

// Plugins returns the registered plugin map from the gateway.
func (s *Suite) Plugins() (map[string]types.Registration, error) {
	resp, err := http.Get(s.apiURL + "/api/plugins")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/plugins: status %d", resp.StatusCode)
	}
	var out map[string]types.Registration
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// Get performs a GET against the gateway and returns the body bytes and status code.
func (s *Suite) Get(path string) ([]byte, int, error) {
	resp, err := http.Get(s.apiURL + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

// GetJSON performs a GET and JSON-decodes the response body into v.
func (s *Suite) GetJSON(path string, v any) error {
	body, status, err := s.Get(path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", path, status, body)
	}
	return json.Unmarshal(body, v)
}

// WaitFor polls cond at 100ms intervals until it returns true or timeout elapses.
func (s *Suite) WaitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// --- internal ---

func (s *Suite) isRegistered(id string) bool {
	plugins, err := s.Plugins()
	if err != nil {
		return false
	}
	_, ok := plugins[id]
	return ok
}

func (s *Suite) isHealthy(id string) bool {
	client := http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(s.apiURL + internalHealthRoute + "?id=" + id)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	status, _ := body["status"].(string)
	return status == "ok" || status == "perfect"
}

// --- build helpers ---

func gobuild(env []string, out, module string) ([]byte, error) {
	cmd := exec.Command("go", "build", "-o", out, module)
	cmd.Env = env
	return cmd.CombinedOutput()
}

func gobuilddir(env []string, out, dir string) ([]byte, error) {
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

func goworkEnv() []string {
	env := os.Environ()
	if gw := findGoWork(); gw != "" {
		env = append(env, "GOWORK="+gw)
	}
	return env
}

func findGoWork() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return filepath.Join(dir, "go.work")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// --- file helpers ---

type runtimeDescriptor struct {
	APIBaseURL string `json:"api_base_url"`
	NATSURL    string `json:"nats_url"`
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func readRuntime(path string) (runtimeDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runtimeDescriptor{}, err
	}
	var rt runtimeDescriptor
	return rt, json.Unmarshal(data, &rt)
}

func copyBin(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("integrationtesting: read binary %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("integrationtesting: write binary %s: %v", dst, err)
	}
}

func writeManifest(t *testing.T, dir string, binaries ...string) {
	t.Helper()
	type entry struct {
		ID     string `json:"id"`
		Binary string `json:"binary"`
	}
	entries := make([]entry, len(binaries))
	for i, b := range binaries {
		entries[i] = entry{ID: b, Binary: b}
	}
	data, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("integrationtesting: write manifest: %v", err)
	}
}

func filterEnv(env []string, keep func(string) bool) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if i := strings.Index(kv, "="); i > 0 {
			key = kv[:i]
		}
		if keep(key) {
			out = append(out, kv)
		}
	}
	return out
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func dumpLog(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	t.Logf("=== %s ===\n%s", filepath.Base(path), data)
}
