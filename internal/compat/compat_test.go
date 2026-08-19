package compat

import "testing"

func TestParseMode(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		def     Mode
		want    Mode
		wantErr bool
	}{
		{name: "empty falls back to given default", in: "", def: ModeForce, want: ModeForce},
		{name: "empty with no default is auto", in: "", def: "", want: ModeAuto},
		{name: "raw", in: "raw", def: ModeAuto, want: ModeRaw},
		{name: "auto", in: "auto", def: ModeForce, want: ModeAuto},
		{name: "force", in: "force", def: ModeAuto, want: ModeForce},
		{name: "typo is rejected, not defaulted", in: "Auto", def: ModeAuto, wantErr: true},
		{name: "unknown", in: "passthrough", def: ModeAuto, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMode(tc.in, tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMode(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMode(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTranslating(t *testing.T) {
	for _, target := range []string{"anthropic", "gemini"} {
		if !Translating(target) {
			t.Errorf("Translating(%q) = false, want true", target)
		}
	}
	for _, target := range []string{"openai", "grok", "ollama", "arliai", ""} {
		if Translating(target) {
			t.Errorf("Translating(%q) = true, want false", target)
		}
	}
}
