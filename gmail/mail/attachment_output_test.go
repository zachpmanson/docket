package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootedAttachmentDownloads(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		name := "relative"
		if absolute {
			name = "absolute"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "downloads"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("downloads", "image.png")
			if absolute {
				path = filepath.Join(root, path)
			}
			got, _, err := fetchFixture(t, imagePayload(), FetchOptions{
				PartID: "1.2", OutPath: path, OutputRoot: root,
			}, func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
			if err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.Join(root, "downloads", "image.png")
			if got.Path != wantPath {
				t.Fatalf("path = %q, want %q", got.Path, wantPath)
			}
			assertAttachmentFile(t, wantPath, pngBytes)
			info, err := os.Stat(wantPath)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("expected private file: %v %v", info, err)
			}
		})
	}
}

func TestRootedAttachmentRejectsEscapesBeforeGmail(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "attachments")
	outside := filepath.Join(base, "protected")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(outside, "config.toml")
	store := filepath.Join(base, "immutable-policy.toml")
	if err := os.WriteFile(store, []byte("policy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../protected", filepath.Join(root, "relative-escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "token.json"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"../protected/config.toml", config,
		filepath.Join(outside, "token.json"), filepath.Join(outside, "new-token.json"),
		filepath.Join(outside, "accounts.json"), filepath.Join(outside, ".profile"),
		"escape/token.json", "escape/new-token.json", "escape/../image.png", "relative-escape/token.json",
		"missing/image.png", ".", "..", root, root + string(os.PathSeparator),
	} {
		t.Run(path, func(t *testing.T) {
			// A nil service would panic if metadata or download were attempted.
			_, err := FetchAttachment(context.Background(), nil, "m1", FetchOptions{
				PartID: "1.2", OutPath: path, OutputRoot: root,
			})
			if !errors.Is(err, ErrOutputWrite) {
				t.Fatalf("err = %v, want ErrOutputWrite before Gmail", err)
			}
		})
	}
	if target, err := os.Readlink(config); err != nil || target != store {
		t.Fatalf("config symlink changed: %q %v", target, err)
	}
	assertAttachmentFile(t, store, []byte("policy"))
	assertAttachmentFile(t, filepath.Join(outside, "token.json"), []byte("token"))
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 2 {
		t.Fatalf("protected directory changed: %v %v", entries, err)
	}
}

func TestRootedAttachmentFinalSymlinkIsReplacedNotFollowed(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(target, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "image.png")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	_, _, err := fetchFixture(t, imagePayload(), FetchOptions{
		PartID: "1.2", OutPath: "image.png", OutputRoot: root,
	}, func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentFile(t, target, []byte("protected"))
	assertAttachmentFile(t, path, pngBytes)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("final symlink not replaced: %v %v", info, err)
	}
}

func TestRootedAttachmentPinsParentAcrossSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(root, "downloads")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := FetchOptions{PartID: "1.2", OutputRoot: root, OutPath: "downloads/image.png"}
	closer, err := opts.PrepareOutput()
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	pinned := filepath.Join(root, "pinned")
	if err := os.Rename(parent, pinned); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	_, _, err = fetchFixture(t, imagePayload(), opts,
		func(f *fakeGmail) { f.attachments["att-img"] = pngBytes })
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentFile(t, filepath.Join(pinned, "image.png"), pngBytes)
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("parent swap escaped: %v %v", entries, err)
	}
	entries, err = os.ReadDir(pinned)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file leaked: %v %v", entries, err)
	}
}

func TestRootedAttachmentRejectsRelativeOperatorRoot(t *testing.T) {
	_, err := FetchAttachment(context.Background(), nil, "m1", FetchOptions{
		PartID: "1.2", OutputRoot: "attachments", OutPath: "image.png",
	})
	if !errors.Is(err, ErrOutputWrite) {
		t.Fatalf("err = %v, want ErrOutputWrite", err)
	}
}

func assertAttachmentFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("%q = %q, %v; want %q", path, got, err, want)
	}
}
