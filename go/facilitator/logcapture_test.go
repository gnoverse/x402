package facilitator

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// captureLogs routes the default logger into a buffer for the duration of the
// test, so tests can assert on the records the package emits. It swaps the
// process-wide default logger, so tests using it must not run in parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// logRecord returns the first captured record carrying the given message.
func logRecord(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "captured logs are not a JSON stream:\n%s", buf.String())
		if record["msg"] == msg {
			return record
		}
	}
	t.Fatalf("no log record with msg %q; captured:\n%s", msg, buf.String())
	return nil
}
