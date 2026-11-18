// Package drm gets content keys for DRM-protected live outputs from
// castLabs DRMtoday, the same integration Nimble Streamer offers
// (drm.conf type = drmtoday: API user/password, merchant UUID, CPIX
// config ID and an asset/content ID per stream).
//
// Flow (DRMtoday CPIX ingest API):
//  1. CAS login: POST {auth}/cas/v1/tickets (username, password) → 201
//     with the ticket-granting ticket (TGT) URL in Location. The TGT is
//     cached per account and reused until DRMtoday rejects it.
//  2. Service ticket: POST {TGT} (service=<CPIX URL>) → a single-use ST.
//  3. GET {fe}/frontend/cpix/v1/{merchant}/ingest/{cpixConfig}/{asset}
//     ?ticket=ST&x-signaling-alg=cbcs → a CPIX 2.3 document with the keys
//     (plain, over TLS), PSSH boxes, DASH ContentProtection data and HLS
//     key tags for Widevine, PlayReady and FairPlay.
//
// DRMtoday returns the same keys every time for an asset, so keys are only
// cached in memory (never written to disk or SQLite) and refetched after a
// restart, and every origin in a cluster encrypts identically.
//
// Scope: cbcs only (one encryption for HLS fMP4, LL-HLS, DASH and nDVR;
// Widevine, PlayReady 4+ and FairPlay all accept it), no key rotation, no
// encrypted (DocumentKey) CPIX. The encryption itself is in
// internal/mux (cbcs.go); wiring per stream is in internal/streamoutputs.
package drm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Default DRMtoday endpoints.
const (
	ProdAuthURL    = "https://auth.drmtoday.com/cas/v1/tickets"
	ProdFEURL      = "https://fe.drmtoday.com"
	StagingAuthURL = "https://auth.staging.drmtoday.com/cas/v1/tickets"
	StagingFEURL   = "https://fe.staging.drmtoday.com"
)

const maxCPIXBytes = 4 << 20

// Account is one application's DRMtoday settings.
type Account struct {
	Staging      bool
	User         string
	Password     string
	MerchantUUID string
	CPIXConfigID string
	// AuthURL / FEURL override the production/staging defaults ([drm] in
	// config.toml), e.g. for a proxy.
	AuthURL string
	FEURL   string
}

func (a Account) authURL() string {
	if a.AuthURL != "" {
		return a.AuthURL
	}
	if a.Staging {
		return StagingAuthURL
	}
	return ProdAuthURL
}

func (a Account) feURL() string {
	if a.FEURL != "" {
		return strings.TrimRight(a.FEURL, "/")
	}
	if a.Staging {
		return StagingFEURL
	}
	return ProdFEURL
}

// Validate reports a missing field, for the API and before any request.
func (a Account) Validate() error {
	switch {
	case a.User == "":
		return errors.New("drm: DRMtoday API user is required")
	case a.Password == "":
		return errors.New("drm: DRMtoday API password is required")
	case a.MerchantUUID == "":
		return errors.New("drm: DRMtoday merchant UUID is required")
	case a.CPIXConfigID == "":
		return errors.New("drm: DRMtoday CPIX config ID is required")
	}
	return nil
}

