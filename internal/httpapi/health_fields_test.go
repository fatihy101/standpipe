package httpapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The detailed /healthz body versus HealthFields(), its closed field list.
//
// HealthFields() is the single source docs/reference/health.md is generated
// from. These tests keep it honest against the encoder:
//
//   - The code side is the JSON the encoder actually produces from
//     adminHealth, not a list of tags copied into this file. A copy here would
//     just move the drift one level up.
//   - Every key the body can carry must be listed, and every listed key must
//     be emitted.
//   - Which keys can be ABSENT must agree. An absent key is a value in this
//     body: github_last_call_at's absence is the third state of the
//     github_reachable verdict, which a bool alone cannot carry.
//
// Renaming the Go struct breaks the build here. Renaming a JSON tag, adding a
// field or dropping a list entry fails with the field's own name.
// ---------------------------------------------------------------------------

// emittedHealthKeys returns every JSON key adminHealth can put on the wire,
// and the subset that can be absent from it.
//
// Both are derived by MARSHALLING rather than by reading tag strings: the
// question is what an operator sees in the body, and the encoder is the
// authority on that. A zero value drops everything `omitempty` can drop; a
// value with every field set non-zero drops nothing.
func emittedHealthKeys(t *testing.T) (all, optional map[string]bool) {
	t.Helper()

	zero := keysOf(t, adminHealth{})

	full := reflect.New(reflect.TypeOf(adminHealth{})).Elem()
	populate(t, full, "adminHealth")
	all = keysOf(t, full.Interface())

	if len(all) == 0 {
		t.Fatalf("adminHealth marshalled to zero JSON keys; this test would pass vacuously")
	}
	optional = map[string]bool{}
	for k := range all {
		if !zero[k] {
			optional[k] = true
		}
	}
	for k := range zero {
		if !all[k] {
			t.Fatalf("key %q appears in a zero adminHealth but not in a fully populated one; "+
				"the populate helper below is wrong, not the field list", k)
		}
	}
	return all, optional
}

// populate sets every field of v to a non-zero value so that no `omitempty`
// can drop it.
//
// An unhandled kind is a HARD FAILURE rather than a skip. A field this helper
// cannot fill would marshal as its zero value, `omitempty` would drop it, and
// the test would then quietly report it as optional: a new field silently
// classified instead of loudly compared.
func populate(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		setNonZero(t, v.Field(i), path+"."+f.Name)
	}
}

// setNonZero gives one value something `omitempty` will not drop.
func setNonZero(t *testing.T, fv reflect.Value, name string) {
	t.Helper()
	switch fv.Kind() {
	case reflect.String:
		fv.SetString("x")
	case reflect.Bool:
		fv.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fv.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		fv.SetUint(1)
	case reflect.Float32, reflect.Float64:
		fv.SetFloat(1)
	case reflect.Slice:
		elem := reflect.New(fv.Type().Elem()).Elem()
		setNonZero(t, elem, name+"[0]")
		fv.Set(reflect.Append(fv, elem))
	case reflect.Map:
		fv.Set(reflect.MakeMap(fv.Type()))
		key := reflect.New(fv.Type().Key()).Elem()
		setNonZero(t, key, name+"<key>")
		val := reflect.New(fv.Type().Elem()).Elem()
		setNonZero(t, val, name+"<value>")
		fv.SetMapIndex(key, val)
	case reflect.Pointer:
		fv.Set(reflect.New(fv.Type().Elem()))
		setNonZero(t, fv.Elem(), name+"*")
	case reflect.Struct:
		populate(t, fv, name)
	default:
		t.Fatalf("%s is a %s and this test does not know how to give it a non-zero value.\n"+
			"Teach setNonZero() about the kind. Leaving it zero would let `omitempty` drop the\n"+
			"field and this test would then report it as optional instead of comparing it.",
			name, fv.Kind())
	}
}

func keysOf(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling adminHealth: %v", err)
	}
	return jsonKeys(t, b)
}

func jsonKeys(t *testing.T, b []byte) map[string]bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshalling a health body: %v\n%s", err, b)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// healthFieldsByName indexes HealthFields() and checks the list's own shape.
func healthFieldsByName(t *testing.T) map[string]HealthField {
	t.Helper()
	fields := HealthFields()
	if len(fields) == 0 {
		t.Fatal("HealthFields() is empty; every comparison below would pass vacuously")
	}
	out := make(map[string]HealthField, len(fields))
	for _, f := range fields {
		if _, dup := out[f.Name]; dup {
			t.Errorf("HealthFields() lists %q twice", f.Name)
		}
		if strings.TrimSpace(f.Meaning) == "" || strings.TrimSpace(f.Presence) == "" {
			t.Errorf("HealthFields() entry %q has an empty meaning or presence; both are part of what "+
				"the closed list promises an operator", f.Name)
		}
		out[f.Name] = f
	}
	return out
}

