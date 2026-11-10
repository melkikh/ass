package main

import "testing"

func TestChatReference(t *testing.T) {
	for _, tc := range []struct {
		source, id, native, want string
	}{
		{"codex", testID, "", "codex://threads/" + testID},
		{"claude", testID, "", testID},
		{"cursor", testID, "", testID},
		{"opencode", testID, "ses_native", "ses_native"},
		{"opencode", testID, "", ""},
		{"opencode", testID, "ses_invalid\nnext", ""},
		{"codex", "invalid", "", ""},
		{"unknown", testID, "", ""},
	} {
		v := session{Source: tc.source, ID: tc.id, Native: tc.native}
		if got := chatReference(v); got != tc.want {
			t.Errorf("reference for %s, %q, %q = %q, want %q", tc.source, tc.id, tc.native, got, tc.want)
		}
	}
}
