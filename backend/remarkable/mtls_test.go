package remarkable

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRMAPIClientCertificateOptions(t *testing.T) {
	certPath, keyPath := writeClientKeyPair(t)
	tests := []struct {
		name    string
		options Options
		wantErr string
	}{
		{name: "neither option"},
		{name: "only certificate", options: Options{ClientCert: certPath}, wantErr: "client_cert and client_key must be set together"},
		{name: "only key", options: Options{ClientKey: keyPath}, wantErr: "client_cert and client_key must be set together"},
		{name: "valid pair", options: Options{ClientCert: certPath, ClientKey: keyPath}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpCtx, err := newRMAPIHTTPClientCtx(test.options)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if httpCtx.Client == nil {
				t.Fatal("rmapi HTTP client is nil")
			}
			if test.options.ClientCert == "" {
				if httpCtx.Client.Transport != nil {
					t.Fatalf("default client transport = %#v, want nil", httpCtx.Client.Transport)
				}
				return
			}
			transportConfig, ok := httpCtx.Client.Transport.(*http.Transport)
			if !ok || transportConfig.TLSClientConfig == nil {
				t.Fatalf("mTLS transport = %#v", httpCtx.Client.Transport)
			}
			if len(transportConfig.TLSClientConfig.Certificates) != 1 {
				t.Fatalf("client certificates = %d, want 1", len(transportConfig.TLSClientConfig.Certificates))
			}
			if transportConfig.TLSClientConfig.InsecureSkipVerify {
				t.Fatal("server certificate verification is disabled")
			}
		})
	}
}

func TestRMAPIClientCertificateInvalidFiles(t *testing.T) {
	certPath := filepath.Join(t.TempDir(), "private-client.crt")
	keyPath := filepath.Join(t.TempDir(), "private-client.key")
	if err := os.WriteFile(certPath, []byte("invalid certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("invalid key"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := newRMAPIHTTPClientCtx(Options{ClientCert: certPath, ClientKey: keyPath})
	if err == nil || !strings.Contains(err.Error(), "unable to load client certificate/key") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), certPath) || strings.Contains(err.Error(), keyPath) || strings.Contains(err.Error(), "invalid certificate") {
		t.Fatalf("error leaks certificate details: %v", err)
	}
}

func TestRMAPIClientCertificateHandshake(t *testing.T) {
	certPath, keyPath := writeClientKeyPair(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			http.Error(response, "missing client certificate", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()

	httpCtx, err := newRMAPIHTTPClientCtx(Options{ClientCert: certPath, ClientKey: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	transportConfig := httpCtx.Client.Transport.(*http.Transport)
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(server.Certificate())
	transportConfig.TLSClientConfig.RootCAs = serverRoots

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpCtx.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200 OK", response.Status)
	}
}

func writeClientKeyPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "rclone-remarkable-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "client.crt")
	keyPath = filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
