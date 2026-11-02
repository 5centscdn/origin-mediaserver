package rtmp

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Adobe RTMP authentication ("authmod=adobe"): the username/password scheme
// FMS and Wowza use, and the one encoders implement — FMLE prompts for
// credentials when it sees it, OBS's "Use authentication", ffmpeg's
// rtmp://user:pass@host/app. It is a three-connection exchange, each
// carried in the connect command's app/tcUrl query and answered with a
// NetConnection.Connect.Rejected _error:
//
//  1. connect "app"                       → "[ code=403 need auth; authmod=adobe ]"
//  2. connect "app?authmod=adobe&user=U"  → "?reason=needauth&user=U&salt=S&challenge=C&opaque=O"
//  3. connect "app?authmod=adobe&user=U&challenge=C2&response=R&opaque=O"
//     with R = b64(md5(b64(md5(U+S+password)) + X + C2))  → success, or
//     "?reason=authfailed"
//
// Clients disagree on X: ffmpeg and librtmp use the opaque O, FMLE uses the
// server challenge C (confirmed from a capture of FMLE 3.2 against this
// server). Both are accepted.
//
// The server keeps no state between the connections: the salt is an HMAC
// of the user, and the opaque carries its own issue time and HMAC, so step
// 3 can be checked on any connection within adobeOpaqueTTL. The challenge
// is derived from the opaque, since FMLE hashes with the challenge but
// only sends back the opaque. salt, challenge
// and opaque are hex so they survive unescaped in a URL query, which is how
// clients send them back.

const adobeOpaqueTTL = 5 * time.Minute

// adobeAuthKey signs salts and opaques. It is per process: a restart only
// invalidates an exchange that is mid-way through.
type adobeAuthKey []byte

func newAdobeAuthKey() adobeAuthKey {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

func (k adobeAuthKey) mac(parts ...string) string {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(m.Sum(nil))
}

func (k adobeAuthKey) salt(user string) string { return k.mac("salt", user)[:16] }

func (k adobeAuthKey) opaque(user string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 16)
	return ts + k.mac("opaque", user, ts)[:16]
}

// challenge is the server challenge sent alongside opaque.
func (k adobeAuthKey) challenge(opaque string) string { return k.mac("challenge", opaque)[:8] }

func (k adobeAuthKey) opaqueValid(user, opaque string, now time.Time) bool {
	if len(opaque) <= 16 {
		return false
	}
	ts, sig := opaque[:len(opaque)-16], opaque[len(opaque)-16:]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(k.mac("opaque", user, ts)[:16])) != 1 {
		return false
	}
	unix, err := strconv.ParseInt(ts, 16, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(unix, 0))
	return age >= -time.Minute && age <= adobeOpaqueTTL
}

// adobeResponse computes the client's expected response.
func adobeResponse(user, salt, password, opaque, clientChallenge string) string {
	h1 := md5.Sum([]byte(user + salt + password))
	h2 := md5.Sum([]byte(base64.StdEncoding.EncodeToString(h1[:]) + opaque + clientChallenge))
	return base64.StdEncoding.EncodeToString(h2[:])
}

// adobeAuthParams splits the query of a connect app/tcUrl ("live?authmod=
// adobe&user=u&response=…"). Values are taken raw, not URL-decoded:
// clients put the base64 response in unescaped, so '+' must stay '+'.
func adobeAuthParams(s string) map[string]string {
	_, query, ok := strings.Cut(s, "?")
	if !ok {
		return nil
	}
	params := map[string]string{}
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		params[k] = v
	}
	return params
}

// adobeAuthResult is what checkAdobeAuth decided for one connect.
type adobeAuthResult struct {
	ok          bool
	description string // _error description to send when !ok
}

// checkAdobeAuth runs one step of the exchange for a connect whose app or
// tcUrl query is params, against the application's login and password.
func (k adobeAuthKey) checkAdobeAuth(params map[string]string, login, password string, now time.Time) adobeAuthResult {
	const reject = "[ AccessManager.Reject ] : [ authmod=adobe ] : "
	user := params["user"]
	if params["authmod"] != "adobe" || user == "" {
		return adobeAuthResult{description: "[ AccessManager.Reject ] : [ code=403 need auth; authmod=adobe ] : "}
	}
	response := params["response"]
	if response == "" {
		// Step 2. Every user gets a challenge; a wrong user only fails at
		// step 3, so the reply doesn't reveal which logins exist.
		opaque := k.opaque(user, now)
		return adobeAuthResult{description: fmt.Sprintf("%s?reason=needauth&user=%s&salt=%s&challenge=%s&opaque=%s",
			reject, user, k.salt(user), k.challenge(opaque), opaque)}
	}
	opaque := params["opaque"]
	salt, clientChallenge := k.salt(user), params["challenge"]
	wantOpaque := adobeResponse(login, salt, password, opaque, clientChallenge)
	wantChallenge := adobeResponse(login, salt, password, k.challenge(opaque), clientChallenge)
	// FMLE percent-encodes the base64 response ("%2B", "%3D"); ffmpeg
	// sends it raw. PathUnescape leaves a literal '+' alone.
	if dec, err := url.PathUnescape(response); err == nil {
		response = dec
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(login)) != 1 ||
		!k.opaqueValid(user, opaque, now) ||
		subtle.ConstantTimeCompare([]byte(response), []byte(wantOpaque))|
			subtle.ConstantTimeCompare([]byte(response), []byte(wantChallenge)) != 1 {
		return adobeAuthResult{description: reject + "?reason=authfailed&opaque=" + opaque}
	}
	return adobeAuthResult{ok: true}
}
