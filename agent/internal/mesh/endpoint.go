package mesh

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// ListenPort is the WireGuard UDP port (matches RenderConfig's ListenPort).
const ListenPort = 51820

const stunMagicCookie = 0x2112A442

// stunServers are public STUN servers tried in order to learn the host's public
// IP. Best-effort; a failure falls back to the local outbound IP.
var stunServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}

// DiscoverEndpoint returns this host's best guess at its reachable WireGuard
// endpoint ("publicIP:51820"). It STUN-probes to learn the public IP and pairs
// it with the WG listen port. STUN reveals the mapping of the probe's own
// ephemeral port, not WireGuard's, so the port is assumed preserved by the NAT
// (or forwarded) — the ~5-10% strict/symmetric-NAT case is a documented
// limitation that simply reports an endpoint peers cannot reach. On STUN
// failure it falls back to the local outbound IP (useful for same-LAN/VPN
// peers). Empty string + error when nothing is determinable.
func DiscoverEndpoint(ctx context.Context) (string, error) {
	if ip := stunPublicIP(ctx); ip != "" {
		return net.JoinHostPort(ip, fmt.Sprintf("%d", ListenPort)), nil
	}
	if ip := localOutboundIP(); ip != "" {
		return net.JoinHostPort(ip, fmt.Sprintf("%d", ListenPort)), nil
	}
	return "", fmt.Errorf("could not determine a reachable endpoint")
}

func stunPublicIP(ctx context.Context) string {
	for _, srv := range stunServers {
		if ip, err := stunQuery(ctx, srv); err == nil && ip != "" {
			return ip
		}
	}
	return ""
}

func stunQuery(ctx context.Context, server string) (string, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	// STUN binding request: type 0x0001, zero length, magic cookie, random txid.
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)
	binary.BigEndian.PutUint16(req[2:], 0x0000)
	binary.BigEndian.PutUint32(req[4:], stunMagicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return "", err
	}
	if _, err := conn.Write(req); err != nil {
		return "", err
	}
	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return "", err
	}
	return parseSTUNAddress(resp[:n])
}

// parseSTUNAddress walks a STUN response's attributes for a mapped IPv4 address,
// preferring XOR-MAPPED-ADDRESS (0x0020) over the legacy MAPPED-ADDRESS (0x0001).
func parseSTUNAddress(msg []byte) (string, error) {
	if len(msg) < 20 {
		return "", fmt.Errorf("short STUN response")
	}
	attrs := msg[20:]
	for len(attrs) >= 4 {
		typ := binary.BigEndian.Uint16(attrs[0:])
		length := int(binary.BigEndian.Uint16(attrs[2:]))
		if 4+length > len(attrs) {
			break
		}
		val := attrs[4 : 4+length]
		switch typ {
		case 0x0020: // XOR-MAPPED-ADDRESS
			if ip, err := parseXORMapped(val); err == nil {
				return ip, nil
			}
		case 0x0001: // MAPPED-ADDRESS
			if len(val) >= 8 && val[1] == 0x01 {
				return net.IP(val[4:8]).String(), nil
			}
		}
		adv := 4 + length
		if pad := length % 4; pad != 0 {
			adv += 4 - pad // attributes are padded to a 4-byte boundary
		}
		attrs = attrs[adv:]
	}
	return "", fmt.Errorf("no mapped address in STUN response")
}

func parseXORMapped(val []byte) (string, error) {
	if len(val) < 8 || val[1] != 0x01 { // family 0x01 = IPv4
		return "", fmt.Errorf("unsupported XOR-MAPPED-ADDRESS")
	}
	cookie := []byte{0x21, 0x12, 0xA4, 0x42}
	ip := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		ip[i] = val[4+i] ^ cookie[i] // X-Address = address XOR magic cookie
	}
	return ip.String(), nil
}

// localOutboundIP returns the source IP the kernel would use to reach the
// public internet (no packet is actually sent for a UDP "connect").
func localOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}
