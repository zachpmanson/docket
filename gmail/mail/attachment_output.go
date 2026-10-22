package mail

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The parent Root is held across credential loading and the download. No output
// operation reopens an unrooted path, even if a parent symlink is later swapped.
// The dedicated output tree must not contain policy, credentials or other state.
type attachmentOutput struct {
	parent *os.Root
	name   string
	path   string
}

func prepareAttachmentOutput(rootPath, path string) (*attachmentOutput, error) {
	fail := func(err error) (*attachmentOutput, error) {
		return nil, fmt.Errorf("%w: destination %q: %v (the parent directory must already exist)", ErrOutputWrite, path, err)
	}
	if path == "" {
		return fail(errors.New("--out is required"))
	}
	resolved := path
	var parent *os.Root
	var err error
	if rootPath != "" {
		if !filepath.IsAbs(rootPath) {
			return fail(errors.New("DOCKET_ATTACHMENT_ROOT must be an absolute operator-controlled directory"))
		}
		rootPath = filepath.Clean(rootPath)
		if filepath.IsAbs(path) {
			path, err = filepath.Rel(rootPath, path)
			if err != nil {
				return fail(err)
			}
		}
		if !filepath.IsLocal(path) {
			return fail(errors.New("--out must stay beneath DOCKET_ATTACHMENT_ROOT"))
		}
		resolved = filepath.Join(rootPath, path)
	}
	// Split without cleaning: Root, not lexical normalization, must resolve
	// symlinks and any internal '..' components in a relative destination.
	dir, name := filepath.Split(path)
	if name == "" || name == "." || name == ".." {
		return fail(errors.New("--out must name a file, not the output root or a directory"))
	}
	if dir == "" {
		dir = "."
	}
	if rootPath != "" {
		root, openErr := os.OpenRoot(rootPath)
		if openErr != nil {
			return fail(openErr)
		}
		parent, err = root.OpenRoot(dir)
		root.Close()
	} else {
		// Trusted same-UID CLI use without a configured root preserves arbitrary
		// output paths. Credentials in that environment are not a sandbox boundary.
		parent, err = os.OpenRoot(dir)
	}
	if err != nil {
		return fail(err)
	}
	info, err := parent.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		parent.Close()
		return fail(err)
	}
	if err == nil && info.IsDir() {
		parent.Close()
		return fail(errors.New("--out names an existing directory"))
	}
	// A final symlink is deliberately replaced, never followed, by Rename.
	return &attachmentOutput{parent: parent, name: name, path: resolved}, nil
}

// Write via a private, exclusively-created temporary file in the pinned parent,
// then atomically replace only the final directory entry. All output I/O stays
// on this Root, including error cleanup; a symlink cannot redirect a write.
func (output *attachmentOutput) write(data []byte) error {
	fail := func(action string, err error) error {
		return fmt.Errorf("%w: %s %q: %v", ErrOutputWrite, action, output.path, err)
	}
	var tmp *os.File
	var name string
	for range 10 {
		name = ".docket-attachment-" + rand.Text()
		var err error
		tmp, err = output.parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fail("creating temporary file for", err)
		}
		break
	}
	if tmp == nil {
		return fail("creating temporary file for", os.ErrExist)
	}
	defer output.parent.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fail("setting permissions on", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fail("writing", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("closing", err)
	}
	if err := output.parent.Rename(name, output.name); err != nil {
		return fail("moving attachment into place at", err)
	}
	return nil
}
