package advisor

import (
	"strings"
	"testing"
)

func TestDoubleHop(t *testing.T) {
	tips := Tips(Link{"Wi-Fi", "192.168.1.20"}, Link{"Wi-Fi", "192.168.1.40"}, []string{"Wi-Fi"})
	if len(tips) != 1 || !strings.Contains(tips[0], "twice") {
		t.Fatalf("tips %q", tips)
	}
}

func TestWiredSideNoAdvice(t *testing.T) {
	if tips := Tips(Link{"Wi-Fi", "192.168.1.20"}, Link{"Ethernet", "192.168.1.40"}, []string{"Wi-Fi"}); len(tips) != 0 {
		t.Fatalf("one wired side needs no advice: %q", tips)
	}
}

func TestMultipathAndCable(t *testing.T) {
	tips := Tips(Link{"Wi-Fi", "192.168.1.20"}, Link{"Thunderbolt Bridge", "169.254.3.4"}, []string{"Wi-Fi", "Thunderbolt Bridge"})
	if len(tips) != 2 || !strings.Contains(tips[0], "Wi-Fi + Thunderbolt Bridge") {
		t.Fatalf("tips %q", tips)
	}
}
