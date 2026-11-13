package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// Split mode keeps two kinds of traffic on different adapters at the same time: flows to the
// "game" ports (typically UDP) leave through the game adapters (by default every cellular modem),
// and every other flow (social media, browsers, downloads) is balanced over the remaining
// adapters (typically Wi-Fi). Both groups are used at once; nothing is merged across the border.
//
// The decision is per flow, made where the scheduler picks an adapter, so it costs nothing on the
// data path. It is by destination port, because net-merge sees flows, not apps.

// PortSet is a set of ports kept as sorted, non-overlapping inclusive ranges.
type PortSet struct{ r [][2]uint16 }

// allPorts is what the word "all" means: every port except the ones that are never a game and
// would hurt if they left over mobile data: DNS (53), NTP (123), QUIC/HTTP3 (443) and mDNS (5353).
const allPorts = "1-52,54-122,124-442,444-5352,5354-65535"

// ParsePorts reads "7000-8000,3074,all". An empty string, "none" or "-" is the empty set.
func ParsePorts(s string) (PortSet, error) {
	var set PortSet
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "none" || s == "-" {
		return set, nil
	}
	if s == "all" {
		s = allPorts
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := parsePort(lo)
		if err != nil {
			return PortSet{}, fmt.Errorf("port %q: %w", part, err)
		}
		b := a
		if isRange {
			if b, err = parsePort(hi); err != nil {
				return PortSet{}, fmt.Errorf("port %q: %w", part, err)
			}
		}
		if b < a {
			return PortSet{}, fmt.Errorf("port range %q runs backwards", part)
		}
		set.r = append(set.r, [2]uint16{a, b})
	}
	set.normalize()
	return set, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("must be a number from 1 to 65535")
	}
	return uint16(n), nil
}

func (p *PortSet) normalize() {
	sort.Slice(p.r, func(i, j int) bool { return p.r[i][0] < p.r[j][0] })
	out := p.r[:0]
	for _, x := range p.r {
		if n := len(out); n > 0 && int(x[0]) <= int(out[n-1][1])+1 {
			out[n-1][1] = max(out[n-1][1], x[1])
			continue
		}
		out = append(out, x)
	}
	p.r = out
}

// Has reports whether port is in the set.
func (p PortSet) Has(port int) bool {
	i := sort.Search(len(p.r), func(i int) bool { return int(p.r[i][1]) >= port })
	return i < len(p.r) && int(p.r[i][0]) <= port
}

func (p PortSet) Empty() bool { return len(p.r) == 0 }

func (p PortSet) String() string {
	if p.Empty() {
		return "none"
	}
	parts := make([]string, len(p.r))
	for i, x := range p.r {
		if x[0] == x[1] {
			parts[i] = strconv.Itoa(int(x[0]))
		} else {
			parts[i] = fmt.Sprintf("%d-%d", x[0], x[1])
		}
	}
	return strings.Join(parts, ",")
}

func (p PortSet) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

func (p *PortSet) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParsePorts(s)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// SplitConfig is the whole split-mode setting. It is saved as split.json.
type SplitConfig struct {
	Enabled bool    `json:"enabled"`
	UDP     PortSet `json:"udp"` // destination UDP ports that go to the game adapters
	TCP     PortSet `json:"tcp"` // destination TCP ports that go to the game adapters (optional)
	// GameNICs names the adapters game flows use. Empty means every adapter detected as cellular
	// (wwan*, rmnet*, ccmni*, ... or a cellular modem driver). Name a phone tether here (usb0, enx...).
	GameNICs []string `json:"game_nics,omitempty"`
	// OtherNICs names the adapters everything else uses. Empty means every adapter that is not a
	// game adapter.
	OtherNICs []string `json:"other_nics,omitempty"`
	// Strict forbids falling back: with it, game flows fail when no game adapter works and other
	// flows never touch a game adapter. Without it, a group that has no working adapter borrows the
	// other group's, so the internet (and the game) keep working when Wi-Fi or the modem drops.
	Strict bool `json:"strict,omitempty"`
}

// Active reports whether split mode changes anything: it is on and has ports to split on.
func (c SplitConfig) Active() bool { return c.Enabled && !(c.UDP.Empty() && c.TCP.Empty()) }

func (c SplitConfig) isGame(name string, cellular bool) bool {
	if len(c.GameNICs) > 0 {
		return containsFold(c.GameNICs, name)
	}
	return cellular
}

func (c SplitConfig) isOther(name string, cellular bool) bool {
	if c.isGame(name, cellular) {
		return false
	}
	return len(c.OtherNICs) == 0 || containsFold(c.OtherNICs, name)
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// Equal compares two settings by their saved form.
func (c SplitConfig) Equal(o SplitConfig) bool {
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(o)
	return string(a) == string(b)
}

// route is the adapter restriction for one flow. A nil route means "any adapter".
type route struct {
	skip   map[string]struct{} // adapters this flow must not use
	strict bool
}

type splitHolder struct{ p atomic.Pointer[SplitConfig] }

// SetSplit replaces the split setting; it applies to flows opened from now on (existing flows
// stay on the adapter they started on, like everywhere else in net-merge).
func (b *Balancer) SetSplit(c SplitConfig) { b.split.p.Store(&c) }

// Split returns the current split setting.
func (b *Balancer) Split() SplitConfig {
	if c := b.split.p.Load(); c != nil {
		return *c
	}
	return SplitConfig{}
}

// routeFor decides which adapters a new flow to port may use. udp tells UDP from TCP.
func (b *Balancer) routeFor(udp bool, port int) *route {
	c := b.split.p.Load()
	if c == nil || !c.Active() {
		return nil
	}
	ports := c.TCP
	if udp {
		ports = c.UDP
	}
	keep := c.isOther
	if ports.Has(port) {
		keep = c.isGame
	}
	return &route{skip: b.S.Exclude(keep), strict: c.Strict}
}

// acquire picks an adapter for a flow under r. tried are adapters this flow already failed on.
func (b *Balancer) acquire(f Family, tried map[string]struct{}, r *route) *Slot {
	if r == nil {
		return b.S.AcquireFor(f, tried)
	}
	skip := make(map[string]struct{}, len(tried)+len(r.skip))
	for k := range tried {
		skip[k] = struct{}{}
	}
	for k := range r.skip {
		skip[k] = struct{}{}
	}
	// Without strict mode the group's adapters are used only while they work: a dead one yields
	// to the other group instead of being the "least bad" choice.
	if sl := b.S.acquire(f, skip, !r.strict); sl != nil || r.strict {
		return sl
	}
	return b.S.AcquireFor(f, tried)
}

// SplitState describes split mode for `status` and the tray.
func (b *Balancer) SplitState() string {
	c := b.Split()
	switch {
	case !c.Enabled:
		return "off"
	case !c.Active():
		return "on, but no game ports are set (net-merge split udp <ports>)"
	}
	game := b.S.Names(c.isGame)
	other := b.S.Names(c.isOther)
	var ports []string
	if !c.UDP.Empty() {
		ports = append(ports, "UDP "+c.UDP.String())
	}
	if !c.TCP.Empty() {
		ports = append(ports, "TCP "+c.TCP.String())
	}
	s := fmt.Sprintf("on: %s via %s; everything else via %s", strings.Join(ports, " + "), listOrNone(game, "no cellular adapter found"), listOrNone(other, "none"))
	if c.Strict {
		s += " (strict)"
	}
	return s
}

func listOrNone(l []string, none string) string {
	if len(l) == 0 {
		return none
	}
	return strings.Join(l, ", ")
}
