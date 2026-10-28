package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

func TestIsSafeEntryName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.txt", true},
		{"my-dir", true},
		{".hidden", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../etc/passwd", false},
		{"../../home/user/.ssh/authorized_keys", false},
		{"a/b", false},
		{"a\\b", false},
		{"..evil", true}, // starts with ".." but isn't the literal ".." component
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSafeEntryName(tc.name); got != tc.want {
				t.Errorf("isSafeEntryName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestErrorMessagesAreNotDuplicated(t *testing.T) {
	// Quiet the "debug:" lines wrapError prints.
	stderr := os.Stderr
	os.Stderr, _ = os.Open(os.DevNull)
	defer func() { os.Stderr = stderr }()

	tlsRaw := errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")
	wrapped := wrapError("upload", tlsRaw)

	if got, want := wrapped.Error(), "upload: TLS error: "+tlsRaw.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// Mirror reports "<op> <file>: " + translateError(err): the op must not
	// repeat, and the TLS prefix must not be applied twice.
	if got := fmt.Sprintf("upload %s: %s", "a.txt", translateError(wrapped)); got != "upload a.txt: TLS error: "+tlsRaw.Error() {
		t.Errorf("mirror message = %q", got)
	}
	// Re-wrapping keeps the innermost (most specific) operation.
	if got := wrapError("mirror", wrapped).Error(); got != wrapped.Error() {
		t.Errorf("re-wrapped = %q, want unchanged %q", got, wrapped.Error())
	}
	// The raw cause stays reachable.
	notExist := wrapError("stat", fmt.Errorf("lstat: %w", fs.ErrNotExist))
	if !errors.Is(notExist, fs.ErrNotExist) {
		t.Error("errors.Is through cleanError broken")
	}
}
