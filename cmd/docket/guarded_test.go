package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/mail"
	"github.com/zachpmanson/docket/internal/out"
)

func TestGuardedAttachmentSave(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0, 255, 128, 10}
	result, err := json.Marshal(out.Envelope{OK: true, Data: mail.FetchedAttachment{
		Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Content: &data,
	}})
	if err != nil {
		t.Fatal(err)
	}
	output, code := captureStdout(t, func() int {
		return saveGuardedAttachment(dir, "file.bin", func() ([]byte, error) { return result, nil })
	})
	path := filepath.Join(dir, "file.bin")
	if code != out.ExitOK || strings.Contains(output, "content_base64") || !strings.Contains(output, path) {
		t.Fatalf("code=%d output=%s", code, output)
	}
	if read, err := os.ReadFile(path); err != nil || string(read) != string(data) {
		t.Fatalf("saved bytes = %v, err = %v", read, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file mode: %v %v", info, err)
	}
	_, code = captureStdout(t, func() int {
		return saveGuardedAttachment(dir, "../escape", func() ([]byte, error) {
			t.Fatal("invalid destination reached protected fetch")
			return nil, nil
		})
	})
	if code != out.ExitError {
		t.Fatalf("invalid destination code = %d", code)
	}
}
