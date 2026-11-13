package engine

import (
	"encoding/json"
	"net"
	"testing"

	"netmerge-android/internal/nics"
)

func TestParsePorts(t *testing.T) {
	good := map[string]string{
		"":                       "none",
		"none":                   "none",
		"3074":                   "3074",
		"7000-8000, 3074":        "3074,7000-8000",
		"10,11,12,20-30,25-40":   "10-12,20-40",
		"1-5,6-9":                "1-9",
		" 443 ":                  "443",
		"65535,1":                "1,65535",
		"9000-9000":              "9000",
		"all":                    allPorts,
		"7000-7100,7050-7200,80": "80,7000-7200",
	}
	for in, want := range good {
		p, err := ParsePorts(in)
		if err != nil {
			t.Errorf("ParsePorts(%q): %v", in, err)
			continue
		}
		if in == "all" {
			for _, port := range []int{53, 123, 443, 5353} {
				if p.Has(port) {
					t.Errorf("all must not include %d", port)
				}
			}
			for _, port := range []int{1, 52, 54, 7777, 27015, 65535} {
				if !p.Has(port) {
					t.Errorf("all must include %d", port)
				}
			}
			continue
		}
		if got := p.String(); got != want {
			t.Errorf("ParsePorts(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"0", "65536", "abc", "10-5", "1-", "-5x", "1,,x"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("ParsePorts(%q) should fail", bad)
		}
	}
}

func TestPortSetHas(t *testing.T) {
	p, _ := ParsePorts("100-200,3074,9000-9010")
	in := []int{100, 150, 200, 3074, 9000, 9005, 9010}
	out := []int{0, 1, 99, 201, 3073, 3075, 8999, 9011, 65535}
	for _, x := range in {
		if !p.Has(x) {
			t.Errorf("want %d in set", x)
		}
	}
	for _, x := range out {
		if p.Has(x) {
			t.Errorf("want %d not in set", x)
		}
	}
	if (PortSet{}).Has(80) {
		t.Error("empty set has nothing")
	}
}

func TestSplitConfigJSON(t *testing.T) {
	udp, _ := ParsePorts("7000-8000,3074")
	c := SplitConfig{Enabled: true, UDP: udp, GameNICs: []string{"wwan0"}, Strict: true}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back SplitConfig
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(c) || !back.Enabled || !back.Strict || !back.UDP.Has(7500) || back.TCP.Has(1) {
		t.Fatalf("round trip lost data: %s -> %+v", b, back)
	}
	if err := json.Unmarshal([]byte(`{"udp":"nope"}`), &back); err == nil {
		t.Fatal("bad ports in the file must be an error")
	}
}

func splitNics() []nics.Nic {
	return []nics.Nic{
		{Name: "wlan0", IP: net.ParseIP("192.168.1.5"), Weight: 1},
		{Name: "wwan0", IP: net.ParseIP("10.20.30.40"), Weight: 1, Cellular: true},
	}
}

func newSplitBalancer(cfg SplitConfig) *Balancer {
	b := NewBalancer(NewScheduler(splitNics()))
	b.SetSplit(cfg)
	return b
}

func ports(s string) PortSet { p, _ := ParsePorts(s); return p }

// pick runs the same steps DialTCP/ListenUDP do, without a socket.
func pick(b *Balancer, udp bool, port int) *Slot {
	return b.acquire(Fam4, map[string]struct{}{}, b.routeFor(udp, port))
}

func TestSplitRoutesGameToCellularAndRestToWifi(t *testing.T) {
	b := newSplitBalancer(SplitConfig{Enabled: true, UDP: ports("7000-8000,3074")})
	for range 20 {
		if got := pick(b, true, 7500); got == nil || got.Name != "wwan0" {
			t.Fatalf("game UDP must use wwan0, got %v", got)
		}
		if got := pick(b, true, 3074); got == nil || got.Name != "wwan0" {
			t.Fatalf("game UDP 3074 must use wwan0, got %v", got)
		}
		if got := pick(b, true, 443); got == nil || got.Name != "wlan0" {
			t.Fatalf("QUIC must stay on wlan0, got %v", got)
		}
		if got := pick(b, false, 443); got == nil || got.Name != "wlan0" {
			t.Fatalf("web TCP must stay on wlan0, got %v", got)
		}
		// TCP to a game UDP port number is not a game flow: the sets are per protocol.
		if got := pick(b, false, 7500); got == nil || got.Name != "wlan0" {
			t.Fatalf("TCP on a UDP game port must stay on wlan0, got %v", got)
		}
	}
}

func TestSplitOffOrWithoutPortsChangesNothing(t *testing.T) {
	for _, cfg := range []SplitConfig{
		{},
		{Enabled: false, UDP: ports("7000")},
		{Enabled: true}, // on, but no ports: must not strand all traffic on wlan0
	} {
		b := newSplitBalancer(cfg)
		if b.routeFor(true, 7000) != nil || b.routeFor(false, 80) != nil {
			t.Fatalf("%+v must not restrict flows", cfg)
		}
		seen := map[string]bool{}
		for range 40 {
			seen[pick(b, false, 80).Name] = true
		}
		if !seen["wlan0"] || !seen["wwan0"] {
			t.Fatalf("%+v: normal balancing must use both adapters, saw %v", cfg, seen)
		}
	}
}

func TestSplitNamedGameAdapter(t *testing.T) {
	// A phone tether is not detected as cellular, so it is named. wlan0 and wwan0 both then count as "other".
	ns := append(splitNics(), nics.Nic{Name: "usb0", IP: net.ParseIP("192.168.42.2"), Weight: 1})
	b := NewBalancer(NewScheduler(ns))
	b.SetSplit(SplitConfig{Enabled: true, UDP: ports("27015"), GameNICs: []string{"USB0"}})
	for range 20 {
		if got := pick(b, true, 27015); got.Name != "usb0" {
			t.Fatalf("game must use the named adapter, got %s", got.Name)
		}
		if got := pick(b, false, 80); got.Name == "usb0" {
			t.Fatal("other traffic must never use the game adapter")
		}
	}
}

func TestSplitOtherAdapterList(t *testing.T) {
	ns := append(splitNics(), nics.Nic{Name: "eth0", IP: net.ParseIP("192.168.2.2"), Weight: 1})
	b := NewBalancer(NewScheduler(ns))
	b.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000"), OtherNICs: []string{"wlan0"}})
	for range 20 {
		if got := pick(b, false, 80); got.Name != "wlan0" {
			t.Fatalf("other traffic must stay on wlan0, got %s", got.Name)
		}
	}
}

func TestSplitFallback(t *testing.T) {
	// Without strict, a group with no working adapter borrows the other one.
	b := newSplitBalancer(SplitConfig{Enabled: true, UDP: ports("7000-8000")})
	cell := b.S.slots[1]
	b.S.SetState(cell, Fam4, false) // modem has no internet
	if got := pick(b, true, 7500); got == nil || got.Name != "wlan0" {
		t.Fatalf("game flow must fall back to wlan0 while the modem is down, got %v", got)
	}
	b.S.SetState(cell, Fam4, true) // modem recovers
	if got := pick(b, true, 7500); got.Name != "wwan0" {
		t.Fatalf("game flow must return to wwan0, got %s", got.Name)
	}
	// Wi-Fi down: the rest of the internet borrows the modem rather than going dark.
	b.S.SetState(b.S.slots[0], Fam4, false)
	if got := pick(b, false, 443); got == nil || got.Name != "wwan0" {
		t.Fatalf("other flow must fall back to wwan0 while wlan0 is down, got %v", got)
	}
}

func TestSplitStrict(t *testing.T) {
	b := newSplitBalancer(SplitConfig{Enabled: true, UDP: ports("7000-8000"), Strict: true})
	b.S.SetState(b.S.slots[1], Fam4, false)
	// Strict never borrows, even though the group is down: the flow still uses the group's adapter
	// (last resort within the group), it does not leak onto wlan0.
	if got := pick(b, true, 7500); got == nil || got.Name != "wwan0" {
		t.Fatalf("strict game flow must stay on wwan0, got %v", got)
	}
	// No game adapter at all: strict means no flow.
	b2 := NewBalancer(NewScheduler([]nics.Nic{nic("wlan0", "192.168.1.5", 1)}))
	b2.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000-8000"), Strict: true})
	if got := pick(b2, true, 7500); got != nil {
		t.Fatalf("strict with no game adapter must refuse, got %s", got.Name)
	}
	if got := pick(b2, false, 443); got == nil || got.Name != "wlan0" {
		t.Fatal("other traffic must still flow")
	}
	// Not strict: with no modem the game just uses Wi-Fi.
	b2.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000-8000")})
	if got := pick(b2, true, 7500); got == nil || got.Name != "wlan0" {
		t.Fatalf("non-strict with no modem must use wlan0, got %v", got)
	}
}

func TestSplitRetryExclusion(t *testing.T) {
	// An adapter a flow already failed on is not picked again, in either group.
	b := newSplitBalancer(SplitConfig{Enabled: true, UDP: ports("7000-8000")})
	tried := map[string]struct{}{"wwan0": {}}
	got := b.acquire(Fam4, tried, b.routeFor(true, 7500))
	if got == nil || got.Name != "wlan0" {
		t.Fatalf("after wwan0 failed non-strict must try wlan0, got %v", got)
	}
	b.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000-8000"), Strict: true})
	if got := b.acquire(Fam4, tried, b.routeFor(true, 7500)); got != nil {
		t.Fatalf("strict must not leave the group, got %s", got.Name)
	}
}

func TestSplitState(t *testing.T) {
	b := newSplitBalancer(SplitConfig{})
	if got := b.SplitState(); got != "off" {
		t.Fatal(got)
	}
	b.SetSplit(SplitConfig{Enabled: true})
	if got := b.SplitState(); got == "off" || got == "" {
		t.Fatal(got)
	}
	b.SetSplit(SplitConfig{Enabled: true, UDP: ports("7000-8000")})
	want := "on: UDP 7000-8000 via wwan0; everything else via wlan0"
	if got := b.SplitState(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
