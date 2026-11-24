package repos

import (
	"os"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/repos/repostest"
)

func TestMain(m *testing.M) {
	os.Exit(repostest.Guard(m).Run())
}
