package sqlitedb

import (
	"os"
	"testing"

	"github.com/rezzminator/callmeter/internal/testjail"
)

// TestMain jails HOME, TMPDIR and CALLMETER_HOME before any test runs. See
// internal/testjail.
func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }
