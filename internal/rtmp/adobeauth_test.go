package rtmp

import (
	"strings"
	"testing"
	"time"
)

// TestAdobeAuth walks the three-connection exchange the way FMLE/ffmpeg do,
// including the failure cases.
func TestAdobeAuth(t *testing.T) {
	k := newAdobeAuthKey()
	now := time.Unix(1_800_000_000, 0)

	// Step 1: no credentials → "need auth".
	res := k.checkAdobeAuth(adobeAuthParams("live"), "enc", "s3cret", now)
	if res.ok || !strings.Contains(res.description, "code=403 need auth; authmod=adobe") {
		t.Fatalf("step 1 = %+v", res)
	}

	// Step 2: user given → salt/challenge/opaque.
	res = k.checkAdobeAuth(adobeAuthParams("live?authmod=adobe&user=enc"), "enc", "s3cret", now)
	if res.ok || !strings.Contains(res.description, "?reason=needauth&") {
		t.Fatalf("step 2 = %+v", res)
	}
	server := adobeAuthParams(res.description[strings.Index(res.description, "?"):])
	for _, f := range []string{"salt", "challenge", "opaque"} {
		if server[f] == "" {
			t.Fatalf("step 2 missing %s: %q", f, res.description)
		}
	}

	// Step 3, computed exactly as ffmpeg's do_adobe_auth does.
	step3 := func(user, password string, at time.Time) adobeAuthResult {
		const clientChallenge = "0a1b2c3d"
		r := adobeResponse(user, server["salt"], password, server["opaque"], clientChallenge)
		app := "live?authmod=adobe&user=" + user + "&challenge=" + clientChallenge +
			"&response=" + r + "&opaque=" + server["opaque"]
		return k.checkAdobeAuth(adobeAuthParams(app), "enc", "s3cret", at)
	}
	if res := step3("enc", "s3cret", now.Add(time.Second)); !res.ok {
		t.Fatalf("correct password rejected: %+v", res)
	}
	if res := step3("enc", "wrong", now); res.ok || !strings.Contains(res.description, "reason=authfailed") {
		t.Fatalf("wrong password = %+v, want authfailed", res)
	}
	if res := step3("other", "s3cret", now); res.ok {
		t.Fatal("wrong user accepted")
	}
	if res := step3("enc", "s3cret", now.Add(adobeOpaqueTTL+time.Minute)); res.ok {
		t.Fatal("expired opaque accepted")
	}
	if res := (adobeAuthKey(newAdobeAuthKey())).checkAdobeAuth(adobeAuthParams(
		"live?authmod=adobe&user=enc&challenge=x&response=y&opaque="+server["opaque"]), "enc", "s3cret", now); res.ok {
		t.Fatal("opaque from another key accepted")
	}
}

// TestAdobeAuth_PercentEncodedResponse covers FMLE, which URL-encodes the
// base64 response it sends back.
func TestAdobeAuth_PercentEncodedResponse(t *testing.T) {
	k := newAdobeAuthKey()
	now := time.Now()
	opaque := k.opaque("enc", now)
	r := adobeResponse("enc", k.salt("enc"), "s3cret", opaque, "c2")
	enc := strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(r)
	app := "live?authmod=adobe&user=enc&challenge=c2&response=" + enc + "&opaque=" + opaque
	if res := k.checkAdobeAuth(adobeAuthParams(app), "enc", "s3cret", now); !res.ok {
		t.Fatalf("percent-encoded response rejected: %+v", res)
	}
}

// TestAdobeAuth_FMLE replays FMLE 3.2's step 3, which hashes with the
// server challenge instead of the opaque, and checks the formula against a
// response FMLE really sent (password "james").
func TestAdobeAuth_FMLE(t *testing.T) {
	if got := adobeResponse("test", "fbce9573fd38a6ae", "james", "2925f466", "oRIAAA=="); got != "1OK0txwNw3fhP+dVRIFQWw==" {
		t.Fatalf("FMLE formula = %q, want the captured 1OK0txwNw3fhP+dVRIFQWw==", got)
	}

	k := newAdobeAuthKey()
	now := time.Now()
	res := k.checkAdobeAuth(adobeAuthParams("live?authmod=adobe&user=test"), "test", "james", now)
	server := adobeAuthParams(res.description[strings.Index(res.description, "?"):])
	r := adobeResponse("test", server["salt"], "james", server["challenge"], "oRIAAA==")
	app := "live?authmod=adobe&user=test&challenge=oRIAAA==&response=" + r + "&opaque=" + server["opaque"]
	if res := k.checkAdobeAuth(adobeAuthParams(app), "test", "james", now); !res.ok {
		t.Fatalf("FMLE-style response rejected: %+v", res)
	}
	r = adobeResponse("test", server["salt"], "wrong", server["challenge"], "oRIAAA==")
	app = "live?authmod=adobe&user=test&challenge=oRIAAA==&response=" + r + "&opaque=" + server["opaque"]
	if res := k.checkAdobeAuth(adobeAuthParams(app), "test", "james", now); res.ok {
		t.Fatal("FMLE-style response with wrong password accepted")
	}
}

func TestAdobeAuthParams_KeepsBase64(t *testing.T) {
	p := adobeAuthParams("live?authmod=adobe&user=u&response=ab+c/d==&opaque=1")
	if p["response"] != "ab+c/d==" || p["user"] != "u" || p["opaque"] != "1" {
		t.Fatalf("params = %v", p)
	}
}
