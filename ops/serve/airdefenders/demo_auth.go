// Air Defenders — who is asking, and may they run the fabric demo?
//
// The fabric demo (GET /demo: eight auto-pilot bots in one tab, every event
// they generate onto the Zenoh fabric) is an operator tool, not a game mode.
// It fills the whole co-op room, so a player who opens it by accident locks
// everyone else out until they close the tab. It is therefore gated to
// platform admins, and the DEMO button on the title screens renders only for
// them — one click, the whole demo opens and runs.
//
// WHERE THE IDENTITY COMES FROM. The game never authenticates anyone itself.
// On a goatnet the service sits behind the authenticated door
// (oauth2-proxy-airdefenders, ADR 1073): every request that reaches this
// process has already been through Hydra, and the proxy sets X-Forwarded-User
// / X-Forwarded-Email / X-Forwarded-Groups from the VALIDATED session. The
// proxy STRIPS that header family from client input, which is what makes it
// the one an app may trust; X-Auth-Request-* is not stripped and is
// never read here.
//
// FAIL CLOSED WITHOUT THE DOOR. A deployment with no door (the earlier
// mesh-only shape, or a bare `go run` on a laptop) receives whatever headers
// the client chose to send, so trusting them would let anyone forge
// `X-Forwarded-Groups: PLATFORM-ADMIN`. The headers are only read when
// TRUST_FORWARDED_IDENTITY=1, which the compose template sets exactly when the
// door is on. Without it nobody is an admin, the button never renders and
// /demo answers 403 — the demo is unreachable rather than open.
//
// Local development is the one deliberate exception: AD_DEMO_UNGATED=1 opens
// /demo and the button to everyone. Ansible never sets it; it exists so
// `go run . ` + `goat demo airdefenders --url http://localhost:8095` still
// works on a laptop with no door in front.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// defaultDemoAdminGroups is the group set that may run the demo when the
// operator sets nothing. PLATFORM-ADMIN is the canonical name
// (docs/design/mesh-and-ory-roles.md); ADMINS is its legacy spelling, still
// dual-accepted until the goat-id-console port retires it.
const defaultDemoAdminGroups = "PLATFORM-ADMIN,ADMINS"

// demoGate decides who may run the fabric demo. Built once at startup from
// the environment; every handler that cares asks it.
type demoGate struct {
	// trustForwarded: read the X-Forwarded-* identity family. True only
	// behind the authenticated door.
	trustForwarded bool
	// ungated: everyone may run the demo (local development only).
	ungated bool
	// adminGroups: upper-cased group names, any one of which is enough.
	adminGroups map[string]bool
}

func newDemoGateFromEnv() *demoGate {
	return newDemoGate(
		os.Getenv("TRUST_FORWARDED_IDENTITY") == "1",
		os.Getenv("AD_DEMO_UNGATED") == "1",
		envOr("DEMO_ADMIN_GROUPS", defaultDemoAdminGroups),
	)
}

func newDemoGate(trustForwarded, ungated bool, groupsCSV string) *demoGate {
	g := &demoGate{trustForwarded: trustForwarded, ungated: ungated, adminGroups: map[string]bool{}}
	for _, name := range splitCSV(groupsCSV) {
		g.adminGroups[strings.ToUpper(name)] = true
	}
	return g
}

// splitCSV splits a comma-separated header/env value, trimming blanks and
// dropping empties. oauth2-proxy renders X-Forwarded-Groups as "a,b,c".
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// viewer is what the client learns about itself from GET /api/me. Deliberately
// small: the page needs to know whether to draw the DEMO button, nothing else.
type viewer struct {
	// Authenticated: a validated identity arrived (door on and headers set).
	Authenticated bool `json:"authenticated"`
	// DemoAdmin: this viewer may open /demo.
	DemoAdmin bool `json:"demo_admin"`
	// User: the forwarded identity, for the title screen's "signed in as"
	// line. Empty when unauthenticated.
	User string `json:"user,omitempty"`
}

// viewerOf reads the request's identity under the gate's trust rules.
func (g *demoGate) viewerOf(r *http.Request) viewer {
	v := viewer{}
	if g.ungated {
		v.DemoAdmin = true
	}
	if !g.trustForwarded {
		return v
	}
	user := strings.TrimSpace(r.Header.Get("X-Forwarded-Email"))
	if user == "" {
		user = strings.TrimSpace(r.Header.Get("X-Forwarded-User"))
	}
	if user != "" {
		v.Authenticated = true
		v.User = user
	}
	for _, grp := range splitCSV(r.Header.Get("X-Forwarded-Groups")) {
		if g.adminGroups[strings.ToUpper(grp)] {
			v.DemoAdmin = true
			break
		}
	}
	return v
}

// mayRunDemo is the one question the /demo route asks.
func (g *demoGate) mayRunDemo(r *http.Request) bool {
	return g.viewerOf(r).DemoAdmin
}

// handleMe — GET /api/me. Tells the page who it is talking to so the title
// screen can draw the DEMO button for admins and nothing for everyone else.
// Lives under /api/ so the door treats it as an XHR (401, no login redirect,
// no CSRF cookie churn — F-342).
func handleMe(g *demoGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(g.viewerOf(r))
	}
}

// requireDemoAdmin wraps the /demo page: admins get the page, everyone else
// gets a short plain-text 403 that says why, so a curious player is told
// rather than shown a broken grid.
func requireDemoAdmin(g *demoGate, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.mayRunDemo(r) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			if !g.trustForwarded {
				w.Write([]byte("The fabric demo is only available behind the authenticated door. This deployment has none, so /demo is closed.\n"))
				return
			}
			w.Write([]byte("The fabric demo fills the whole co-op room, so it is reserved for platform admins. Ask an admin to run it, or play at / and /mp.\n"))
			return
		}
		next(w, r)
	}
}
