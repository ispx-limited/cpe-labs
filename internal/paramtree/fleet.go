package paramtree

import (
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"regexp"
	"strconv"
)

// reservedPoolNames are placeholder names the fleet substitution system
// already owns. Pools cannot use these as names because {cpe} /
// {cpe_id} placeholders would otherwise collide with named-pool
// placeholders.
var reservedPoolNames = map[string]struct{}{
	"cpe":    {},
	"cpe_id": {},
	"i":      {},
	"base":   {},
}

// poolNameRE is the syntax pool names must satisfy: ASCII letter or
// underscore, then letters / digits / underscores. Stricter than
// "anything that doesn't break YAML" so the placeholder parser doesn't
// have to deal with weird characters in {pool_name} forms.
var poolNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateFleetPool checks one pool entry from fleet.pools and returns
// the typed FleetPool. Used at LoadProfile time so misconfiguration
// surfaces before any CPE is built.
func validateFleetPool(name string, raw rawFleetPool) (FleetPool, error) {
	if name == "" {
		return FleetPool{}, fmt.Errorf("pool name is empty")
	}
	if !poolNameRE.MatchString(name) {
		return FleetPool{}, fmt.Errorf("pool name %q must match %s", name, poolNameRE.String())
	}
	if _, reserved := reservedPoolNames[name]; reserved {
		return FleetPool{}, fmt.Errorf("pool name %q is reserved (clashes with built-in placeholders {cpe}/{cpe_id}/etc.)", name)
	}
	switch raw.Type {
	case "ipv4":
		if raw.CIDR == "" {
			return FleetPool{}, fmt.Errorf("type=ipv4 requires cidr")
		}
		ip, ipnet, err := net.ParseCIDR(raw.CIDR)
		if err != nil {
			return FleetPool{}, fmt.Errorf("cidr %q: %w", raw.CIDR, err)
		}
		if ip.To4() == nil {
			return FleetPool{}, fmt.Errorf("cidr %q is not an IPv4 network", raw.CIDR)
		}
		_ = ipnet // parsed for validation only; resolver re-parses
	case "ipv6":
		if raw.CIDR == "" {
			return FleetPool{}, fmt.Errorf("type=ipv6 requires cidr")
		}
		ip, _, err := net.ParseCIDR(raw.CIDR)
		if err != nil {
			return FleetPool{}, fmt.Errorf("cidr %q: %w", raw.CIDR, err)
		}
		if ip.To4() != nil {
			return FleetPool{}, fmt.Errorf("cidr %q is not an IPv6 network", raw.CIDR)
		}
	case "ipv6prefix":
		if raw.Super == "" {
			return FleetPool{}, fmt.Errorf("type=ipv6prefix requires super")
		}
		ip, ipnet, err := net.ParseCIDR(raw.Super)
		if err != nil {
			return FleetPool{}, fmt.Errorf("super %q: %w", raw.Super, err)
		}
		if ip.To4() != nil {
			return FleetPool{}, fmt.Errorf("super %q must be an IPv6 network", raw.Super)
		}
		superLen, _ := ipnet.Mask.Size()
		if raw.SubLen <= superLen {
			return FleetPool{}, fmt.Errorf("sublen %d must be greater than super prefix length %d", raw.SubLen, superLen)
		}
		if raw.SubLen > 128 {
			return FleetPool{}, fmt.Errorf("sublen %d exceeds 128", raw.SubLen)
		}
	case "":
		return FleetPool{}, fmt.Errorf("type is required")
	default:
		return FleetPool{}, fmt.Errorf("type %q unsupported (want ipv4, ipv6, ipv6prefix)", raw.Type)
	}
	return FleetPool(raw), nil
}

// ResolvePool returns the per-instance string value for the pool. The
// instance index is 1-based to match cpe-1 / cpe-2 / ... naming.
//
// For ipv4/ipv6 pools, returns the Nth host in the CIDR
// (instance 1 -> network base + 1, etc.). For ipv6prefix, returns the
// Nth /SubLen prefix carved from Super formatted as "<addr>/<sublen>".
//
// Returns an error when instance exceeds the pool's capacity so the
// caller can surface "fleet.count is bigger than pool X can hold" at
// CPE-construction time rather than producing wrong addresses.
func ResolvePool(p FleetPool, instance int) (string, error) {
	if instance < 1 {
		return "", fmt.Errorf("instance must be >= 1, got %d", instance)
	}
	switch p.Type {
	case "ipv4":
		return resolveIPv4(p.CIDR, instance)
	case "ipv6":
		return resolveIPv6(p.CIDR, instance)
	case "ipv6prefix":
		return resolveIPv6Prefix(p.Super, p.SubLen, instance)
	default:
		return "", fmt.Errorf("unsupported pool type %q", p.Type)
	}
}

