package cli

import (
	"errors"
	"net"
	"testing"
)

// TestIsAddrInUseDetectsHeldPort binds a real port twice so the check is
// exercised with the error each platform's network stack actually returns.
func TestIsAddrInUseDetectsHeldPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	second, err := net.Listen("tcp", ln.Addr().String())
	if err == nil {
		second.Close()
		t.Fatal("second listen on a held port succeeded")
	}
	if !isAddrInUse(err) {
		t.Fatalf("isAddrInUse(%v) = false, want true", err)
	}
	if isAddrInUse(errors.New("permission denied")) {
		t.Error("isAddrInUse reported an unrelated error as address in use")
	}
}
