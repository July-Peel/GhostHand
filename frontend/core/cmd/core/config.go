package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type configuration struct {
	Server    string
	Key       string
	Pin       string
	Name      string
	Group     string
	MachineID string
	Token     string
	DeviceID  string
	UpdateKey string
}

func transport(cfg configuration) (*http.Client, error) {
	u, err := url.Parse(cfg.Server)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("controller URL must be an origin")
	}
	host := u.Hostname()
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("controller URL must use HTTP or HTTPS")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Pin != "" {
		pin, err := hex.DecodeString(cfg.Pin)
		if err != nil || len(pin) != sha256.Size || u.Scheme != "https" {
			return nil, errors.New("invalid HTTPS certificate fingerprint")
		}
		// The administrator-provided leaf fingerprint is the trust anchor.
		tlsConfig.InsecureSkipVerify = true
		tlsConfig.VerifyConnection = func(connection tls.ConnectionState) error {
			if len(connection.PeerCertificates) == 0 {
				return errors.New("TLS certificate missing")
			}
			leaf := connection.PeerCertificates[0]
			actual := sha256.Sum256(leaf.Raw)
			now := time.Now()
			if subtle.ConstantTimeCompare(actual[:], pin) != 1 || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return errors.New("TLS certificate pin mismatch or outside its validity period")
			}
			return leaf.VerifyHostname(host)
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		DialContext: dialer.DialContext, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 1,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("controller returned HTTP %d", e) }

func authorized(req *http.Request, cfg configuration) {
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("X-GhostHand-Device", cfg.DeviceID)
}

func postJSON(ctx context.Context, client *http.Client, cfg configuration, path string, input, output any, auth bool) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Server+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		authorized(req, cfg)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return statusError(resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(output)
}
