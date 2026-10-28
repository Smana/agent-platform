// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

// roomRowOnlyFields are the roomRow fields the text view deliberately does not show: the JSON has
// them, the table does not.
var roomRowOnlyFields = []string{"Owner", "Driver", "DataClass", "You", "Status"}

// A field added to roomRow or to roomview.Row without its twin fails here: every roomRow field is
// carried by name or listed as JSON-only, and every Row field is filled by the conversion.
func TestViewRowCarriesEveryField(t *testing.T) {
	var r roomRow
	fill(reflect.ValueOf(&r).Elem())
	row := viewRow(r)
	rv, vv := reflect.ValueOf(r), reflect.ValueOf(row)
	for i := range rv.NumField() {
		name := rv.Type().Field(i).Name
		to := vv.FieldByName(name)
		switch {
		case slices.Contains(roomRowOnlyFields, name):
			if to.IsValid() {
				t.Errorf("roomRow.%s is listed as JSON-only but roomview.Row has it", name)
			}
		case !to.IsValid():
			t.Errorf("roomRow.%s has no roomview.Row field: carry it, or add it to roomRowOnlyFields", name)
		case !reflect.DeepEqual(to.Interface(), rv.Field(i).Interface()):
			t.Errorf("roomview.Row.%s = %v, roomRow has %v", name, to.Interface(), rv.Field(i).Interface())
		}
	}
	for i := range vv.NumField() {
		if name := vv.Type().Field(i).Name; vv.Field(i).IsZero() {
			t.Errorf("roomview.Row.%s is not set by the conversion", name)
		} else if _, ok := rv.Type().FieldByName(name); !ok {
			t.Errorf("roomview.Row.%s has no roomRow field", name)
		}
	}
}

// fill sets every field of v, recursively for pointers, to a non-zero value.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64:
		v.SetInt(3)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1700000000, 0)))
			return
		}
		for i := range v.NumField() {
			fill(v.Field(i))
		}
	}
}
