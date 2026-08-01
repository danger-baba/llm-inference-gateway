package providers

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter_Seconds(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"empty", "", 0},
		{"typical", "30", 30 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"garbage", "not-a-value", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseRetryAfter(tt.header); got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	header := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	got := ParseRetryAfter(header)
	if got <= 0 || got > 2*time.Minute {
		t.Errorf("ParseRetryAfter(%q) = %v, want a positive duration <= 2m", header, got)
	}
}

func TestParseRetryAfter_PastHTTPDate(t *testing.T) {
	header := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := ParseRetryAfter(header); got != 0 {
		t.Errorf("ParseRetryAfter(%q) = %v, want 0 for a past date", header, got)
	}
}
