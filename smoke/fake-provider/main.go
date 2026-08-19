package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log"
	"math/big"
	"net/http"
	"os"
	"time"
)

type chatRequest struct {
	Model string `json:"model"`
}

func main() {
	if err := writeCertificates("/run/smoke"); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletions)
	if err := http.ListenAndServeTLS(":8081", "/run/smoke/server.crt", "/run/smoke/server.key", mux); err != nil {
		log.Fatal(err)
	}
}

func writeCertificates(directory string) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Gonka smoke CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caCertificate, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "fake-provider"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		DNSNames:     []string{"fake-provider"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverCertificate, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	serverKeyBytes := x509.MarshalPKCS1PrivateKey(serverKey)
	if err := os.WriteFile(directory+"/ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertificate}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(directory+"/server.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertificate}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(directory+"/server.key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: serverKeyBytes}), 0o600); err != nil {
		return err
	}
	return nil
}

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer smoke-provider-key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var request chatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":             "smoke-completion",
		"object":         "chat.completion",
		"authorized":     true,
		"received_model": request.Model,
	})
}
