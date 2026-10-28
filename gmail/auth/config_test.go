package auth

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zachpmanson/docket/gmail/config"
)

type configReaderFunc func([]byte) (int, error)

func (f configReaderFunc) Read(p []byte) (int, error) { return f(p) }

// Pause after one byte has been written, not merely before creation. This
// deterministically exposes early publication without sleeps or scheduler luck.
func pauseConfigWrite(t *testing.T, path string) (<-chan error, func()) {
	t.Helper()
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	source := bytes.NewReader(config.Default)
	first := true
	paused := false
	reader := configReaderFunc(func(p []byte) (int, error) {
		if first {
			first = false
			return source.Read(p[:1])
		}
		if !paused {
			paused = true
			close(started)
			<-resume
		}
		return source.Read(p)
	})
	done := make(chan error, 1)
	go func() {
		done <- ensureConfigFrom(path, reader)
		close(done)
	}()
	t.Cleanup(func() {
		release()
		<-done
	})
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("writer exited before pause: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not reach pause")
	}
	return done, release
}

func assertConfigFiles(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("config differs: got %d bytes, want %d", len(got), len(want))
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestEnsureConfigConcurrentPublication(t *testing.T) {
	for _, caller := range []string{"goroutine", "process", "operator"} {
		t.Run(caller, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("DOCKET_CLIENT_ID", "")
			t.Setenv("DOCKET_CLIENT_SECRET", "")
			path, err := ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			done, release := pauseConfigWrite(t, path)
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incomplete config is observable: %v", err)
			}

			want := config.Default
			switch caller {
			case "goroutine":
				if _, err := LoadConfig(); err != nil {
					t.Fatal(err)
				}
			case "process":
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(exe, "-test.run=^TestEnsureConfigProcessHelper$")
				cmd.Env = append(os.Environ(), "DOCKET_CONFIG_TEST_CHILD=1")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("concurrent process: %v\n%s", err, out)
				}
			case "operator":
				want = []byte("operator configuration\n")
				if err := os.WriteFile(path, want, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			assertConfigFiles(t, path, want)
		})
	}
}

func TestEnsureConfigProcessHelper(t *testing.T) {
	if os.Getenv("DOCKET_CONFIG_TEST_CHILD") != "1" {
		return
	}
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureConfigFirstUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docket", "config.toml")
	const callers = 32
	start := make(chan struct{})
	done := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			if err := ensureConfig(path); err != nil {
				done <- err
				return
			}
			got, err := os.ReadFile(path)
			if err == nil && !bytes.Equal(got, config.Default) {
				err = errors.New("reader observed incomplete defaults")
			}
			done <- err
		}()
	}
	close(start)
	for range callers {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	assertConfigFiles(t, path, config.Default)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestEnsureConfigPreservesExisting(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "dangling-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			target := filepath.Join(dir, "operator.toml")
			want := []byte("operator settings\n")
			if kind == "file" {
				target = path
			}
			if kind != "dangling-symlink" {
				if err := os.WriteFile(target, want, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "file" {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := ensureConfig(path); err != nil {
				t.Fatal(err)
			}
			if kind != "file" {
				got, err := os.Readlink(path)
				if err != nil || got != target {
					t.Fatalf("symlink changed: %q, %v", got, err)
				}
			}
			if kind == "dangling-symlink" {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("created dangling target: %v", err)
				}
			} else {
				got, err := os.ReadFile(target)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("operator config changed: %q, %v", got, err)
				}
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != 0o400 {
					t.Fatalf("operator config permissions changed: %v, %v", info, err)
				}
			}
		})
	}
}

func TestEnsureConfigWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeErr := errors.New("injected read failure")
	reader := io.MultiReader(bytes.NewReader(config.Default[:1]), configReaderFunc(func([]byte) (int, error) {
		return 0, writeErr
	}))
	if err := ensureConfigFrom(path, reader); !errors.Is(err, writeErr) {
		t.Fatalf("got %v, want injected error", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed initialization left files: %v, %v", entries, err)
	}
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	assertConfigFiles(t, path, config.Default)
}
