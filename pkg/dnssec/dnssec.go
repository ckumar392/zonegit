// Package dnssec provides DNSSEC keypair management and RRSIG generation
// for zonegit zones. It ships Ed25519 (RFC 8080, algorithm 15) — the
// smallest, fastest, simplest DNSSEC algorithm. Adding RSA or ECDSA later
// is a matter of plumbing additional Algorithm cases through the same
// surfaces (generation, key file marshalling, RRSIG.Sign).
//
// Key material lives in a keys directory as four files per zone. The zone
// name keeps its trailing dot, so for foo.com. the files are:
//
//	foo.com.ksk.priv   Ed25519 private key, base64    (mode 0600)
//	foo.com.ksk.pub    Ed25519 public  key, base64    (mode 0644)
//	foo.com.zsk.priv   Ed25519 private key, base64    (mode 0600)
//	foo.com.zsk.pub    Ed25519 public  key, base64    (mode 0644)
//
// The on-disk format matches the commit-signing keys in pkg/sign.
package dnssec

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Algorithm 15 — Ed25519, the only algorithm currently supported.
const Algorithm = dns.ED25519

// Keypair holds one DNSSEC key (KSK or ZSK).
type Keypair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// ZoneKeys holds both keys for a zone. By DNSSEC convention KSK signs
// only the DNSKEY RRset; ZSK signs everything else. Splitting the two
// lets operators rotate ZSKs frequently while keeping the KSK pinned in
// the parent zone's DS record.
type ZoneKeys struct {
	KSK Keypair
	ZSK Keypair
}

// Generate returns a freshly minted KSK+ZSK pair.
func Generate() (*ZoneKeys, error) {
	ksk, err := newKeypair()
	if err != nil {
		return nil, fmt.Errorf("ksk: %w", err)
	}
	zsk, err := newKeypair()
	if err != nil {
		return nil, fmt.Errorf("zsk: %w", err)
	}
	return &ZoneKeys{KSK: ksk, ZSK: zsk}, nil
}

func newKeypair() (Keypair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Keypair{}, err
	}
	return Keypair{Public: pub, Private: priv}, nil
}

// WriteToDir persists zk to dir using the per-zone filenames above. dir
// is created if necessary. Existing files are overwritten — callers
// that care must check existence themselves first.
func (zk *ZoneKeys) WriteToDir(dir, zone string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zone = canonZone(zone)
	for _, kv := range []struct {
		filename string
		data     []byte
		mode     os.FileMode
	}{
		{zone + "ksk.priv", []byte(base64.StdEncoding.EncodeToString(zk.KSK.Private) + "\n"), 0o600},
		{zone + "ksk.pub", []byte(base64.StdEncoding.EncodeToString(zk.KSK.Public) + "\n"), 0o644},
		{zone + "zsk.priv", []byte(base64.StdEncoding.EncodeToString(zk.ZSK.Private) + "\n"), 0o600},
		{zone + "zsk.pub", []byte(base64.StdEncoding.EncodeToString(zk.ZSK.Public) + "\n"), 0o644},
	} {
		if err := os.WriteFile(filepath.Join(dir, kv.filename), kv.data, kv.mode); err != nil {
			return fmt.Errorf("write %s: %w", kv.filename, err)
		}
	}
	return nil
}

