package ami

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"example.org/asterisk-linphone-push/internal/store"
)

var digits = regexp.MustCompile(`^[0-9]{1,16}$`)
var tokenSyntax = regexp.MustCompile(`^[A-Za-z0-9._~:-]+$`)

// ParseContact extracts the token only from an FCM contact belonging to a numeric AOR.
// ContactList normally has an ObjectName like 5@@hash, but Endpoint is a
// fallback for configurations that do not expose the AOR in ObjectName.
func ParseContact(msg map[string]string, projectNumber string) (store.AutoContact, bool) {
	uri := field(msg, "uri")
	if uri == "" || len(uri) > 12000 {
		return store.AutoContact{}, false
	}
	parameters := map[string]string{}
	for _, item := range strings.Split(uri, ";")[1:] {
		p := strings.SplitN(strings.TrimSpace(strings.TrimSuffix(item, ">")), "=", 2)
		if len(p) == 2 {
			v, err := url.PathUnescape(strings.Trim(p[1], `"`))
			if err == nil {
				parameters[strings.ToLower(strings.TrimSpace(p[0]))] = v
			}
		}
	}
	if !strings.EqualFold(parameters["pn-provider"], "fcm") || len(parameters["pn-prid"]) < 20 || len(parameters["pn-prid"]) > 4096 || !tokenSyntax.MatchString(parameters["pn-prid"]) {
		return store.AutoContact{}, false
	}
	pnparam := parameters["pn-param"]
	if projectNumber != "" && pnparam != projectNumber {
		return store.AutoContact{}, false
	}
	name := field(msg, "objectname")
	aor := strings.SplitN(name, "@@", 2)[0]
	if !digits.MatchString(aor) {
		aor = field(msg, "aor")
	}
	if !digits.MatchString(aor) {
		aor = field(msg, "endpoint")
	}
	if !digits.MatchString(aor) {
		return store.AutoContact{}, false
	}
	// If both IDs are numeric but disagree, do not assign a token to a wrong extension.
	if endpoint := field(msg, "endpoint"); digits.MatchString(endpoint) && endpoint != aor {
		return store.AutoContact{}, false
	}
	instance := parameters["+sip.instance"]
	deviceSeed := parameters["pn-prid"]
	if instance != "" && len(instance) < 512 {
		deviceSeed = "instance:" + instance
	}
	h := sha256.Sum256([]byte(deviceSeed))
	id := "auto-" + hex.EncodeToString(h[:16])
	contactSum := sha256.Sum256([]byte(uri))
	return store.AutoContact{Number: aor, ID: id, Token: parameters["pn-prid"], PNParam: pnparam,
		ContactHash: fmt.Sprintf("%x", contactSum[:12])}, true
}

func field(m map[string]string, key string) string {
	return strings.TrimSpace(m[strings.ToLower(key)])
}
