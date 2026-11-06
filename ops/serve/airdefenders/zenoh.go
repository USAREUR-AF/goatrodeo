// SPDX-License-Identifier: Apache-2.0
//
// zenoh.go — REST-based Zenoh publisher for the airdefenders multiplayer
// server.
//
// Why REST and not a native Zenoh client? The original plan picked
// `github.com/eclipse-zenoh/zenoh-go v0.11.0` (CGo around zenoh-c). That
// module does NOT resolve on the Go proxy ("unknown revision v0.11.0").
// Eclipse's actively-maintained Zenoh 1.x language bindings are zenoh-c /
// zenoh-pico / zenoh-python / zenoh-kotlin — there is no maintained Go
// client. The pre-described fallback (see docs/design/airdefenders-multiplayer.md §"Fallback
// plan if zenoh-go is broken on Tegra arm64 or against zenoh-c 1.9.0") is
// to publish via the router's REST plugin: HTTP PUT to
// `http://<router>:8000/<keyexpr>` with the JSON envelope as body.
//
// This file replaces the prior //go:build zenoh CGo path entirely. No
// build tag, no CGO, pure stdlib. The nethost-hosted distroless static image
// is preserved unconditionally — gating is the runtime env var
// `ZENOH_PUBLISH=1` (read by main.go), not a build tag.
//
// Trade-off: the REST publish path does NOT carry per-vendor mTLS identity.
// The publisher container running on nova-a is implicitly trusted because it
// reaches the router over the shared docker network. Per-vendor X.509 +
// portal topic registration STILL APPLY for the inspector "decoded samples"
// view and the schema viewer / autogen-code surfaces — those walk uploaded
// .fds descriptors and are independent of the wire-publish auth surface.
// See docs/design/airdefenders-multiplayer.md §"Zenoh mapping".
//
// Shape:
//   1. StartZenohPublisher subscribes to the in-process EventBus (buffer 32).
//   2. One goroutine drains the channel, marshals each event into the
//      envelope JSON shape, and PUTs it to the matching key expression on
//      the configured REST endpoint.
//   3. HTTP errors / non-2xx status increment mpZenohPubErrs; per-type
//      success increments mpZenohPubByType. Log lines are rate-limited to
//      one per minute so a downed router does not spam stderr.
//   4. Non-blocking semantics preserved end-to-end: a slow / unreachable
//      router slows only the subscriber goroutine. The EventBus drops at
//      the subscribe-buffer boundary; the game loop never blocks.
//   5. stop() unsubscribes from the EventBus (closes the events channel),
//      waits for the goroutine to drain, and closes idle HTTP connections.
//
// Topic mapping (locked in docs/design/airdefenders-multiplayer.md):
//
//   EventRadarDetection   → airdefenders/<site>/radar/detection/v1
//   EventMissileFired     → airdefenders/<site>/missile/fired/v1
//   EventMissileIntercept → airdefenders/<site>/missile/intercept/v1
//   EventShahedImpact     → airdefenders/<site>/shahed/impact/v1
//
// <site> = cfg.SiteID (env SITE_ID, default "nethost", override "nova" on nova-a).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ZenohConfig — runtime config for the REST publisher.
//
// Cert/key/CA paths from the prior CGo design are gone: the REST plugin's
// HTTP endpoint does not require client certs (the router's mTLS listener
// on :7447 still does for native peers; this is a different surface).
type ZenohConfig struct {
	// RESTEndpoint — base URL of the router's REST plugin, e.g.
	// "http://goat-zenoh-router:8000". No trailing slash required.
	RESTEndpoint string
	// SiteID — slotted into the key expression. "nethost" | "nova" | ...
	SiteID string
	// Instance — surfaced into envelope.source.instance (HOSTNAME env).
	Instance string
}

// keyExprFor returns the Zenoh key-expression for a GameEvent type +
// configured site_id. Returns ("", false) for unknown types so the
// publisher loop drops-with-metric rather than panic.
func keyExprFor(siteID, eventType string) (string, bool) {
	switch eventType {
	case EventRadarDetection:
		return fmt.Sprintf("airdefenders/%s/radar/detection/v1", siteID), true
	case EventMissileFired:
		return fmt.Sprintf("airdefenders/%s/missile/fired/v1", siteID), true
	case EventMissileIntercept:
		return fmt.Sprintf("airdefenders/%s/missile/intercept/v1", siteID), true
	case EventShahedImpact:
		return fmt.Sprintf("airdefenders/%s/shahed/impact/v1", siteID), true
	default:
		return "", false
	}
}

// envelope is the canonical wire shape per §Envelope of the design doc.
type envelope struct {
	Envelope envelopeMeta   `json:"envelope"`
	Payload  map[string]any `json:"payload"`
}

