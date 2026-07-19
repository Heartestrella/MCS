package main

// tls.go — 面板自签 HTTPS。浏览器只在 Secure Context(HTTPS 或 localhost)开放
// 麦克风 / 屏幕共享 / WebCodecs,语音开黑要在局域网可用必须走 HTTPS。
// 证书首次生成(SAN 含 localhost/127.0.0.1/当前局域网 IP),存 data/tls/,
// 局域网 IP 变化后自动重新签发。手机/其他电脑首次访问需在警告页选「继续访问」。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ensureTLSCert returns cert/key PEM paths, generating or re-issuing when the
// cert is missing, expired, or doesn't cover the current LAN IP.
func ensureTLSCert(dataDir string) (certFile, keyFile string, err error) {
	dir := filepath.Join(dataDir, "tls")
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	lan := lanIP()
	if certOK(certFile, lan) {
		return certFile, keyFile, nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", err
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "MCS Panel", Organization: []string{"MCS"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(3, 0, 0),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	if lan != "" {
		if ip := net.ParseIP(lan); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return "", "", err
	}
	keyDer, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// certOK reports whether the existing cert is valid for 30+ days and covers ip.
func certOK(certFile, ip string) bool {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if time.Now().AddDate(0, 0, 30).After(cert.NotAfter) {
		return false
	}
	if ip == "" {
		return true
	}
	want := net.ParseIP(ip)
	for _, san := range cert.IPAddresses {
		if san.Equal(want) {
			return true
		}
	}
	return false
}

// tlsConfig returns a Config that reloads the cert lazily so an IP-change
// re-issue during runtime is picked up without restart.
func tlsConfig(certFile, keyFile string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}
}
