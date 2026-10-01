package command

import (
	"os"
	"testing"

	"github.com/rezzminator/callmeter/internal/testjail"
)

func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }
