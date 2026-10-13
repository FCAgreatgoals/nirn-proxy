package lib

import (
	"io"
	"os"
	"testing"

	"github.com/sirupsen/logrus"
)

// TestMain installs the logger main.go would, discarding its output. Without
// it the package logger is nil, and any test that reaches a queue panics on
// its first log line.
func TestMain(m *testing.M) {
	l := logrus.New()
	l.SetOutput(io.Discard)
	SetLogger(l)
	os.Exit(m.Run())
}
