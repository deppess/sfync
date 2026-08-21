package transfer

import "testing"

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
