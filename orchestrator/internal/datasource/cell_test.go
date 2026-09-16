package datasource

import (
	"testing"
	"time"
)

// What a value becomes on its way to a model.
//
// Every case here was measured against a real server first, in the form it came
// back as before this existed.
func TestBinaryIsNotHandedOverAsText(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	for _, c := range []struct {
		columnType string
		want       string
	}{
		{"VARBINARY", "[binary, 8 bytes, not returned]"},
		{"BLOB", "[binary, 8 bytes, not returned]"},
		{"LONGBLOB", "[binary, 8 bytes, not returned]"},
		{"BYTEA", "[binary, 8 bytes, not returned]"},
		{"IMAGE", "[binary, 8 bytes, not returned]"},
		{"BINARY(8)", "[binary, 8 bytes, not returned]"},
	} {
		if got := Cell(png, c.columnType); got != c.want {
			t.Errorf("%s: got %q, want %q", c.columnType, got, c.want)
		}
	}
	// Text arrives as []byte too, on every driver, and must stay text.
	for _, columnType := range []string{"VARCHAR", "TEXT", "CHAR", "NVARCHAR", "JSON", ""} {
		if got := Cell([]byte("hello"), columnType); got != "hello" {
			t.Errorf("%s: text became %q", columnType, got)
		}
	}
}

// A blob is megabytes, and a model's context is not. What is left out says so.
func TestALongBinaryValueSaysHowLongItWas(t *testing.T) {
	got, _ := Cell(make([]byte, 5000), "BLOB").(string)
	if got != "[binary, 5000 bytes, not returned]" {
		t.Errorf("a 5000 byte value came back as %q", got)
	}
	if len(got) > 60 {
		t.Errorf("a 5000 byte value came back %d characters long", len(got))
	}
}

// SQL Server writes a GUID's first three groups little-endian on the wire and
// big-endian in text, so it cannot be a straight hex dump. Before this it came
// back as sixteen unprintable bytes.
func TestAGUIDIsReadable(t *testing.T) {
	raw := []byte{0x2A, 0x83, 0x77, 0x4D, 0x1C, 0x24, 0x82, 0x4E, 0xAB, 0xDC, 0x57, 0x8F, 0x67, 0x4E, 0x98, 0x00}
	want := "4D77832A-241C-4E82-ABDC-578F674E9800"
	if got := Cell(raw, "UNIQUEIDENTIFIER"); got != want {
		t.Errorf("got %v, want %s", got, want)
	}
}

// A DATE is not a moment and a TIME is not a day in year one.
func TestADateIsADate(t *testing.T) {
	moment := time.Date(2027, 1, 15, 13, 45, 2, 0, time.UTC)
	if got := Cell(moment, "DATE"); got != "2027-01-15" {
		t.Errorf("DATE became %v", got)
	}
	if got := Cell(time.Date(1, 1, 1, 13, 45, 2, 0, time.UTC), "TIME"); got != "13:45:02" {
		t.Errorf("TIME became %v", got)
	}
	// A moment keeps its time, its date and its zone: nothing is converted.
	for _, columnType := range []string{"DATETIME", "DATETIME2", "TIMESTAMP", "TIMESTAMPTZ", "SMALLDATETIME", ""} {
		if got := Cell(moment, columnType); got != moment {
			t.Errorf("%s was altered: %v", columnType, got)
		}
	}
}
