package main

import (
	"strings"
	"testing"
)

func TestResolvePlayerContextVideoID(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		args    []string
		wantID  string // checked when wantErr == ""
		wantErr string // substring; "" means success
	}{
		{name: "positional", args: []string{"exampleVid1"}, wantID: "exampleVid1"},
		{name: "flag", flag: "aqz-KE-bpKQ", wantID: "aqz-KE-bpKQ"},
		{name: "both given", flag: "AAA", args: []string{"BBB"}, wantErr: "video ID once"},
		{name: "missing", wantErr: "provide a video ID"},
		{name: "URL positional", args: []string{"https://youtu.be/x"}, wantErr: "not a URL"},
		{name: "URL flag", flag: "https://youtu.be/x", wantErr: "not a URL"},
		{name: "bad shape", args: []string{"bad id!!"}, wantErr: "1 to 64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePlayerContextVideoID(tt.flag, tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.wantID {
					t.Errorf("id = %q, want %q", got, tt.wantID)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got id %q", tt.wantErr, got)
			}
			if _, ok := err.(*usageError); !ok {
				t.Errorf("error type = %T, want *usageError (exit 2)", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
