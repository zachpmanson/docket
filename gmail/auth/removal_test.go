package auth

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
)

func TestGuardedConcurrentRemoveRemoveReenroll(t *testing.T) {
	useStateHome(t)
	if err := InstallNewAccount("reusable", &oauth2.Token{AccessToken: "old-synthetic"}); err != nil {
		t.Fatal(err)
	}
	path, err := AccountTokenPath("reusable")
	if err != nil {
		t.Fatal(err)
	}
	cleanupPaused := make(chan struct{})
	resumeCleanup := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- removeAccountGuarded("reusable", func(target string) error {
			err := os.Remove(target)
			if target == filepath.Dir(path) && err == nil {
				// Token and directory are gone, but removal has not committed.
				close(cleanupPaused)
				<-resumeCleanup
			}
			return err
		})
	}()
	defer func() {
		// Also release the worker if a lock assertion fails below.
		select {
		case <-resumeCleanup:
		default:
			close(resumeCleanup)
		}
	}()
	select {
	case <-cleanupPaused:
	case err := <-firstDone:
		t.Fatalf("first removal did not reach cleanup barrier: %v", err)
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- RemoveAccountGuarded("reusable")
	}()
	<-secondStarted
	registryPath, err := RegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	// A nonblocking lock probe, rather than timing/sleeps, proves that neither
	// the second removal nor enrollment can pass registry resolution while
	// the first removal still has filesystem cleanup in flight.
	probe := flock.New(registryPath + ".lock")
	locked, err := probe.TryLock()
	if locked {
		_ = probe.Unlock()
	}
	if err != nil || locked {
		t.Fatalf("registry lock not held through cleanup: locked=%v err=%v", locked, err)
	}
	if err := InstallNewAccount("reusable", &oauth2.Token{AccessToken: "premature-synthetic"}); err == nil {
		t.Fatal("uncommitted removal exposed name for enrollment")
	}
	close(resumeCleanup)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err == nil {
		t.Fatal("second removal used stale account metadata")
	}
	if err := InstallNewAccount("reusable", &oauth2.Token{AccessToken: "new-synthetic"}); err != nil {
		t.Fatal(err)
	}
	tok, err := readToken(path)
	if err != nil || tok.AccessToken != "new-synthetic" {
		t.Fatalf("re-enrolled credentials lost: token=%v err=%v", tok, err)
	}
	if names, err := RegisteredAccounts(); err != nil || !reflect.DeepEqual(names, []string{"reusable"}) {
		t.Fatalf("re-enrolled registry lost: %v %v", names, err)
	}
}

func TestGuardedRemoveDefaultDoesNotReadOrMigrateCredentials(t *testing.T) {
	root := useStateHome(t)
	legacy := filepath.Join(root, "docket", "token.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	// Invalid JSON would fail legacy migration if AccountTokenPath were used.
	if err := os.WriteFile(legacy, []byte("invalid synthetic token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccountGuarded("default"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy token remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "docket", "accounts")); !os.IsNotExist(err) {
		t.Fatalf("removal created a migrated profile: %v", err)
	}
}

func TestGuardedRemoveRejectsSymlinkProfile(t *testing.T) {
	root := useStateHome(t)
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(outside, "token.json")
	if err := os.WriteFile(protected, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "docket", "accounts", "linked")
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, profile); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccountGuarded("linked"); err == nil {
		t.Fatal("accepted symlink profile")
	}
	data, err := os.ReadFile(protected)
	if err != nil || string(data) != "protected" {
		t.Fatal("deleted credentials through profile symlink")
	}
}
