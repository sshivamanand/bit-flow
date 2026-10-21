package discovery

import (
	"context"
	"net"
	"testing"
	"time"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func TestRespondAndScanUnicast(t *testing.T) {
	port := freeUDPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	me := Info{Proto: 3, ID: "abc", Name: "Desk", OS: "darwin", Port: 7700, Role: RoleReceive}
	go Respond(ctx, port, func() Info { return me })
	time.Sleep(100 * time.Millisecond)

	peers, err := Scan(ctx, ScanOptions{Port: port, Wait: 600 * time.Millisecond, NoProbe: true, Targets: []string{"127.0.0.1"}, SelfID: "me"})
	if err != nil {
		t.Fatal(err)
	}
	var got *Peer
	for i := range peers {
		if peers[i].ID == "abc" {
			got = &peers[i]
		}
	}
	if got == nil {
		t.Fatalf("responder not found: %+v", peers)
	}
	if got.Name != "Desk" || got.Role != RoleReceive || got.Addr() == "" {
		t.Fatalf("peer %+v", got)
	}

	// A scanner never lists itself.
	peers, _ = Scan(ctx, ScanOptions{Port: port, Wait: 400 * time.Millisecond, NoProbe: true, Targets: []string{"127.0.0.1"}, SelfID: "abc"})
	for _, p := range peers {
		if p.ID == "abc" {
			t.Fatal("scanner listed itself")
		}
	}
}

func TestFindByName(t *testing.T) {
	port := freeUDPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Respond(ctx, port, func() Info { return Info{Proto: 3, ID: "x1", Name: "Gaming-Rig", Port: 7700, Role: RoleReceive} })
	time.Sleep(100 * time.Millisecond)
	peers, err := Find(ctx, "gaming", ScanOptions{Port: port, Wait: 500 * time.Millisecond, NoProbe: true, Targets: []string{"127.0.0.1"}})
	if err != nil || len(peers) == 0 || peers[0].ID != "x1" {
		t.Fatalf("peers=%+v err=%v", peers, err)
	}
}

func TestSubnetHostsAndBroadcast(t *testing.T) {
	for _, h := range subnetHosts() {
		if net.ParseIP(h) == nil {
			t.Fatalf("bad host %q", h)
		}
	}
	if b := broadcastAddrs(); len(b) == 0 || !b[0].Equal(net.IPv4bcast) {
		t.Fatalf("broadcast addrs %v", b)
	}
}
