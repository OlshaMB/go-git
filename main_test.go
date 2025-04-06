package git

import (
	"testing"

	"github.com/go-git/go-git/v6/internal/trace"
)

func TestMain(t *testing.M) {
	// Set the trace targets based on the environment variables.
	trace.ReadEnv()
	// Run the tests.
	t.Run()
}