// resolveIPv4 returns the Nth host (1-based) in the CIDR. Capacity is
// 2^(32-prefixLen) - 1 (skips network base; broadcast is allowed but
// flagged at the upper bound by the operator profile if they care).
func resolveIPv4(cidr string, instance int) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("cidr %q: %w", cidr, err)
	}
	if !prefix.Addr().Is4() {
		return "", fmt.Errorf("cidr %q is not IPv4", cidr)
	}
	bits := prefix.Bits()
	hostBits := 32 - bits
	if hostBits == 0 {
		// /32, only one address; instance must be 1 and we return
		// the address itself.
		if instance != 1 {
			return "", fmt.Errorf("/32 pool can only hold instance 1; got %d", instance)
		}
		return prefix.Addr().String(), nil
	}
	maxHost := uint64(1)<<uint(hostBits) - 1 // exclude network base
	if uint64(instance) > maxHost {
		return "", fmt.Errorf("instance %d exceeds capacity %d for cidr %s", instance, maxHost, cidr)
	}
	base := prefix.Masked().Addr().As4()
	baseInt := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	addrInt := baseInt + uint32(instance) //nolint:gosec // checked above
	out := [4]byte{
		byte(addrInt >> 24),
		byte(addrInt >> 16),
		byte(addrInt >> 8),
		byte(addrInt),
	}
	return netip.AddrFrom4(out).String(), nil
}

// resolveIPv6 returns the Nth host in the IPv6 CIDR.
func resolveIPv6(cidr string, instance int) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("cidr %q: %w", cidr, err)
	}
	if !prefix.Addr().Is6() || prefix.Addr().Is4() {
		return "", fmt.Errorf("cidr %q is not IPv6", cidr)
	}
	bits := prefix.Bits()
	hostBits := 128 - bits
	// Capacity check is best-effort, for very long host portions we
	// trust the operator (instance is a Go int, capped at int64 max).
	if hostBits < 64 {
		maxHost := uint64(1)<<uint(hostBits) - 1
		if uint64(instance) > maxHost {
			return "", fmt.Errorf("instance %d exceeds capacity %d for cidr %s", instance, maxHost, cidr)
		}
	}
	base := prefix.Masked().Addr().As16()
	baseBig := new(big.Int).SetBytes(base[:])
	addrBig := new(big.Int).Add(baseBig, big.NewInt(int64(instance)))
	return ipv6BigIntToAddr(addrBig).String(), nil
}

// resolveIPv6Prefix returns the Nth /SubLen prefix carved from the
// super-prefix. Format: "<address>/<sublen>".
func resolveIPv6Prefix(super string, subLen, instance int) (string, error) {
	prefix, err := netip.ParsePrefix(super)
	if err != nil {
		return "", fmt.Errorf("super %q: %w", super, err)
	}
	if !prefix.Addr().Is6() || prefix.Addr().Is4() {
		return "", fmt.Errorf("super %q is not IPv6", super)
	}
	superLen := prefix.Bits()
	if subLen <= superLen || subLen > 128 {
		return "", fmt.Errorf("sublen %d invalid for super /%d", subLen, superLen)
	}
	selectorBits := subLen - superLen
	hostBits := 128 - subLen
	if selectorBits < 64 {
		maxHost := uint64(1) << uint(selectorBits)
		if uint64(instance) >= maxHost {
			return "", fmt.Errorf("instance %d exceeds capacity %d for super /%d -> /%d",
				instance, maxHost-1, superLen, subLen)
		}
	}
	base := prefix.Masked().Addr().As16()
	baseBig := new(big.Int).SetBytes(base[:])
	// shift instance left into the selector bit window: instance << hostBits
	shifted := new(big.Int).Lsh(big.NewInt(int64(instance)), uint(hostBits))
	addrBig := new(big.Int).Or(baseBig, shifted)
	return fmt.Sprintf("%s/%d", ipv6BigIntToAddr(addrBig).String(), subLen), nil
}

// ipv6BigIntToAddr converts a big.Int (treated as 128-bit MSB-first)
// to a netip.Addr. Pads / truncates to exactly 16 bytes.
func ipv6BigIntToAddr(n *big.Int) netip.Addr {
	b := n.Bytes()
	var out [16]byte
	if len(b) > 16 {
		copy(out[:], b[len(b)-16:])
	} else {
		copy(out[16-len(b):], b)
	}
	return netip.AddrFrom16(out)
}

// PoolNamePattern returns the regex source pool names must match.
// Exposed for documentation and validation in upstream tooling.
func PoolNamePattern() string { return poolNameRE.String() }

// Avoid the lint warning about strconv being unused if the build
// changes; strconv is currently consumed only via the test helper. We
// keep the import below in fleet_test.go.
var _ = strconv.Itoa
