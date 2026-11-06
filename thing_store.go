package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"ofam.dev/ochazuke"
)

// listStore (declared with DefineOwn: the row kinds are the tool's own nouns) is a thing noun (a noun with a Store) over the noun's own list
// verb: the standard get and list read the rows the verb answers, so the
// verb stays what it was (its rows, its parameters, its TUI list) and the
// noun gains the core verbs. The tool only reads, so the store is a facts
// store (create, set, delete and archive are on the noun, unavailable, unless
// the tool registers a verb of that name itself, which then wins). The verb
// runs off the loop: the store is Remote and reads through Ctx.Await, which
// takes the verb's pending work and leaves its Then (the TUI list update)
// alone.
type listStore struct {
	Noun string
	// Sample is a value of the struct the rows carry as Data: its json tags
	// are the props (embedded structs flattened, "-" skipped). nil: the
	// rows' data has no fixed fields, so it is one object prop, data.
	Sample any
	// Rename gives a field another prop name where its own is a verb of the
	// noun (a prop and a verb never share a word): json name to prop name.
	Rename map[string]string
	Docs   map[string]string
	// ListParams are list's parameters beside where, props and limit.
	ListParams map[string]ochazuke.Parameter
	// List is the noun's list verb.
	List func(ochazuke.Ctx) ochazuke.Result
}

// define declares the noun's props (the row's id as the key, its label,
// then the sample's fields) and its store.
func (r listStore) define(rt *ochazuke.Runtime) {
	props := []ochazuke.PropSpec{
		{Name: "id", Key: true, Human: true, ReadOnly: true, Parameter: ochazuke.Parameter{Description: "The row's id"}},
		{Name: "label", Role: ochazuke.RoleLabel, ReadOnly: true, Parameter: ochazuke.Parameter{Description: "The row's label"}},
	}
	fields := []ochazuke.PropSpec{{Name: "data", Parameter: ochazuke.Parameter{Type: ochazuke.PropObject, Description: "The row's fields"}}}
	if r.Sample != nil {
		fields = jsonProps(reflect.TypeOf(r.Sample), r.Docs)
	}
	for _, p := range fields {
		if to, ok := r.Rename[p.Name]; ok {
			p.Name = to
		}
		p.ReadOnly = true
		props = append(props, p)
	}
	list := func(c ochazuke.Ctx, q ochazuke.ListQuery) ([]ochazuke.Thing, error) {
		if len(q.Args) > 0 {
			c.Args = q.Args
		}
		v, err := c.Await(ochazuke.StoreCall{Store: r.Noun, Op: "list"}, func() ochazuke.Result { return r.List(c) }, 0)
		if err != nil {
			return nil, err
		}
		return r.things(v)
	}
	rt.DefineOwn(r.Noun).Props(props...).Store(ochazuke.Store{
		Kind: ochazuke.Collection, Facts: true, Remote: true, ListParams: r.ListParams, List: list,
		Get: func(c ochazuke.Ctx, key string) (ochazuke.Thing, error) {
			things, err := list(c, ochazuke.ListQuery{})
			if err != nil {
				return nil, err
			}
			for _, t := range things {
				if id := fmt.Sprint(t["id"]); id == key || strings.TrimPrefix(id, r.Noun+"/") == key {
					return t, nil
				}
			}
			return nil, fmt.Errorf("unknown %s %q", r.Noun, key)
		},
	})
}

// things reads a list verb's answer as the noun's things: the rows' data
// fields (or the data itself), then each row's id and label. Any answer that
// encodes as rows {id, label, data} reads, whatever its Go type.
func (r listStore) things(v any) ([]ochazuke.Thing, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Data  any    `json:"data"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("%s list answered %T, not rows", r.Noun, v)
	}
	out := make([]ochazuke.Thing, 0, len(rows))
	for _, row := range rows {
		t := ochazuke.Thing{}
		switch {
		case r.Sample == nil:
			t["data"] = row.Data
		case row.Data != nil:
			d, err := ochazuke.ThingOf(row.Data)
			if err != nil {
				return nil, err
			}
			t = d
			for from, to := range r.Rename {
				if v, ok := t[from]; ok {
					t[to] = v
					delete(t, from)
				}
			}
		}
		t["id"], t["label"] = row.ID, row.Label
		out = append(out, t)
	}
	return out, nil
}

// jsonProps are the props a struct's json tags name, in field order:
// strings, booleans, integers and numbers as themselves, lists of strings
// as lists, everything else as an object.
func jsonProps(t reflect.Type, docs map[string]string) []ochazuke.PropSpec {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []ochazuke.PropSpec
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonProps(f.Type, docs)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		p := ochazuke.PropSpec{Name: name}
		p.Type = propTypeOf(f.Type)
		p.Description = docs[name]
		out = append(out, p)
	}
	return out
}

// propTypeOf is the prop type that holds a Go type's JSON form.
func propTypeOf(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(time.Duration(0)) {
		return ochazuke.SettingDuration
	}
	switch t.Kind() {
	case reflect.String:
		return ochazuke.SettingString
	case reflect.Bool:
		return ochazuke.SettingBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return ochazuke.SettingInteger
	case reflect.Float32, reflect.Float64:
		return ochazuke.SettingNumber
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return ochazuke.SettingList
		}
	}
	return ochazuke.PropObject
}

// defineListStores declares each noun's store.
func defineListStores(r *ochazuke.Runtime, stores []listStore) {
	for _, s := range stores {
		s.define(r)
	}
}
