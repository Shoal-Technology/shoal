package edge

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// EncodePEM returns c as PEM: the certificate chain and the private key. It is
// how a certificate is written into a TLS Secret (tls.crt and tls.key).
func EncodePEM(c *tls.Certificate) (certPEM, keyPEM []byte, err error) {
	if c == nil || len(c.Certificate) == 0 {
		return nil, nil, errors.New("edge: no certificate to encode")
	}
	key, ok := c.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("edge: only ECDSA keys are encoded")
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range c.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
