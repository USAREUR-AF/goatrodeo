package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func demoReq(headers map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/demo", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// With no door in front (the default), the identity headers are untrusted:
// a forged X-Forwarded-Groups must not open the demo.
func TestDemoGate_FailsClosedWithoutTrust(t *testing.T) {
	g := newDemoGate(false, false, defaultDemoAdminGroups)
	forged := demoReq(map[string]string{
		"X-Forwarded-Groups": "PLATFORM-ADMIN",
		"X-Forwarded-Email":  "someone@example.test",
	})
	if g.mayRunDemo(forged) {
		t.Fatal("forged PLATFORM-ADMIN admitted with trust_forwarded=false")
	}
	v := g.viewerOf(forged)
	if v.Authenticated || v.User != "" {
		t.Fatalf("identity read without trust: %+v", v)
	}
}

// Behind the door, a member of any configured admin group may run the demo;
// everyone else, including an authenticated non-admin, may not.
func TestDemoGate_AdminGroupBehindDoor(t *testing.T) {
	g := newDemoGate(true, false, defaultDemoAdminGroups)

	admin := demoReq(map[string]string{
		"X-Forwarded-Groups": "DEVELOPER,platform-admin", // case-insensitive, any position
		"X-Forwarded-Email":  "ops@example.test",
	})
	if !g.mayRunDemo(admin) {
		t.Fatal("PLATFORM-ADMIN refused behind the door")
	}
	if v := g.viewerOf(admin); !v.Authenticated || v.User != "ops@example.test" {
		t.Fatalf("admin viewer: %+v", v)
	}

	legacy := demoReq(map[string]string{"X-Forwarded-Groups": "ADMINS", "X-Forwarded-User": "uuid-1"})
	if !g.mayRunDemo(legacy) {
		t.Fatal("legacy ADMINS spelling refused (still dual-accepted)")
	}

	player := demoReq(map[string]string{
		"X-Forwarded-Groups": "DEVELOPER,OBSERVER",
		"X-Forwarded-Email":  "player@example.test",
	})
	if g.mayRunDemo(player) {
		t.Fatal("non-admin admitted")
	}
	if v := g.viewerOf(player); !v.Authenticated || v.DemoAdmin {
		t.Fatalf("player viewer: %+v", v)
	}

	// X-Auth-Request-* is the forgeable family and is never consulted.
	xauth := demoReq(map[string]string{"X-Auth-Request-Groups": "PLATFORM-ADMIN"})
	if g.mayRunDemo(xauth) {
		t.Fatal("X-Auth-Request-Groups honoured")
	}
}

// The operator may narrow or widen the group set from the environment.
func TestDemoGate_ConfiguredGroups(t *testing.T) {
	g := newDemoGate(true, false, " site-ops , ")
	if !g.mayRunDemo(demoReq(map[string]string{"X-Forwarded-Groups": "SITE-OPS"})) {
		t.Fatal("configured group refused")
	}
	if g.mayRunDemo(demoReq(map[string]string{"X-Forwarded-Groups": "PLATFORM-ADMIN"})) {
		t.Fatal("default group admitted after the operator replaced the set")
	}
}

// AD_DEMO_UNGATED=1 (local development only) opens the demo to everyone.
func TestDemoGate_Ungated(t *testing.T) {
	g := newDemoGate(false, true, defaultDemoAdminGroups)
	if !g.mayRunDemo(demoReq(nil)) {
		t.Fatal("ungated gate refused an anonymous request")
	}
}

// /demo answers 403 with a reason for non-admins and serves the page for
// admins; /api/me reports the same decision the page's button reads.
func TestDemoRoutes(t *testing.T) {
	g := newDemoGate(true, false, defaultDemoAdminGroups)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /demo", requireDemoAdmin(g, handleDemoPage("zagros")))
	mux.HandleFunc("GET /api/me", handleMe(g))

	// Non-admin: refused, told why.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, demoReq(map[string]string{"X-Forwarded-Groups": "DEVELOPER"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin /demo: got %d want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "platform admins") {
		t.Fatalf("403 body does not explain: %q", rec.Body.String())
	}

	// Admin: the page, with SITE_ID substituted.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, demoReq(map[string]string{"X-Forwarded-Groups": "PLATFORM-ADMIN"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin /demo: got %d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "goat sub --literal 'airdefenders/zagros/**' --follow") {
		t.Fatal("demo page did not substitute SITE_ID")
	}
	if strings.Contains(rec.Body.String(), "__SITE_ID__") {
		t.Fatal("demo page left a __SITE_ID__ placeholder")
	}

	// /api/me mirrors the decision.
	for _, tc := range []struct {
		groups string
		want   bool
	}{{"PLATFORM-ADMIN", true}, {"DEVELOPER", false}, {"", false}} {
		rec = httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/me", nil)
		if tc.groups != "" {
			r.Header.Set("X-Forwarded-Groups", tc.groups)
			r.Header.Set("X-Forwarded-Email", "x@example.test")
		}
		mux.ServeHTTP(rec, r)
		var v viewer
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("/api/me json: %v", err)
		}
		if v.DemoAdmin != tc.want {
			t.Fatalf("/api/me groups=%q demo_admin=%v want %v", tc.groups, v.DemoAdmin, tc.want)
		}
	}

	// No door at all: 403 names the missing door rather than the group.
	open := newDemoGate(false, false, defaultDemoAdminGroups)
	rec = httptest.NewRecorder()
	requireDemoAdmin(open, handleDemoPage("x"))(rec, demoReq(nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "authenticated door") {
		t.Fatalf("doorless /demo: %d %q", rec.Code, rec.Body.String())
	}
}
