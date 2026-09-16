package datasource

import (
	"fmt"
	"strings"
	"time"
)

// What a value becomes on its way to a model, and why it is not simply itself.
//
// A driver hands back what the wire gave it, and two of those are not safe to
// pass on as they are.
//
// BINARY IS NOT BROUGHT BACK AT ALL. Every driver here returns []byte for text
// as well as for binary, so turning []byte into a string is right for a VARCHAR
// and wrong for a BLOB. Measured before this existed: a PNG came back as
// "\x89PNG\r\n\x1a\n...", a JPEG as "\xff\xd8\xff\xe0\x00\x10JFIF", and a
// SQL Server GUID as sixteen unprintable bytes.
//
// Hex was the first answer and it is the wrong one. A blob is megabytes, and a
// thousand rows of hex is a model's whole context spent on something it cannot
// read anyway, on its way to a screen that has no use for it either. Nothing
// downstream wants these bytes in any form.
//
// So the value does not come back. What comes back is its size, which is enough
// to know the column holds something, and the result carries a line telling the
// model this tool does not return binary so that it stops asking. A GUID is the
// one exception and is not really binary: it is an identifier with a text form,
// and it is written in it.
//
// A DATE IS NOT A MOMENT. A DATE column becomes a time.Time, and a time.Time
// becomes 2027-01-15T00:00:00Z, which is a date with a midnight bolted onto it
// and a timezone it never had. A TIME column is worse: SQL Server hands back
// 0001-01-01 13:45:02, a real time under a year that does not exist. The column
// says which of the three it is, so it is written as what it is. Nothing is
// converted, reformatted, or moved between zones: a datetime is still handed
// over whole.

// Cell renders one value from a row, given the column type the database
// declared for it. An unknown or empty type name falls back to the old
// behaviour, so a driver that cannot say leaves its values exactly as they were.
func Cell(value any, columnType string) any {
	switch v := value.(type) {
	case []byte:
		if isBinaryType(columnType) {
			return binaryText(v, columnType)
		}
		return string(v)
	case time.Time:
		switch {
		case isDateType(columnType):
			return v.Format("2006-01-02")
		case isTimeType(columnType):
			return v.Format("15:04:05.999999999")
		}
		return v
	}
	return value
}

// binaryText says what is there without handing it over.
func binaryText(raw []byte, columnType string) string {
	if strings.EqualFold(columnType, "UNIQUEIDENTIFIER") && len(raw) == 16 {
		return guidText(raw)
	}
	return fmt.Sprintf("[binary, %d bytes, not returned]", len(raw))
}

// IsBinaryColumn reports whether a column holds bytes this tool will not return,
// so a caller can say so once rather than leave it to be inferred from every
// cell.
func IsBinaryColumn(columnType string) bool {
	return isBinaryType(columnType) && !strings.EqualFold(columnType, "UNIQUEIDENTIFIER")
}

// guidText writes SQL Server's sixteen bytes the way SQL Server writes them.
// The first three groups are little-endian on the wire and big-endian in the
// text, which is why this cannot be a straight hex dump.
func guidText(raw []byte) string {
	return fmt.Sprintf("%02X%02X%02X%02X-%02X%02X-%02X%02X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		raw[3], raw[2], raw[1], raw[0],
		raw[5], raw[4],
		raw[7], raw[6],
		raw[8], raw[9],
		raw[10], raw[11], raw[12], raw[13], raw[14], raw[15])
}

// The type names each engine uses. Matched loosely, because a driver may report
// VARBINARY(64) or just VARBINARY, and because being wrong here costs a value
// written as hex rather than a value written as bytes.
func isBinaryType(name string) bool {
	n := strings.ToUpper(name)
	for _, binary := range []string{
		"BINARY", "BLOB", "BYTEA", "IMAGE", "GEOMETRY", "UNIQUEIDENTIFIER", "RAW", "BIT",
	} {
		if strings.Contains(n, binary) {
			return true
		}
	}
	return false
}

func isDateType(name string) bool {
	n := strings.ToUpper(name)
	// DATE and nothing else: DATETIME, DATETIME2, TIMESTAMP and SMALLDATETIME
	// are moments and keep their time.
	return n == "DATE"
}

func isTimeType(name string) bool {
	n := strings.ToUpper(name)
	return n == "TIME" || n == "TIMETZ" || strings.HasPrefix(n, "TIME(")
}
