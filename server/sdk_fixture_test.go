package main

// Instinct, acting on behalf of Tal Chaim Damri.
// This opt-in bridge exposes the existing Go handler-test server to SDK clients.
import (
	"net"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

func TestSDKFixture(t *testing.T) {
	ready := os.Getenv("ACQ_SDK_FIXTURE_READY")
	if ready == "" {
		t.Skip("only started by test/mcp-sdk")
	}
	// Reuse real handlers and state isolation; never call production main().
	server := newTestServer(t)
	defer server.Close()
	addr, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		t.Fatal("fixture must listen on loopback")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(stop)
	if err := os.WriteFile(ready, []byte(server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	<-stop
}
