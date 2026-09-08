package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ensureCert 生成（或复用已存在的）自签名 TLS 证书。
// 证书会持久化保存，避免每次启动换新证书导致手机浏览器反复告警。
// 若已有证书缺少本机回环地址(127.0.0.1/::1)的 SAN，说明是旧版生成的证书，
// 会导致访问 https://127.0.0.1 时主机名不匹配 → 浏览器转圈，需要重新生成。
func ensureCert(certFile, keyFile string, ips []string) error {
	if fileExists(certFile) && fileExists(keyFile) {
		if coversLoopback(certFile) {
			return nil
		}
		log.Println("[cert] 旧证书缺少本机 SAN，重新生成", certFile)
	}
	os.MkdirAll(filepath.Dir(certFile), 0700)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"phonecameratoweb"}, CommonName: "phonecameratoweb"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	for _, ip := range ips {
		if parsed := net.ParseIP(ip); parsed != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, parsed)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	certOut, err := os.Create(certFile)
	if err != nil {
		return err
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certOut.Close()

	eeOut, err := os.Create(keyFile)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	pem.Encode(eeOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	eeOut.Close()
	return nil
}

// coversLoopback 检查已存在证书的 SAN 是否包含 127.0.0.1（回环）。
func coversLoopback(certFile string) bool {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}