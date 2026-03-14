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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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

	globalSuiteMu sync.Mutex
	globalSuite   *Suite
)

// buildInfraE builds gateway and launcher binaries once per process.
// Safe to call from both test and non-test contexts.
func buildInfraE() error {
	infraOnce.Do(func() {
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
	return infraErr
}

func buildInfra(t *testing.T) {
	t.Helper()
	if err := buildInfraE(); err != nil {
		t.Fatalf("integrationtesting: %v", err)
	}
}

// buildPluginE builds a plugin binary once per process, returning the path or an error.
func buildPluginE(module, srcDir string) (string, error) {
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

func buildPlugin(t *testing.T, module, srcDir string) (string, error) {
	t.Helper()
	return buildPluginE(module, srcDir)
}

// --- New / Stop ---

// withT returns a lightweight view of this suite bound to a different *testing.T.
// The view does NOT own the stack — Stop() is a no-op and no t.Cleanup is registered.
// Used by New() when RunAll has already started a shared stack.
func (s *Suite) withT(t *testing.T) *Suite {
	return &Suite{
		t:       t,
		dir:     s.dir,
		logDir:  s.logDir,
		apiURL:  s.apiURL,
		natsURL: s.natsURL,
		env:     s.env,
		// cmd intentionally nil: Stop() on a view is a no-op.
	}
}

// New builds all required binaries (once per test binary run), boots a full
// Slidebolt stack, and blocks until the gateway writes runtime.json.
// The stack is stopped automatically via t.Cleanup.
//
// pluginModule is the Go module path, e.g. "github.com/slidebolt/plugin-test-clean".
// pluginDir is the directory containing the plugin's go.mod; pass "." when calling
// from inside the plugin's own test files.
//
// When called inside a RunAll-based TestMain (test_multi / test_multi_local), New
// returns a view of the shared stack instead of starting a fresh one. This means
// every existing integration test re-runs automatically against the full multi-plugin
// stack with zero changes to the test files themselves.
func New(t *testing.T, pluginModule, pluginDir string) *Suite {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration stack bootstrap in short mode")
	}

	// If RunAll already started a shared stack (test_multi context), reuse it.
	// We don't start a new stack, and we don't register a Stop cleanup.
	globalSuiteMu.Lock()
	gs := globalSuite
	globalSuiteMu.Unlock()
	if gs != nil {
		return gs.withT(t)
	}

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
		return !strings.HasPrefix(k, "LAUNCHER_") || k == "LAUNCHER_PLUGIN_CONFIG_ROOT"
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

// PluginSpec identifies a plugin binary to build and include in a multi-plugin stack.
type PluginSpec struct {
	Module   string            // Go module path, e.g. "github.com/slidebolt/plugin-test-clean"
	Dir      string            // Directory containing go.mod; pass "." from inside the plugin directory
	EnvFiles []string          // Optional env files resolved relative to Dir unless absolute
	Env      map[string]string // Optional explicit env overrides for this plugin's stack context
}

// NewMulti is like New but boots multiple plugins in the same stack.
// primary is the main plugin under test; extras are additional plugins to co-start.
// The launcher auto-discovers all binaries whose names start with "plugin-".
func NewMulti(t *testing.T, primary PluginSpec, extras ...PluginSpec) *Suite {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration stack bootstrap in short mode")
	}

	buildInfra(t)
	if infraErr != nil {
		t.Fatalf("integrationtesting: %v", infraErr)
	}

	allSpecs := append([]PluginSpec{primary}, extras...)
	builtBins := make([]string, 0, len(allSpecs))
	for _, spec := range allSpecs {
		bin, err := buildPlugin(t, spec.Module, spec.Dir)
		if err != nil {
			t.Fatalf("integrationtesting: %v", err)
		}
		builtBins = append(builtBins, bin)
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
	for i, spec := range allSpecs {
		copyBin(t, builtBins[i], filepath.Join(binDir, filepath.Base(spec.Module)))
	}
	writeManifest(t, dir, "gateway")

	env := filterEnv(os.Environ(), func(k string) bool {
		return !strings.HasPrefix(k, "LAUNCHER_") || k == "LAUNCHER_PLUGIN_CONFIG_ROOT"
	})
	env, err = mergePluginSpecEnv(env, allSpecs)
	if err != nil {
		t.Fatalf("integrationtesting: %v", err)
	}
	env = append(env,
		"LAUNCHER_PREBUILT=true",
		"LAUNCHER_API_PORT=0",
		"LAUNCHER_NATS_PORT=0",
		"LAUNCHER_BUILD_DIR="+filepath.Join(dir, ".build"),
	)

	logDir := filepath.Join(dir, ".build", "logs")
	s := &Suite{t: t, dir: dir, logDir: logDir, env: env}
	s.start()
	s.waitForRuntime()

	t.Cleanup(s.Stop)
	return s
}


func (s *Suite) start() {
	logFile, err := os.Create(filepath.Join(s.dir, "launcher.log"))
	if err != nil {
		s.fatal("integrationtesting: create launcher log: %v", err)
	}
	cmd := exec.Command(infraLC, "up")
	cmd.Dir = s.dir
	cmd.Env = s.env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		s.fatal("integrationtesting: start launcher: %v", err)
	}
	s.logFile = logFile
	s.cmd = cmd
}

func (s *Suite) waitForRuntime() {
	runtimePath := filepath.Join(s.dir, ".build", "runtime.json")
	if !waitForFile(runtimePath, 30*time.Second) {
		s.dumpLogs()
		s.stop(false)
		s.fatal("integrationtesting: runtime.json not written within 30s (stack failed to start)")
	}
	rt, err := readRuntime(runtimePath)
	if err != nil {
		s.stop(false)
		s.fatal("integrationtesting: read runtime.json: %v", err)
	}
	s.apiURL = rt.APIBaseURL
	s.natsURL = rt.NATSURL
}

// fatal calls t.Fatalf when running under a test, or log.Fatalf otherwise (RunAll context).
func (s *Suite) fatal(format string, args ...any) {
	if s.t != nil {
		s.t.Helper()
		s.t.Fatalf(format, args...)
	} else {
		log.Fatalf(format, args...)
	}
}

func (s *Suite) dumpLogs() {
	if s.t != nil {
		dumpLog(s.t, filepath.Join(s.dir, "launcher.log"))
		dumpLog(s.t, filepath.Join(s.logDir, "plugin-test-flaky.log"))
	}
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

// --- RunAll / Suite — full-stack shared test environment ---

// RunAll builds all listed plugins, starts one shared stack, runs all tests via
// m.Run(), tears down, then calls os.Exit. Call from TestMain when using the
// test_multi build tag. After RunAll starts the stack, tests call Suite(t) to
// get the running environment.
func RunAll(m *testing.M, plugins []PluginSpec) {
	if err := buildInfraE(); err != nil {
		log.Fatalf("integrationtesting RunAll: %v", err)
	}

	dir, err := os.MkdirTemp("", "sdk-integration-testing-run-*")
	if err != nil {
		log.Fatalf("integrationtesting RunAll: create run dir: %v", err)
	}

	binDir := filepath.Join(dir, ".build", "bin")
	for _, sub := range []string{
		binDir,
		filepath.Join(dir, ".build", "pids"),
		filepath.Join(dir, ".build", "logs"),
		filepath.Join(dir, ".build", "data"),
	} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			log.Fatalf("integrationtesting RunAll: mkdir: %v", err)
		}
	}

	// Copy gateway binary.
	if err := copyBinE(infraGW, filepath.Join(binDir, "gateway")); err != nil {
		log.Fatalf("integrationtesting RunAll: %v", err)
	}

	// Build and copy every plugin.
	for _, spec := range plugins {
		bin, err := buildPluginE(spec.Module, spec.Dir)
		if err != nil {
			log.Fatalf("integrationtesting RunAll: %v", err)
		}
		if err := copyBinE(bin, filepath.Join(binDir, filepath.Base(spec.Module))); err != nil {
			log.Fatalf("integrationtesting RunAll: %v", err)
		}
	}

	// Write manifest listing only the gateway (plugins are auto-discovered by launcher).
	writeManifestFatal(dir, "gateway")

	env := filterEnv(os.Environ(), func(k string) bool {
		return !strings.HasPrefix(k, "LAUNCHER_") || k == "LAUNCHER_PLUGIN_CONFIG_ROOT"
	})
	env, err = mergePluginSpecEnv(env, plugins)
	if err != nil {
		log.Fatalf("integrationtesting RunAll: %v", err)
	}
	env = append(env,
		"LAUNCHER_PREBUILT=true",
		"LAUNCHER_API_PORT=0",
		"LAUNCHER_NATS_PORT=0",
		"LAUNCHER_BUILD_DIR="+filepath.Join(dir, ".build"),
	)

	logDir := filepath.Join(dir, ".build", "logs")
	s := &Suite{dir: dir, logDir: logDir, env: env} // no t — RunAll context
	s.start()
	s.waitForRuntime()

	globalSuiteMu.Lock()
	globalSuite = s
	globalSuiteMu.Unlock()

	code := m.Run()
	s.Stop()
	os.Exit(code)
}

// GetSuite returns the shared stack started by RunAll.
// Must be called after RunAll has been set up in TestMain.
// Panics with a clear message if RunAll was not called.
func GetSuite(t *testing.T) *Suite {
	t.Helper()
	globalSuiteMu.Lock()
	s := globalSuite
	globalSuiteMu.Unlock()
	if s == nil {
		panic("integrationtesting.Suite called without integrationtesting.RunAll in TestMain")
	}
	return s
}

// --- Plugin assertions ---

// RequirePlugin waits for pluginID to register with the gateway and become healthy.
// Fails (not skips) the test if it doesn't happen in time.
func (s *Suite) RequirePlugin(id string) {
	if s.t != nil {
		s.t.Helper()
	}
	if !s.WaitFor(30*time.Second, func() bool { return s.isRegistered(id) }) {
		if s.t != nil {
			dumpLog(s.t, filepath.Join(s.dir, "launcher.log"))
			dumpLog(s.t, filepath.Join(s.logDir, id+".log"))
		}
		s.fatal("integrationtesting: plugin %q did not register within 30s", id)
	}
	if !s.WaitFor(15*time.Second, func() bool { return s.isHealthy(id) }) {
		if s.t != nil {
			dumpLog(s.t, filepath.Join(s.dir, "launcher.log"))
			dumpLog(s.t, filepath.Join(s.logDir, id+".log"))
		}
		s.fatal("integrationtesting: plugin %q not healthy within 15s of registration", id)
	}
}

// SkipUnlessPlugin is like RequirePlugin but skips the test instead of failing
// when the plugin doesn't register. Use for plugins that require hardware or
// optional external services (e.g. MQTT broker, cameras).
func (s *Suite) SkipUnlessPlugin(t *testing.T, id string) {
	t.Helper()
	if !s.WaitFor(20*time.Second, func() bool { return s.isRegistered(id) }) {
		t.Skipf("integrationtesting: plugin %q not registered; skipping (hardware not available)", id)
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

// PostJSON performs a POST with a JSON body and optionally JSON-decodes the response into v.
// Pass nil for v to ignore the response body.
func (s *Suite) PostJSON(path string, body any, v any) error {
	return s.doJSON(http.MethodPost, path, body, v)
}

// PatchJSON performs a PATCH with a JSON body and optionally JSON-decodes the response into v.
// Pass nil for v to ignore the response body.
func (s *Suite) PatchJSON(path string, body any, v any) error {
	return s.doJSON(http.MethodPatch, path, body, v)
}

func (s *Suite) doJSON(method, path string, reqBody any, v any) error {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("%s %s: marshal: %w", method, path, err)
	}
	req, err := http.NewRequest(method, s.apiURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, respBody)
	}
	if v != nil {
		return json.Unmarshal(respBody, v)
	}
	return nil
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
	if err := copyBinE(src, dst); err != nil {
		t.Fatalf("integrationtesting: %v", err)
	}
}

func copyBinE(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read binary %s: %w", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("write binary %s: %w", dst, err)
	}
	return nil
}

