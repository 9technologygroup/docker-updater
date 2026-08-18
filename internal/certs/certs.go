package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const validity = 5 * 365 * 24 * time.Hour

type Result struct {
	CertFile    string
	KeyFile     string
	Fingerprint string
	NotAfter    time.Time
	Hosts       []string
}

func Exists(certFile, keyFile string) bool {
	if _, err := os.Stat(certFile); err != nil {
		return false
	}
	_, err := os.Stat(keyFile)
	return err == nil
}

func Describe(certFile string) (Result, error) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return Result{}, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return Result{}, fmt.Errorf("%s does not contain a PEM certificate", certFile)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Result{}, fmt.Errorf("parse %s: %w", certFile, err)
	}

	hosts := append([]string(nil), cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		hosts = append(hosts, ip.String())
	}
	return Result{
		CertFile:    certFile,
		Fingerprint: fingerprint(cert.Raw),
		NotAfter:    cert.NotAfter,
		Hosts:       hosts,
	}, nil
}

func Generate(certFile, keyFile string, hosts []string, ownerGID int) (Result, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Result{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Result{}, fmt.Errorf("generate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: primaryHost(hosts), Organization: []string{"dup"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, h)
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return Result{}, fmt.Errorf("create certificate: %w", err)
	}

	if err := writePEM(certFile, "CERTIFICATE", der, 0o644, ownerGID); err != nil {
		return Result{}, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Result{}, fmt.Errorf("marshal key: %w", err)
	}
	if err := writePEM(keyFile, "EC PRIVATE KEY", keyDER, 0o640, ownerGID); err != nil {
		return Result{}, err
	}

	return Result{
		CertFile:    certFile,
		KeyFile:     keyFile,
		Fingerprint: fingerprint(der),
		NotAfter:    template.NotAfter,
		Hosts:       hosts,
	}, nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode, ownerGID int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if ownerGID >= 0 && os.Geteuid() == 0 {
		if err := os.Chown(tmp, 0, ownerGID); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("chown %s: %w", path, err)
		}
	}
	return os.Rename(tmp, path)
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	out := make([]byte, 0, len(sum)*3)
	const hexDigits = "0123456789abcdef"
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

func primaryHost(hosts []string) string {
	if len(hosts) > 0 {
		return hosts[0]
	}
	return "dup"
}

func DefaultHosts(listen string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts = append([]string{name}, hosts...)
	}
	if host, _, err := net.SplitHostPort(listen); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		hosts = append(hosts, host)
	}
	return dedupe(hosts)
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
