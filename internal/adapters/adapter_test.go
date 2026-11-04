package adapters

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadRecordRejectsOversizedLines(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxRecordBytes+1)))
	if _, err := ReadRecord(reader); err == nil {
		t.Fatal("ReadRecord accepted an oversized provider record")
	}
}

func TestReadRecordPreservesUnterminatedFinalLine(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("record"))
	record, err := ReadRecord(reader)
	if string(record) != "record" {
		t.Fatalf("record = %q, want final line", record)
	}
	if err == nil {
		t.Fatal("ReadRecord returned nil error for an unterminated final line")
	}
}