// LoadFromDir reads zone keys previously written by WriteToDir.
func LoadFromDir(dir, zone string) (*ZoneKeys, error) {
	zone = canonZone(zone)
	loadKey := func(filename string, want int) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, fmt.Errorf("%s: bad base64: %w", filename, err)
		}
		if len(raw) != want {
			return nil, fmt.Errorf("%s: length %d, want %d", filename, len(raw), want)
		}
		return raw, nil
	}
	kskPriv, err := loadKey(zone+"ksk.priv", ed25519.PrivateKeySize)
	if err != nil {
		return nil, err
	}
	kskPub, err := loadKey(zone+"ksk.pub", ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	zskPriv, err := loadKey(zone+"zsk.priv", ed25519.PrivateKeySize)
	if err != nil {
		return nil, err
	}
	zskPub, err := loadKey(zone+"zsk.pub", ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	return &ZoneKeys{
		KSK: Keypair{Public: kskPub, Private: kskPriv},
		ZSK: Keypair{Public: zskPub, Private: zskPriv},
	}, nil
}

// HasKeys reports whether a key bundle exists for zone in dir.
func HasKeys(dir, zone string) bool {
	_, err := os.Stat(filepath.Join(dir, canonZone(zone)+"ksk.priv"))
	return err == nil
}

// DNSKEYs returns the (KSK, ZSK) DNSKEY RRs derived from the keypair,
// ready to be staged via Repo.Set.
func (zk *ZoneKeys) DNSKEYs(zone string, ttl uint32) (ksk, zsk *dns.DNSKEY) {
	zone = canonZone(zone)
	ksk = &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zone, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: ttl},
		Flags:     257, // SEP / Key Signing Key
		Protocol:  3,
		Algorithm: Algorithm,
		PublicKey: base64.StdEncoding.EncodeToString(zk.KSK.Public),
	}
	zsk = &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zone, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: ttl},
		Flags:     256, // Zone Signing Key
		Protocol:  3,
		Algorithm: Algorithm,
		PublicKey: base64.StdEncoding.EncodeToString(zk.ZSK.Public),
	}
	return
}

// SignRRset returns an RRSIG covering rrs, signed by key. Use the KSK
// only for the DNSKEY RRset; the ZSK signs everything else.
//
// inception and expiration are unix timestamps. A typical validity
// window is 7–30 days; callers passing zero get a 30-day default
// starting now.
func SignRRset(rrs []dns.RR, zone string, key Keypair, isKSK bool, inception, expiration uint32) (*dns.RRSIG, error) {
	if len(rrs) == 0 {
		return nil, fmt.Errorf("SignRRset: empty rrset")
	}
	if inception == 0 {
		inception = uint32(time.Now().Unix())
	}
	if expiration == 0 {
		expiration = inception + 30*24*3600
	}
	zone = canonZone(zone)
	hdr := rrs[0].Header()

	// Build the DNSKEY (just so we can compute the key tag — it's a
	// 16-bit checksum over the DNSKEY's wire form).
	keyFlag := uint16(256)
	if isKSK {
		keyFlag = 257
	}
	keyDNSKEY := &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zone, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: hdr.Ttl},
		Flags:     keyFlag,
		Protocol:  3,
		Algorithm: Algorithm,
		PublicKey: base64.StdEncoding.EncodeToString(key.Public),
	}

	sig := &dns.RRSIG{
		Hdr:         dns.RR_Header{Name: hdr.Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: hdr.Ttl},
		TypeCovered: hdr.Rrtype,
		Algorithm:   Algorithm,
		Labels:      uint8(dns.CountLabel(hdr.Name)),
		OrigTtl:     hdr.Ttl,
		Expiration:  expiration,
		Inception:   inception,
		KeyTag:      keyDNSKEY.KeyTag(),
		SignerName:  zone,
	}
	if err := sig.Sign(key.Private, rrs); err != nil {
		return nil, fmt.Errorf("RRSIG.Sign: %w", err)
	}
	return sig, nil
}

// VerifyRRset is a convenience wrapper around (*dns.RRSIG).Verify for
// callers (mainly tests) that want to confirm an RRSIG validates.
func VerifyRRset(sig *dns.RRSIG, key Keypair, rrs []dns.RR) error {
	zone := canonZone(sig.SignerName)
	keyFlag := uint16(256)
	if sig.KeyTag != (&dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zone, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: sig.OrigTtl},
		Flags:     keyFlag,
		Protocol:  3,
		Algorithm: Algorithm,
		PublicKey: base64.StdEncoding.EncodeToString(key.Public),
	}).KeyTag() {
		// Try KSK flags.
		keyFlag = 257
	}
	dnskey := &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: zone, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: sig.OrigTtl},
		Flags:     keyFlag,
		Protocol:  3,
		Algorithm: Algorithm,
		PublicKey: base64.StdEncoding.EncodeToString(key.Public),
	}
	return sig.Verify(dnskey, rrs)
}

// canonZone ensures the trailing dot and lowercases. Mirrors the
// canonZone helpers elsewhere in the codebase to keep ref paths and key
// filenames consistent.
func canonZone(z string) string {
	z = strings.ToLower(z)
	if z == "" {
		return ""
	}
	if !strings.HasSuffix(z, ".") {
		z += "."
	}
	return z
}
