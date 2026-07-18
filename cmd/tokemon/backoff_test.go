package main

import (
	"testing"
	"time"
)

func TestNextAgentFailureIntervalHasBoundedBackoff(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		current    time.Duration
		want       time.Duration
	}{
		{name: "small interval starts at minimum", configured: 2 * time.Second, current: 2 * time.Second, want: 5 * time.Second},
		{name: "doubles", configured: 2 * time.Second, current: 5 * time.Second, want: 10 * time.Second},
		{name: "caps", configured: time.Second, current: 4 * time.Minute, want: 5 * time.Minute},
		{name: "preserves long configured interval", configured: 10 * time.Minute, current: 10 * time.Minute, want: 10 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextAgentFailureInterval(test.configured, test.current); got != test.want {
				t.Fatalf("nextAgentFailureInterval(%s, %s) = %s, want %s", test.configured, test.current, got, test.want)
			}
		})
	}
}
