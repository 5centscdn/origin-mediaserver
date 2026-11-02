package rtsp

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"strings"
)

// challenge parses a 401 response's WWW-Authenticate header and records
// enough state to build an Authorization header for the retried (and all
// subsequent) requests — most RTSP cameras (Hikvision, Dahua, and most
// ONVIF-compliant devices) require Digest auth; Basic is supported too
// since it costs nothing extra.
func (c *session) challenge(headers map[string]string) error {
	wwwAuth, ok := headers["www-authenticate"]
	if !ok {
		return fmt.Errorf("401 with no WWW-Authenticate header")
	}

	switch {
	case strings.HasPrefix(strings.ToLower(wwwAuth), "digest"):
		c.authScheme = "digest"
		c.digestRealm = authParam(wwwAuth, "realm")
		c.digestNonce = authParam(wwwAuth, "nonce")
		if c.digestRealm == "" || c.digestNonce == "" {
			return fmt.Errorf("digest challenge missing realm/nonce: %q", wwwAuth)
		}
		return nil
	case strings.HasPrefix(strings.ToLower(wwwAuth), "basic"):
		c.authScheme = "basic"
		return nil
	default:
		return fmt.Errorf("unsupported auth scheme: %q", wwwAuth)
	}
}

// authorizationHeader builds the Authorization header value for the given
// request, or "" if no challenge has been received yet (the common case
// for servers that don't require auth at all).
func (c *session) authorizationHeader(method, uri string) string {
	switch c.authScheme {
	case "digest":
		ha1 := md5Hex(c.username + ":" + c.digestRealm + ":" + c.password)
		ha2 := md5Hex(method + ":" + uri)
		response := md5Hex(ha1 + ":" + c.digestNonce + ":" + ha2)
		return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
			c.username, c.digestRealm, c.digestNonce, uri, response)
	case "basic":
		token := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
		return "Basic " + token
	default:
		return ""
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// authParam extracts one quoted parameter (e.g. realm="...") from a
// WWW-Authenticate header value.
func authParam(header, key string) string {
	idx := strings.Index(strings.ToLower(header), strings.ToLower(key)+"=")
	if idx < 0 {
		return ""
	}
	rest := header[idx+len(key)+1:]
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
