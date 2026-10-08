package bus

import (
	"os"
	"testing"
)

func TestIsTerminalPTY(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	if !IsTerminal(ptmx.Fd()) {
		t.Fatal("a pty master is a terminal")
	}
}
