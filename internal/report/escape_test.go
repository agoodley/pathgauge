package report

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agoodley/pathgauge/internal/results"
)

// marker is what a hostile result would put in a text field. If it ever
// shows up unescaped in a report, that field is an HTML injection point.
const marker = `<pg-inject x="1">'&`

// fill sets every string under v to the marker, every number to 1 and every
// bool to true, and gives every slice, map and pointer one filled element,
// so each renderer takes its "has data" branches.
func fill(v reflect.Value, depth int) {
	if depth > 8 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(marker)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		if v.Type().Elem().Kind() == reflect.Struct || v.Type().Elem().Kind() == reflect.Float64 {
			p := reflect.New(v.Type().Elem())
			fill(p.Elem(), depth+1)
			v.Set(p)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1_700_000_000, 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fill(v.Field(i), depth+1)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := 0; i < 2; i++ {
			fill(s.Index(i), depth+1)
		}
		v.Set(s)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fill(v.Index(i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fill(k, depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	}
}

func filled[T any]() *T {
	v := new(T)
	fill(reflect.ValueOf(v).Elem(), 0)
	return v
}

// Every text field of every result type, of the report itself and of its
// findings is filled with markup. None of it may reach the page unescaped,
// in either template.
func TestNoResultFieldReachesTheReportUnescaped(t *testing.T) {
	steps := []any{
		filled[results.Info](), filled[results.Latency](), filled[results.TCP](), filled[results.UDP](),
		filled[results.StreamScaling](), filled[results.Bufferbloat](), filled[results.Trace](), filled[results.PMTU](),
		filled[results.Integrity](), filled[results.VoIP](), filled[results.Sweep](), filled[results.SizeSweep](),
		filled[results.CCCompare](), filled[results.ShortFlow](), filled[results.Discrimination](), filled[results.SQMTuner](),
		filled[results.Mix](), filled[results.Capacity](), filled[results.CDP](), filled[results.QoS](),
	}
	r := filled[results.Report]()
	r.Steps = nil
	for i, res := range steps {
		s := filled[results.Step]()
		s.ID, s.Status, s.Result = "s"+string(rune('a'+i)), "ok", res
		r.Steps = append(r.Steps, *s)
	}
	bad := filled[results.Step]()
	bad.Status, bad.Result = "error", nil
	r.Steps = append(r.Steps, *bad)

	for _, tmpl := range []string{"", "executive"} {
		var b bytes.Buffer
		if err := WriteHTML(&b, r, Options{Template: tmpl, Org: marker, Title: marker, Notes: marker}); err != nil {
			t.Fatalf("template %q: %v", tmpl, err)
		}
		out := b.String()
		if tmpl == "" && strings.Contains(out, "No renderer") {
			t.Error("a result type has no renderer, so its fields were not checked")
		}
		for at := 0; ; {
			i := strings.Index(out[at:], "<pg-inject")
			if i < 0 {
				break
			}
			at += i
			from, to := max(0, at-160), min(len(out), at+60)
			t.Errorf("template %q: unescaped result text in the report:\n…%s…", tmpl, out[from:to])
			at += len("<pg-inject")
		}
	}
}