type envelopeMeta struct {
	SiteID        string         `json:"site_id"`
	EventID       string         `json:"event_id"`
	EventType     string         `json:"event_type"`
	TsServer      int64          `json:"ts_server"`      // unix millis
	TsISO         string         `json:"ts_iso"`         // RFC3339Nano
	SchemaVersion string         `json:"schema_version"` // "v1"
	Source        envelopeSource `json:"source"`
	// NTP attestation in the envelope is deliberately deferred to Block 11
	// Step 9. Step 9 introduces a reusable `telemetry-client` Go module
	// that all fabric publishers will import for NTP-attested timestamps.
	// Wiring `chronyc` (or equivalent) into airdefenders here would build
	// the exact "airdefenders-only code" Step 9 was scoped to prevent.
	// Re-enable when Step 9 lands.
}

type envelopeSource struct {
	Service  string `json:"service"`  // "airdefenders"
	Instance string `json:"instance"` // HOSTNAME env
	RoomID   string `json:"room_id"`  // "global" in v1
}

// Publish metrics live in multiplayer.go's package-global counter block so
// the (always-linked) main.go render path reads them uniformly:
// mpZenohPubErrs + mpZenohPubByType (keyed by event_type).

// rateLimitedLogger — emit at most one log line per `interval` per logger
// instance. Used to suppress stderr spam when the router is down for an
// extended period (per-event log on every drop would be O(events)).
type rateLimitedLogger struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

func (r *rateLimitedLogger) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if now.Sub(r.last) < r.interval {
		return
	}
	r.last = now
	log.Printf(format, args...)
}

// StartZenohPublisher subscribes to the EventBus, returns a stop fn that
// tears everything down. Errors only on configuration mistakes (missing
// required fields); transient HTTP failures surface as warnings + metric
// increments because the game-loop path must never block on Zenoh.
func StartZenohPublisher(bus EventBus, cfg ZenohConfig) (stop func(), err error) {
	if bus == nil {
		return nil, fmt.Errorf("zenoh: nil EventBus")
	}
	if cfg.RESTEndpoint == "" {
		return nil, fmt.Errorf("zenoh: ZENOH_REST_ENDPOINT required when ZENOH_PUBLISH=1")
	}
	if cfg.SiteID == "" {
		cfg.SiteID = "nethost"
	}
	// Normalize: drop trailing slash so concatenation produces clean URLs.
	endpoint := strings.TrimRight(cfg.RESTEndpoint, "/")

	// Dedicated transport so we can release idle conns on stop and the
	// publisher's HTTP traffic stays isolated from any other client code
	// that might be added later.
	transport := &http.Transport{
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	log.Printf("zenoh: REST publisher targeting %s (site_id=%s)", endpoint, cfg.SiteID)

	events, unsubscribe := bus.Subscribe(32)
	done := make(chan struct{})

	errLogger := &rateLimitedLogger{interval: time.Minute}

	go func() {
		defer close(done)
		for ev := range events {
			keyExpr, ok := keyExprFor(cfg.SiteID, ev.Type)
			if !ok {
				// Unknown event type — increment the error counter and
				// continue. We do NOT panic; the game keeps running.
				mpZenohPubErrs.Add(1)
				errLogger.logf("zenoh: unknown event type %q; dropping", ev.Type)
				continue
			}
			ts := ev.Timestamp
			if ts.IsZero() {
				ts = time.Now()
			}
			env := envelope{
				Envelope: envelopeMeta{
					SiteID:        cfg.SiteID,
					EventID:       ev.EventID,
					EventType:     ev.Type,
					TsServer:      ts.UnixMilli(),
					TsISO:         ts.UTC().Format(time.RFC3339Nano),
					SchemaVersion: "v1",
					Source: envelopeSource{
						Service:  "airdefenders",
						Instance: cfg.Instance,
						RoomID:   "global",
					},
				},
				Payload: ev.Payload,
			}
			body, err := json.Marshal(env)
			if err != nil {
				mpZenohPubErrs.Add(1)
				errLogger.logf("zenoh: marshal failed for %s: %v", ev.Type, err)
				continue
			}

			// Zenoh REST plugin write contract:
			//   PUT /<keyexpr>  body=<bytes>  Content-Type=<as appropriate>
			// (the plugin treats the URL path as the key expression and
			// the body as the value). Both PUT and POST are accepted by
			// upstream; PUT is the canonical "put a sample" verb.
			url := endpoint + "/" + keyExpr
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
			if err != nil {
				cancel()
				mpZenohPubErrs.Add(1)
				errLogger.logf("zenoh: build request for %s: %v", keyExpr, err)
				continue
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			cancel()
			if err != nil {
				mpZenohPubErrs.Add(1)
				errLogger.logf("zenoh: PUT %s failed: %v", keyExpr, err)
				continue
			}
			// Drain + close so the connection can be reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				mpZenohPubErrs.Add(1)
				errLogger.logf("zenoh: PUT %s returned %d", keyExpr, resp.StatusCode)
				continue
			}
			mpZenohPubByTypeInc(ev.Type)
		}
	}()

	stop = func() {
		unsubscribe() // closes `events`, which makes the goroutine return
		<-done
		transport.CloseIdleConnections()
	}
	return stop, nil
}
