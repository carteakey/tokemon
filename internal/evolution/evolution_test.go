package evolution

import "testing"

func TestStageThresholds(t *testing.T) {
	tests := []struct {
		tokens int64
		stage  int
	}{
		{0, 0}, {9, 0}, {10, 1}, {99, 1}, {100, 2}, {9999, 3},
		{10_000, 4}, {1_000_000_000, 9}, {999_999_999_999, 11}, {1_000_000_000_000, 12},
	}
	for _, test := range tests {
		if got := Stage(test.tokens); got != test.stage {
			t.Errorf("Stage(%d) = %d, want %d", test.tokens, got, test.stage)
		}
	}
}

func TestSnapshotProgress(t *testing.T) {
	snapshot := SnapshotFor(1_482_938_221)
	if snapshot.Stage != 9 || snapshot.Form != "token-titan" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.NextThreshold == nil || *snapshot.NextThreshold != 10_000_000_000 {
		t.Fatalf("unexpected next threshold: %+v", snapshot.NextThreshold)
	}
	if snapshot.TokensRemaining == nil || *snapshot.TokensRemaining != 8_517_061_779 {
		t.Fatalf("unexpected remaining tokens: %+v", snapshot.TokensRemaining)
	}
	if snapshot.Progress < 0.05365 || snapshot.Progress > 0.05367 {
		t.Fatalf("unexpected progress: %f", snapshot.Progress)
	}
}

func TestSingularityHasNoNextThreshold(t *testing.T) {
	snapshot := SnapshotFor(1_000_000_000_000)
	if snapshot.Stage != 12 || snapshot.NextThreshold != nil || snapshot.TokensRemaining != nil || snapshot.Progress != 1 {
		t.Fatalf("unexpected singularity snapshot: %+v", snapshot)
	}
}
