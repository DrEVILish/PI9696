package hardware

import (
	"strings"
	"testing"
)

// /proc/net/route stores addresses little-endian hex; a byte-order slip
// shows the gateway reversed (1.10.168.192) on the Network Info page.
func TestHexToIP(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"010200C0", "192.0.2.1"},
		{"00000000", "0.0.0.0"},
		{"FFFFFFFF", "255.255.255.255"},
		{"zz0200C0", ""}, // not hex
		{"0200C0", ""},   // 3 bytes
		{"", ""},
	} {
		if got := hexToIP(tc.in); got != tc.want {
			t.Errorf("hexToIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const routeTable = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	FE0200C0	0003	0	0	600	00000000	0	0	0
eth0	000200C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
eth0	00000000	010200C0	0003	0	0	100	00000000	0	0	0
`

// The default route via our own interface wins even when another
// interface's default route comes first; otherwise the first one found is
// the fallback; non-default routes and malformed gateways are ignored.
func TestParseDefaultGateway(t *testing.T) {
	if got := parseDefaultGateway(strings.NewReader(routeTable), "eth0"); got != "192.0.2.1" {
		t.Errorf("eth0 gateway = %q, want 192.0.2.1 (not wlan0's)", got)
	}
	if got := parseDefaultGateway(strings.NewReader(routeTable), "usb0"); got != "192.0.2.254" {
		t.Errorf("no route via usb0: fallback = %q, want the first default route 192.0.2.254", got)
	}
	noDefault := "Iface\tDestination\tGateway\neth0\t000200C0\t00000000\n"
	if got := parseDefaultGateway(strings.NewReader(noDefault), "eth0"); got != "" {
		t.Errorf("no default route: got %q, want empty", got)
	}
	bad := "Iface\tDestination\tGateway\neth0\t00000000\tXYZ12345\neth0\t00000000\t0102\n"
	if got := parseDefaultGateway(strings.NewReader(bad), "eth0"); got != "" {
		t.Errorf("malformed gateways: got %q, want empty", got)
	}
}