func writeManifest(t *testing.T, dir string, binaries ...string) {
	t.Helper()
	if err := writeManifestE(dir, binaries...); err != nil {
		t.Fatalf("integrationtesting: %v", err)
	}
}

func writeManifestFatal(dir string, binaries ...string) {
	if err := writeManifestE(dir, binaries...); err != nil {
		log.Fatalf("integrationtesting: %v", err)
	}
}

func writeManifestE(dir string, binaries ...string) error {
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
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
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

func mergePluginSpecEnv(base []string, specs []PluginSpec) ([]string, error) {
	merged := envSliceToMap(base)
	for _, spec := range specs {
		for _, file := range spec.EnvFiles {
			path := file
			if !filepath.IsAbs(path) {
				path = filepath.Join(spec.Dir, file)
			}
			values, err := parseEnvFile(path)
			if err != nil {
				return nil, fmt.Errorf("load env for %s from %s: %w", spec.Module, path, err)
			}
			for k, v := range values {
				merged[k] = v
			}
		}
		for k, v := range spec.Env {
			merged[k] = v
		}
	}
	return envMapToSlice(merged), nil
}

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				unquoted, err := strconv.Unquote(`"` + strings.ReplaceAll(value[1:len(value)-1], `"`, `\"`) + `"`)
				if err == nil {
					value = unquoted
				} else {
					value = value[1 : len(value)-1]
				}
			}
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

func envMapToSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func dumpLog(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	t.Logf("=== %s ===\n%s", filepath.Base(path), data)
}
