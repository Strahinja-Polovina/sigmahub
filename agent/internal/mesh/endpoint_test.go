package mesh

import (
	"encoding/binary"
	"testing"
)

// buildSTUNResponse crafts a minimal STUN success response carrying an
// XOR-MAPPED-ADDRESS for the given IPv4 (a.b.c.d) and port.
func buildSTUNResponse(a, b, c, d byte, port uint16) []byte {
	msg := make([]byte, 20)
	binary.BigEndian.PutUint16(msg[0:], 0x0101) // Binding success
	binary.BigEndian.PutUint32(msg[4:], stunMagicCookie)

	// XOR-MAPPED-ADDRESS attribute: type 0x0020, len 8.
	attr := make([]byte, 12)
	binary.BigEndian.PutUint16(attr[0:], 0x0020)
	binary.BigEndian.PutUint16(attr[2:], 8)
	attr[4] = 0x00
	attr[5] = 0x01 // family IPv4
	binary.BigEndian.PutUint16(attr[6:], port^0x2112)
	cookie := []byte{0x21, 0x12, 0xA4, 0x42}
	attr[8] = a ^ cookie[0]
	attr[9] = b ^ cookie[1]
	attr[10] = c ^ cookie[2]
	attr[11] = d ^ cookie[3]

	binary.BigEndian.PutUint16(msg[2:], uint16(len(attr))) // message length
	return append(msg, attr...)
}

func TestParseSTUNAddress(t *testing.T) {
	resp := buildSTUNResponse(203, 0, 113, 5, 51820)
	got, err := parseSTUNAddress(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != "203.0.113.5" {
		t.Fatalf("got %q, want 203.0.113.5", got)
	}
}

func TestParseSTUNAddressRejectsShort(t *testing.T) {
	if _, err := parseSTUNAddress([]byte{0x01, 0x01}); err == nil {
		t.Fatal("short response should error")
	}
	// A header with no attributes has no mapped address.
	if _, err := parseSTUNAddress(make([]byte, 20)); err == nil {
		t.Fatal("no-attribute response should error")
	}
}

func TestRenderConfigEmitsEndpoint(t *testing.T) {
	ep := "203.0.113.9:51820"
	cfg := RenderConfig("priv", "10.8.0.1", []Peer{
		{Name: "peer", ServerID: "srv_1", Pubkey: "pub", MeshIP: "10.8.0.2", Endpoint: &ep},
	})
	if !contains(cfg, "Endpoint = 203.0.113.9:51820") {
		t.Fatalf("config missing endpoint line:\n%s", cfg)
	}
	// A nil-endpoint peer emits no Endpoint line.
	cfg2 := RenderConfig("priv", "10.8.0.1", []Peer{
		{Name: "peer", ServerID: "srv_2", Pubkey: "pub", MeshIP: "10.8.0.3"},
	})
	if contains(cfg2, "Endpoint =") {
		t.Fatalf("nil-endpoint peer should not emit an Endpoint line:\n%s", cfg2)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
