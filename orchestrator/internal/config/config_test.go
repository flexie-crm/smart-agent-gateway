package config

import (
	"reflect"
	"testing"
)

func TestParseOrigins(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty means same-origin only", raw: "", want: nil},
		{name: "blank means same-origin only", raw: "   ", want: nil},
		{
			name: "a list, with the spaces people put after commas",
			raw:  "http://localhost:5174, http://127.0.0.1:5174",
			want: []string{"http://localhost:5174", "http://127.0.0.1:5174"},
		},
		{
			name: "a trailing comma is not an entry",
			raw:  "https://console.example.com,",
			want: []string{"https://console.example.com"},
		},
		{
			// A browser sends "http://localhost:5174", so an entry without a
			// scheme could never match anything. Refusing it at boot beats
			// silently allowing nothing.
			name:    "an entry without a scheme is a boot error",
			raw:     "localhost:5174",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOrigins(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseOrigins(%q) accepted a non-origin", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOrigins(%q): %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseOrigins(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
