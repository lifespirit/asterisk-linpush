package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type accountFile struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}
type Client struct {
	mu          sync.Mutex
	account     accountFile
	key         *rsa.PrivateKey
	project     string
	token       string
	tokenExpiry time.Time
	http        *http.Client
	endpoint    string
}

func New(credentials []byte, project string) (*Client, error) {
	var a accountFile
	if err := json.Unmarshal(credentials, &a); err != nil {
		return nil, fmt.Errorf("service-account JSON: %w", err)
	}
	if a.Type != "service_account" || a.ClientEmail == "" || a.PrivateKey == "" || a.ProjectID == "" {
		return nil, errors.New("expected Firebase service-account JSON (NOT google-services.json)")
	}
	if project == "" {
		project = a.ProjectID
	}
	if project != a.ProjectID {
		return nil, errors.New("configured project_id differs from service account project_id")
	}
	if a.TokenURI == "" {
		a.TokenURI = "https://oauth2.googleapis.com/token"
	}
	u, err := url.Parse(a.TokenURI)
	if err != nil || u.Scheme != "https" {
		return nil, errors.New("service account token_uri must be https")
	}
	block, _ := pem.Decode([]byte(a.PrivateKey))
	if block == nil {
		return nil, errors.New("invalid service account PEM private_key")
	}
	pk, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
			pk = k
		} else {
			return nil, err
		}
	}
	key, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service account key must be RSA")
	}
	return &Client{account: a, key: key, project: project, http: &http.Client{Timeout: 8 * time.Second}, endpoint: "https://fcm.googleapis.com"}, nil
}

// SetEndpoints is intended for isolated integration tests only.
func (c *Client) SetEndpoints(tokenURI, apiBase string) {
	c.account.TokenURI = tokenURI
	c.endpoint = apiBase
}
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExpiry) > 2*time.Minute {
		return c.token, nil
	}
	now := time.Now().UTC()
	hdr := `{"alg":"RS256","typ":"JWT"}`
	claims, _ := json.Marshal(map[string]any{"iss": c.account.ClientEmail, "scope": "https://www.googleapis.com/auth/firebase.messaging", "aud": c.account.TokenURI, "iat": now.Unix(), "exp": now.Add(55 * time.Minute).Unix()})
	input := b64([]byte(hdr)) + "." + b64(claims)
	hash := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {input + "." + b64(sig)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.account.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("OAuth request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OAuth HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", errors.New("OAuth missing access_token")
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 3600
	}
	c.token = r.AccessToken
	c.tokenExpiry = now.Add(time.Duration(r.ExpiresIn) * time.Second)
	return c.token, nil
}

type Payload struct {
	CallID      string
	FromURI     string
	DisplayName string
	DeviceID    string
	TTL         time.Duration
}
type SendError struct {
	Code      string
	Message   string
	Permanent bool
}

func (e *SendError) Error() string { return e.Code + ": " + e.Message }
func (c *Client) Send(ctx context.Context, token string, p Payload) error {
	auth, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	from := p.DisplayName
	if from == "" {
		from = p.FromURI
	}
	data := map[string]string{"uuid": p.DeviceID, "from-uri": p.FromURI, "display-name": p.DisplayName,
		"sip-from": from, "loc-key": "", "loc-args": from, "send-time": time.Now().UTC().Format("2006-01-02 15:04:05")}
	if p.CallID != "" {
		data["call-id"] = p.CallID
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	body := map[string]any{"message": map[string]any{"token": token, "data": data,
		"android": map[string]any{"priority": "HIGH", "ttl": fmt.Sprintf("%ds", int(ttl.Seconds()))}}}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/projects/"+url.PathEscape(c.project)+"/messages:send", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("FCM: %w", err)
	}
	defer resp.Body.Close()
	result, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(result, &e)
	code := e.Error.Status
	perm := false
	for _, d := range e.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			code = "UNREGISTERED"
			perm = true
		}
	}
	if code == "" {
		code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
	}
	msg := e.Error.Message
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return &SendError{Code: code, Message: msg, Permanent: perm}
}