// StatusError is a non-success answer from DRMtoday, with its meaning per
// the CPIX API docs (403 ticket/merchant, 404 CPIX config, 412 body).
type StatusError struct {
	Step   string // "login", "ticket", "cpix"
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("drmtoday %s: status %d", e.Step, e.Status)
	switch {
	case e.Step == "login" && (e.Status == 400 || e.Status == 401):
		msg += " (wrong API user or password)"
	case e.Step == "cpix" && e.Status == 403:
		msg += " (invalid ticket or merchant UUID)"
	case e.Step == "cpix" && e.Status == 404:
		msg += " (CPIX ingest config not found)"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Client talks to DRMtoday. It's safe for concurrent use.
type Client struct {
	HTTP *http.Client

	mu   sync.Mutex
	tgts map[string]string // authURL+"\x00"+user → TGT URL
}

// NewClient returns a client whose requests time out after timeout.
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{HTTP: &http.Client{Timeout: timeout}, tgts: map[string]string{}}
}

// CPIXURL is the ingest endpoint for an asset (without the ticket).
func CPIXURL(a Account, assetID string) string {
	return a.feURL() + "/frontend/cpix/v1/" + url.PathEscape(a.MerchantUUID) +
		"/ingest/" + url.PathEscape(a.CPIXConfigID) + "/" + url.PathEscape(assetID)
}

// FetchCPIX returns the raw CPIX document for assetID (cbcs signaling).
//
// The CAS service the ticket is issued for is the CPIX URL without query
// parameters (the endpoint "which includes the assetId"). A rejected TGT
// (expired) is replaced by a fresh login once.
func (c *Client) FetchCPIX(ctx context.Context, a Account, assetID string) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if assetID == "" {
		return nil, errors.New("drm: empty asset ID")
	}
	service := CPIXURL(a, assetID)
	for attempt := 0; ; attempt++ {
		tgt, err := c.tgt(ctx, a, attempt > 0)
		if err != nil {
			return nil, err
		}
		st, err := c.serviceTicket(ctx, tgt, service)
		if err != nil {
			var se *StatusError
			if attempt == 0 && errors.As(err, &se) {
				c.dropTGT(a)
				continue
			}
			return nil, err
		}
		q := url.Values{"ticket": {st}, "x-signaling-alg": {"cbcs"}}
		body, err := c.do(ctx, "cpix", http.MethodGet, service+"?"+q.Encode(), nil, http.StatusOK)
		if err != nil {
			var se *StatusError
			// A 403 can also be a TGT DRMtoday no longer honours; one retry
			// with a fresh login tells it apart from a wrong merchant.
			if attempt == 0 && errors.As(err, &se) && se.Status == http.StatusForbidden {
				c.dropTGT(a)
				continue
			}
			return nil, err
		}
		return body, nil
	}
}

// Keys fetches and parses the asset's keys.
func (c *Client) Keys(ctx context.Context, a Account, assetID string) (*KeySet, error) {
	b, err := c.FetchCPIX(ctx, a, assetID)
	if err != nil {
		return nil, err
	}
	ks, err := ParseCPIX(b)
	if err != nil {
		return nil, err
	}
	ks.AssetID = assetID
	return ks, nil
}

func tgtKey(a Account) string { return a.authURL() + "\x00" + a.User }

func (c *Client) dropTGT(a Account) {
	c.mu.Lock()
	delete(c.tgts, tgtKey(a))
	c.mu.Unlock()
}

func (c *Client) tgt(ctx context.Context, a Account, fresh bool) (string, error) {
	k := tgtKey(a)
	c.mu.Lock()
	t, ok := c.tgts[k]
	c.mu.Unlock()
	if ok && !fresh {
		return t, nil
	}
	form := url.Values{"username": {a.User}, "password": {a.Password}}
	resp, err := c.request(ctx, http.MethodPost, a.authURL(), form)
	if err != nil {
		return "", fmt.Errorf("drmtoday login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", &StatusError{Step: "login", Status: resp.StatusCode, Body: readSnippet(resp.Body)}
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("drmtoday login: no ticket-granting ticket in Location")
	}
	base, _ := url.Parse(a.authURL())
	if u, err := base.Parse(loc); err == nil {
		loc = u.String()
	}
	c.mu.Lock()
	c.tgts[k] = loc
	c.mu.Unlock()
	return loc, nil
}

func (c *Client) serviceTicket(ctx context.Context, tgt, service string) (string, error) {
	b, err := c.do(ctx, "ticket", http.MethodPost, tgt, url.Values{"service": {service}}, http.StatusOK)
	if err != nil {
		return "", err
	}
	st := strings.TrimSpace(string(b))
	if st == "" {
		return "", errors.New("drmtoday ticket: empty service ticket")
	}
	return st, nil
}

func (c *Client) do(ctx context.Context, step, method, u string, form url.Values, want int) ([]byte, error) {
	resp, err := c.request(ctx, method, u, form)
	if err != nil {
		return nil, fmt.Errorf("drmtoday %s: %w", step, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return nil, &StatusError{Step: step, Status: resp.StatusCode, Body: readSnippet(resp.Body)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxCPIXBytes+1))
	if err != nil {
		return nil, fmt.Errorf("drmtoday %s: %w", step, err)
	}
	if len(b) > maxCPIXBytes {
		return nil, fmt.Errorf("drmtoday %s: response too large", step)
	}
	return b, nil
}

// request sends one request. Errors never include the URL: TGT URLs and
// tickets are credentials.
func (c *Client) request(ctx context.Context, method, u string, form url.Values) (*http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, errors.New("bad url")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/xml, text/plain, */*")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if inner := errors.Unwrap(err); inner != nil {
			err = inner
		}
		return nil, err
	}
	return resp, nil
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
