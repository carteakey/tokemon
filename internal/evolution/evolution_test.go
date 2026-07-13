package evolution

import "testing"

func TestStageThresholds(t *testing.T) {
	tests := []struct {
		tokens int64
		stage  int
	}{
		{0, 0}, {9, 0}, {10, 1}, {99, 1}, {100, 2}, {9999, 3},
		{10_000, 4}, {999_999_999, 8}, {1_000_000_000, 9},
		{1_999_999_999, 9}, {2_000_000_000, 10}, {4_999_999_999, 10},
		{5_000_000_000, 11}, {9_999_999_999, 11}, {10_000_000_000, 12},
		{19_999_999_999, 12}, {20_000_000_000, 13}, {49_999_999_999, 13},
		{50_000_000_000, 14}, {99_999_999_999, 14}, {100_000_000_000, 15},
		{199_999_999_999, 15}, {200_000_000_000, 16}, {499_999_999_999, 16},
		{500_000_000_000, 17}, {999_999_999_999, 17}, {1_000_000_000_000, 18},
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
	if snapshot.NextThreshold == nil || *snapshot.NextThreshold != 2_000_000_000 {
		t.Fatalf("unexpected next threshold: %+v", snapshot.NextThreshold)
	}
	if snapshot.TokensRemaining == nil || *snapshot.TokensRemaining != 517_061_779 {
		t.Fatalf("unexpected remaining tokens: %+v", snapshot.TokensRemaining)
	}
	if snapshot.Progress < 0.48293 || snapshot.Progress > 0.48295 {
		t.Fatalf("unexpected progress: %f", snapshot.Progress)
	}
}

func TestSingularityHasNoNextThreshold(t *testing.T) {
	snapshot := SnapshotFor(1_000_000_000_000)
	if snapshot.Stage != 18 || snapshot.NextThreshold != nil || snapshot.TokensRemaining != nil || snapshot.Progress != 1 {
		t.Fatalf("unexpected singularity snapshot: %+v", snapshot)
	}
}
