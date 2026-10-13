package proxy

import (
	"fmt"
	"net"
	"testing"
)

// occupyUDPOnlyPort keeps UDP occupied while leaving the same TCP port free.
func occupyUDPOnlyPort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen TCP: %v", err)
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		udp, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		_ = tcp.Close()
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = udp.Close() })
		return port
	}
	t.Fatal("could not reserve a UDP-only loopback port")
	return 0
}

func TestListenTCPAndUDPRejectsUDPOnlyPort(t *testing.T) {
	port := occupyUDPOnlyPort(t)
	listener, packetConn, err := listenTCPAndUDP(port)
	if listener != nil {
		defer listener.Close()
	}
	if packetConn != nil {
		defer packetConn.Close()
	}
	if err == nil || listener != nil || packetConn != nil {
		t.Fatalf("occupied UDP port was accepted: TCP=%v UDP=%v err=%v", listener, packetConn, err)
	}
	// A failed UDP probe must release the TCP listener it already opened.
	tcp, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("failed UDP probe leaked the TCP listener: %v", err)
	}
	_ = tcp.Close()
}

func TestListenTCPAndUDPRejectsTCPOnlyPort(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	listener, packetConn, err := listenTCPAndUDP(tcp.Addr().(*net.TCPAddr).Port)
	if listener != nil {
		defer listener.Close()
	}
	if packetConn != nil {
		defer packetConn.Close()
	}
	if err == nil || listener != nil || packetConn != nil {
		t.Fatalf("occupied TCP port was accepted: TCP=%v UDP=%v err=%v", listener, packetConn, err)
	}
}

func TestListenTCPAndUDPReservesAndReleasesBoth(t *testing.T) {
	var listener net.Listener
	var packetConn net.PacketConn
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		listener, packetConn, err = listenTCPAndUDP(0)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer packetConn.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	tcp, err := net.Listen("tcp", addr)
	if err == nil {
		_ = tcp.Close()
		t.Fatal("TCP was not reserved")
	}
	udp, err := net.ListenPacket("udp", addr)
	if err == nil {
		_ = udp.Close()
		t.Fatal("UDP was not reserved")
	}
	_ = listener.Close()
	_ = packetConn.Close()
	tcp, udp, err = listenTCPAndUDP(port)
	if err != nil {
		t.Fatalf("TCP/UDP port was not released: %v", err)
	}
	_ = tcp.Close()
	_ = udp.Close()
}

func TestNextAvailablePortSupportsTCPAndUDP(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		port, err := nextAvailablePort()
		if err != nil {
			t.Fatal(err)
		}
		listener, packetConn, err := listenTCPAndUDP(port)
		if err != nil {
			t.Fatalf("allocated port %d is not available for TCP and UDP: %v", port, err)
		}
		_ = listener.Close()
		_ = packetConn.Close()
	}
}

func TestNextAvailablePortWithRetryExhausted(t *testing.T) {
	port, err := nextAvailablePortWithRetry(0)
	if port != 0 || err == nil {
		t.Fatalf("zero retry budget returned port=%d err=%v", port, err)
	}
}
