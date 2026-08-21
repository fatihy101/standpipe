package cli

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/fatihy101/standpipe/internal/store"
)

// eventObj is one event as store.ExportEvents renders it — the canonical JSON
// object of its event type, decoded back into a map.
//
// The CLI reads events as JSON rather than as store structs because
// store.ExportEvents is the only exported read that covers ALL FIVE event
// types with their per-type field sets. Going through the same function the
// archive dump uses has a second benefit: if a field is missing from
// `standpipe trace`, it is missing from the archive too, and the bug is found once.
type eventObj map[string]any

// str walks a path of map keys and returns the leaf as a string.
func (e eventObj) str(path ...string) string {
	v := e.at(path...)
	s, _ := v.(string)
	return s
}

// num walks a path of map keys and returns the leaf as an int64. ok is false
// for a JSON null or a missing key, which is how the nullable integers
// (run_id, outcome_http_status, suppressed_count) are told from a real zero.
func (e eventObj) num(path ...string) (int64, bool) {
	switch v := e.at(path...).(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		return int64(v), true
	case int64:
		// store.EventByID builds its map straight from the SQL scan rather
		// than through JSON, so its integers arrive as int64 while the same
		// field from scanEvents arrives as json.Number. Both paths feed the
		// same renderer.
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

func (e eventObj) at(path ...string) any {
	var cur any = map[string]any(e)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// eventID, eventType and ts are the envelope fields every type carries.
func (e eventObj) eventID() string   { return e.str("event_id") }
func (e eventObj) eventType() string { return e.str("event_type") }

func (e eventObj) ts() time.Time {
	ms, ok := e.num("ts")
	if !ok {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// parent is the trigger this event hangs off, empty on a `trigger` and on the
// parentless `trigger_rejected`.
func (e eventObj) parent() string { return e.str("trigger_event_id") }

// scanEvents streams the event log from `since` and hands each event to fn.
//
// ## Why this is a scan
//
// store.Store exposes EventByID (one event) and ExportEvents (everything from
// a timestamp). It has no "children of this trigger" query and no aggregate
// over a window, so both `standpipe trace` and `standpipe status` are written as a
// forward scan from a lower bound.
//
// That is a REAL cost and it is stated rather than hidden. `trace` on a
// trigger from the start of the retention window reads every event written
// since. The commands stay correct at any size; they get slower with the log.
// Two store methods would collapse both to an index seek, and the need is
// recorded as an internal/store interface request rather than worked
// around by opening a second database handle and writing SQL here — that would
// put schema knowledge in package main, where nobody maintaining
// internal/store would think to look for it.
//
// fn returning an error stops the scan and closes the reader, which unblocks
// and terminates the producing goroutine.
func scanEvents(ctx context.Context, db *store.DB, since time.Time, fn func(eventObj) error) error {
	pr, pw := io.Pipe()
	go func() {
		_, err := db.ExportEvents(ctx, since, pw)
		_ = pw.CloseWithError(err)
	}()
	defer func() { _ = pr.Close() }()

	dec := json.NewDecoder(pr)
	// UseNumber: run_id is an int64 from GitHub and ts is unix-millis. Decoding
	// them through float64 is lossless at today's magnitudes and stops being so
	// silently, which is not a property to depend on in the command whose job
	// is proving the log is trustworthy.
	dec.UseNumber()

	for {
		var obj eventObj
		err := dec.Decode(&obj)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(obj); err != nil {
			return err
		}
	}
}
