// Package registry verifies a username and password against a Docker Registry
// v2 endpoint over HTTPS, without involving the docker CLI. It is linked by the
// unprivileged half of dup only; the root agent makes no outbound calls.
package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/9technologygroup/docker-updater/internal/version"
)

const (
	maxBody        = 1 << 20
	maxRedirects   = 3
	defaultTimeout = 15 * time.Second
)

var (
	// ErrRejected means the registry answered, and said no. ErrUnreachable
	// means we never got a verdict, which the caller words differently.
	ErrRejected    = errors.New("the registry rejected those credentials")
	ErrUnreachable = errors.New("the registry could not be reached")
)

type Client struct {
	HTTP    *http.Client
	Timeout time.Duration
}

func Verify(ctx context.Context, host, username, password string) error {
	return (&Client{}).Verify(ctx, host, username, password)
}

func (c *Client) Verify(ctx context.Context, host, username, password string) error {
	if host == "" {
		return errors.New("no registry host given")
	}
	if username == "" || password == "" {
		return errors.New("both a username and a password are needed")
	}

	base := "https://" + host + "/v2/"
	resp, err := c.get(ctx, base, host, "", "")
	if err != nil {
		return err
	}
	scheme, params := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	status := resp.StatusCode
	drain(resp)

	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
	default:
		return fmt.Errorf("%w: %s answered %d to GET /v2/", ErrUnreachable, host, status)
	}

	switch strings.ToLower(scheme) {
	case "bearer":
		return c.token(ctx, host, params, username, password)
	case "basic", "":
		return c.basic(ctx, base, host, username, password)
	default:
		return fmt.Errorf("%w: %s asked for an authentication scheme dup does not support (%s)", ErrUnreachable, host, clip(scheme))
	}
}

func (c *Client) token(ctx context.Context, host string, params map[string]string, username, password string) error {
	realm := params["realm"]
	u, err := url.Parse(realm)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: %s pointed at a token endpoint dup will not use (%s)", ErrUnreachable, host, clip(realm))
	}

	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()

	resp, err := c.get(ctx, u.String(), u.Host, username, password)
	if err != nil {
		return err
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrRejected
	default:
		return fmt.Errorf("%w: the token endpoint for %s answered %d", ErrUnreachable, host, resp.StatusCode)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return fmt.Errorf("%w: the token endpoint for %s returned something that is not a token response", ErrUnreachable, host)
	}
	if body.Token == "" && body.AccessToken == "" {
		return fmt.Errorf("%w: the token endpoint for %s issued no token", ErrUnreachable, host)
	}
	return nil
}

func (c *Client) basic(ctx context.Context, rawURL, host, username, password string) error {
	resp, err := c.get(ctx, rawURL, host, username, password)
	if err != nil {
		return err
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrRejected
	default:
		return fmt.Errorf("%w: %s answered %d to GET /v2/", ErrUnreachable, host, resp.StatusCode)
	}
}

func (c *Client) get(ctx context.Context, rawURL, host, username, password string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dup/"+version.Short())
	req.Header.Set("Accept", "application/json")
	if username != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := c.client(host).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, redactURL(err))
	}
	return resp, nil
}

// client refuses a redirect that leaves the host we asked for, so nothing the
// registry returns can walk the credentials onto another origin.
func (c *Client) client(host string) *http.Client {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				Proxy:           http.ProxyFromEnvironment,
			},
		}
	}

	clone := *hc
	if clone.Timeout == 0 {
		clone.Timeout = c.timeout()
	}
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req.URL.Host != host {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Host)
		}
		return nil
	}
	return &clone
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
}

func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil
	}
	scheme, rest, _ := strings.Cut(header, " ")

	params := make(map[string]string)
	for _, part := range splitParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		params[key] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return scheme, params
}

func splitParams(s string) []string {
	var (
		out    []string
		buf    strings.Builder
		quoted bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			buf.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if strings.TrimSpace(buf.String()) != "" {
		out = append(out, buf.String())
	}
	return out
}

// redactURL keeps any userinfo a transport error echoed back out of the message.
func redactURL(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.User = nil
			return fmt.Sprintf("%s %s: %v", ue.Op, u.Redacted(), ue.Err)
		}
	}
	return err.Error()
}

func clip(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