// TestHealthFieldsMatchTheEncodedBody is the pin: HealthFields() and
// adminHealth's JSON keys must describe the same set, and must agree about
// which keys can be absent.
func TestHealthFieldsMatchTheEncodedBody(t *testing.T) {
	listed := healthFieldsByName(t)
	emitted, optional := emittedHealthKeys(t)

	var undocumented, unimplemented []string
	for k := range emitted {
		if _, ok := listed[k]; !ok {
			undocumented = append(undocumented, k)
		}
	}
	for k := range listed {
		if !emitted[k] {
			unimplemented = append(unimplemented, k)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unimplemented)

	if len(undocumented) > 0 {
		t.Errorf("adminHealth emits %v and HealthFields() has no entry for them.\n"+
			"The field list is closed: every key the body can carry is documented. Add an entry with\n"+
			"the field's meaning and presence, then run `go generate ./...` to refresh\n"+
			"docs/reference/health.md.", undocumented)
	}
	if len(unimplemented) > 0 {
		t.Errorf("HealthFields() documents %v and adminHealth does not emit them.\n"+
			"Either a JSON tag was renamed or removed, or the field was documented but never\n"+
			"implemented. Operators branch on these names, so removing an entry is a deliberate\n"+
			"change, not a cleanup.", unimplemented)
	}

	// Presence, compared where it is observable: whether the key can be
	// missing from the body.
	for name, f := range listed {
		if !emitted[name] {
			continue // already reported above
		}
		switch {
		case optional[name] && !f.MayBeAbsent:
			t.Errorf("adminHealth can OMIT %q from the body, but HealthFields() does not mark it "+
				"MayBeAbsent (presence %q).\n"+
				"A reader who is not told the key can vanish will treat its absence as a parse error or "+
				"as a zero.", name, f.Presence)
		case !optional[name] && f.MayBeAbsent:
			t.Errorf("HealthFields() says %q can be absent, but adminHealth always emits it.\n"+
				"Either the tag lost its `omitempty`, in which case the documented meaning of the absence\n"+
				"is unreachable, or the entry describes a condition on the VALUE and should not be marked\n"+
				"MayBeAbsent.", name)
		}
	}

	// Anti-vacuity on the presence half: github_last_call_at's absence and
	// metrics_drift's absence each MEAN something. If neither can be absent,
	// the comparison above compared nothing.
	if len(optional) == 0 {
		t.Errorf("no adminHealth field is optional, so the presence half of this test compared " +
			"nothing. github_last_call_at's absence and metrics_drift's absence each carry meaning; if " +
			"both tags lost `omitempty`, those meanings are gone.")
	}
}

// TestHealthFieldsFollowTheBodyOrder keeps the documented order equal to the
// order the encoder writes, so a reader can compare the reference against a
// live body line by line.
func TestHealthFieldsFollowTheBodyOrder(t *testing.T) {
	typ := reflect.TypeOf(adminHealth{})
	var tags []string
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			tags = append(tags, name)
		}
	}
	var listed []string
	for _, f := range HealthFields() {
		listed = append(listed, f.Name)
	}
	if !reflect.DeepEqual(tags, listed) {
		t.Errorf("HealthFields() order differs from adminHealth's field order.\n  struct: %v\n  list:   %v",
			tags, listed)
	}
}

// TestHealthExampleShowsEveryUnconditionalField checks the published example
// body against the closed list.
//
// The example is documented as a HEALTHY MOMENT, so every field that is
// always written must appear in it, and nothing outside the list may. A field
// that can be absent may or may not appear; both of its states are
// meaningful, so neither is asserted.
func TestHealthExampleShowsEveryUnconditionalField(t *testing.T) {
	listed := healthFieldsByName(t)
	raw, err := HealthExampleJSON()
	if err != nil {
		t.Fatalf("HealthExampleJSON: %v", err)
	}
	example := jsonKeys(t, raw)

	var extra, missing []string
	for k := range example {
		if _, ok := listed[k]; !ok {
			extra = append(extra, k)
		}
	}
	for name, f := range listed {
		if !f.MayBeAbsent && !example[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("the example body shows %v, which HealthFields() does not list", extra)
	}
	if len(missing) > 0 {
		t.Errorf("the example body omits %v, which are always written; a healthy moment shows all of them", missing)
	}
	if healthExample.Status != "ok" || !healthExample.SQLiteOK || len(healthExample.MetricsDrift) != 0 {
		t.Errorf("the example is documented as a healthy moment and is not one: %+v", healthExample)
	}
}
