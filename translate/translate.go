// Copyright 2019 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package translate

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/coreos/ignition/v2/config/util"
	"github.com/coreos/vcontext/path"
)

/*
 * This is an automatic translator that replace boilerplate code to copy one
 * struct into a nearly identical struct in another package. To use it first
 * call NewTranslator() to get a translator instance. This can then have
 * additional translation rules (in the form of functions) to translate from
 * types in one struct to the other. Those functions are in the form:
 *     func(typeFromInputStruct) -> typeFromOutputStruct
 * These can be closures that reference the translator as well. This allows for
 * manually translating some fields but resuming automatic translation on the
 * other fields through the Translator.Translate() function.
 */

var (
	translationsType = reflect.TypeOf(TranslationSet{})
)

// Returns if this type can be translated without a custom translator. Children or other
// ancestors might require custom translators however
func (t translator) translatable(t1, t2 reflect.Type) bool {
	k1 := t1.Kind()
	k2 := t2.Kind()
	if k1 != k2 {
		return false
	}
	switch {
	case util.IsPrimitive(k1):
		return true
	case util.IsInvalidInConfig(k1):
		panic(fmt.Sprintf("Encountered invalid kind %s in config. This is a bug, please file a report", k1))
	case k1 == reflect.Ptr || k1 == reflect.Slice:
		return t.translatable(t1.Elem(), t2.Elem()) || t.hasTranslator(t1.Elem(), t2.Elem())
	case k1 == reflect.Struct:
		return t.translatableStruct(t1, t2)
	default:
		panic(fmt.Sprintf("Encountered unknown kind %s in config. This is a bug, please file a report", k1))
	}
}

// precondition: t1, t2 are both of Kind 'struct'
func (t translator) translatableStruct(t1, t2 reflect.Type) bool {
	if t1.NumField() != t2.NumField() || t1.Name() != t2.Name() {
		return false
	}
	for i := 0; i < t1.NumField(); i++ {
		t1f := t1.Field(i)
		t2f, ok := t2.FieldByName(t1f.Name)

		if !ok {
			return false
		}
		if !t.translatable(t1f.Type, t2f.Type) && !t.hasTranslator(t1f.Type, t2f.Type) {
			return false
		}
	}
	return true
}

// checks that t could reasonably be the type of a translator function
func couldBeValidTranslator(t reflect.Type) bool {
	if t.Kind() != reflect.Func {
		return false
	}
	if t.NumIn() != 1 || t.NumOut() != 2 {
		return false
	}
	if util.IsInvalidInConfig(t.In(0).Kind()) ||
		util.IsInvalidInConfig(t.Out(0).Kind()) ||
		t.Out(1) != translationsType {
		return false
	}
	return true
}

// fieldName returns the name uses when (un)marshalling a field. t should be a reflect.Value of a struct,
// index is the field index, and tag is the struct tag used when (un)marshalling (e.g. "json" or "yaml")
func fieldName(t reflect.Value, index int, tag string) string {
	f := t.Type().Field(index)
	if tag == "" {
		return f.Name
	}
	return strings.Split(f.Tag.Get(tag), ",")[0]
}

// translate from one type to another, but deep copy all data
// precondition: vFrom and vTo are the same type as defined by translatable()
// precondition: vTo is addressable and settable
func (t translator) translateSameType(vFrom, vTo reflect.Value, fromPath, toPath path.ContextPath) {
	k := vFrom.Kind()
	switch {
	case util.IsPrimitive(k):
		// Use convert, even if not needed; type alias to primitives are not
		// directly assignable and calling Convert on primitives does no harm
		vTo.Set(vFrom.Convert(vTo.Type()))
		t.translations.AddTranslation(fromPath, toPath)
	case k == reflect.Ptr:
		if vFrom.IsNil() {
			return
		}
		vTo.Set(reflect.New(vTo.Type().Elem()))
		t.translate(vFrom.Elem(), vTo.Elem(), fromPath, toPath)
	case k == reflect.Slice:
		if vFrom.IsNil() {
			return
		}
		vTo.Set(reflect.MakeSlice(vTo.Type(), vFrom.Len(), vFrom.Len()))
		for i := 0; i < vFrom.Len(); i++ {
			t.translate(vFrom.Index(i), vTo.Index(i), fromPath.Append(i), toPath.Append(i))
		}
	case k == reflect.Struct:
		for i := 0; i < vFrom.NumField(); i++ {
			fieldGoName := vFrom.Type().Field(i).Name
			toStructField, ok := vTo.Type().FieldByName(fieldGoName)
			if !ok {
				panic("vTo did not have a matching type. This is a bug; please file a report")
			}
			toFieldIndex := toStructField.Index[0]
			vToField := vTo.FieldByName(fieldGoName)

			from := fromPath.Append(fieldName(vFrom, i, fromPath.Tag))
			to := toPath.Append(fieldName(vTo, toFieldIndex, toPath.Tag))
			if vFrom.Type().Field(i).Anonymous {
				from = fromPath
				to = toPath
			}
			t.translate(vFrom.Field(i), vToField, from, to)
		}
	default:
		panic("Encountered types that are not the same when they should be. This is a bug, please file a report")
	}
}

// helper to return if a custom translator was defined
func (t translator) hasTranslator(tFrom, tTo reflect.Type) bool {
	return t.getTranslator(tFrom, tTo).IsValid()
}

// vTo must be addressable, should be acquired by calling reflect.ValueOf() on a variable of the correct type
func (t translator) translate(vFrom, vTo reflect.Value, fromPath, toPath path.ContextPath) {
	tFrom := vFrom.Type()
	tTo := vTo.Type()
	if fnv := t.getTranslator(tFrom, tTo); fnv.IsValid() {
		returns := fnv.Call([]reflect.Value{vFrom})
		vTo.Set(returns[0])

		// handle all the translations and "rebase" them to our current place
		retSet := returns[1].Interface().(TranslationSet)
		for _, trans := range retSet.Set {
			from := fromPath.Append(trans.From.Path...)
			to := toPath.Append(trans.To.Path...)
			t.translations.AddTranslation(from, to)
		}
		return
	}
	if t.translatable(tFrom, tTo) {
		t.translateSameType(vFrom, vTo, fromPath, toPath)
		return
	}

	panic(fmt.Sprintf("Translator not defined for %v to %v", tFrom, tTo))
}

type Translator interface {
	// Adds a custom translator for cases where the structs are not identical. Must be of type
	// func(fromType) -> (toType, TranslationSet). The translator should return the set of all
	// translations it did.
	AddCustomTranslator(t interface{})
	// Also returns a list of source and dest paths, autocompleted by fromTag and toTag
	Translate(from, to interface{}) TranslationSet
}

// Translation represents how a path changes when translating. If something at $yaml.storage.filesystems.4
// generates content at $json.systemd.units.3 a translation can represent that. This allows validation errors
// in Ignition structs to be tracked back to their source in the yaml.
type Translation struct {
	From path.ContextPath
	To   path.ContextPath
}

// TranslationSet represents all of the translations that occurred. They're stored in a map from a string representation
// of the destination path to the translation struct. The map is purely an optimization to allow fast lookups. Ideally the
// map would just be from the destination path.ContextPath to the source path.ContextPath, but ContextPath contains a slice
// which are not comparable and thus cannot be used as keys in maps.
type TranslationSet struct {
	FromTag string
	ToTag   string
	Set     map[string]Translation
}

func NewTranslationSet(fromTag, toTag string) TranslationSet {
	return TranslationSet{
		FromTag: fromTag,
		ToTag:   toTag,
		Set:     map[string]Translation{},
	}
}

func (ts TranslationSet) String() string {
	str := fmt.Sprintf("from: %v\nto: %v\n", ts.FromTag, ts.ToTag)
	for k, v := range ts.Set {
		str += fmt.Sprintf("%s: %v -> %v\n", k, v.From.String(), v.To.String())
	}
	return str
}

// AddTranslation adds a translation to the set
func (ts TranslationSet) AddTranslation(from, to path.ContextPath) {
	// create copies of the paths so if someone else changes from.Path the added translation does not change.
	from = from.Copy()
	to = to.Copy()
	translation := Translation{
		From: from,
		To:   to,
	}
	toString := translation.To.String()
	ts.Set[toString] = translation
}

// Shortcut for AddTranslation for identity translations
func (ts TranslationSet) AddIdentity(paths ...string) {
	for _, p := range paths {
		from := path.New(ts.FromTag, p)
		to := path.New(ts.ToTag, p)
		ts.AddTranslation(from, to)
	}
}

// Merge adds all the entries to the set. It mutates the Set in place.
func (ts TranslationSet) Merge(from TranslationSet) {
	for _, t := range from.Set {
		ts.AddTranslation(t.From, t.To)
	}
}

// MergeP is like Merge, but first it calls Prefix on the set being merged in.
func (ts TranslationSet) MergeP(prefix interface{}, from TranslationSet) {
	from = from.Prefix(prefix)
	ts.Merge(from)
}

// Prefix returns a TranslationSet with all translation paths prefixed by prefix.
func (ts TranslationSet) Prefix(prefix interface{}) TranslationSet {
	ret := NewTranslationSet(ts.FromTag, ts.ToTag)
	from := path.New(ts.FromTag, prefix)
	to := path.New(ts.ToTag, prefix)
	for _, tr := range ts.Set {
		ret.AddTranslation(from.Append(tr.From.Path...), to.Append(tr.From.Path...))
	}
	return ret
}

// NewTranslator creates a new Translator for translating from types with fromTag struct tags (e.g. "yaml")
// to types with toTag struct tages (e.g. "json"). These tags are used when determining paths when generating
// the TranslationSet returned by Translator.Translate()
func NewTranslator(fromTag, toTag string) Translator {
	return &translator{
		translations: TranslationSet{
			FromTag: fromTag,
			ToTag:   toTag,
			Set:     map[string]Translation{},
		},
	}
}

type translator struct {
	// List of custom translation funcs, must pass couldBeValidTranslator
	// This is only for fields that cannot or should not be trivially translated,
	// All trivially translated fields use the default behavior.
	translators  []reflect.Value
	translations TranslationSet
}

// fn should be of the form func(fromType, translationsMap) -> toType
// fn should mutate translationsMap to add all the translations it did
func (t *translator) AddCustomTranslator(fn interface{}) {
	fnv := reflect.ValueOf(fn)
	if !couldBeValidTranslator(fnv.Type()) {
		panic("Tried to register invalid translator function")
	}
	t.translators = append(t.translators, fnv)
}

func (t translator) getTranslator(from, to reflect.Type) reflect.Value {
	for _, fn := range t.translators {
		if fn.Type().In(0) == from && fn.Type().Out(0) == to {
			return fn
		}
	}
	return reflect.Value{}
}

// Translate translates from into to and returns a set of all the path changes it performed.
func (t translator) Translate(from, to interface{}) TranslationSet {
	fv := reflect.ValueOf(from)
	tv := reflect.ValueOf(to)
	if fv.Kind() != reflect.Ptr || tv.Kind() != reflect.Ptr {
		panic("Translate needs to be called on pointers")
	}
	fv = fv.Elem()
	tv = tv.Elem()
	// Make sure to clear this every time`
	t.translations = TranslationSet{
		FromTag: t.translations.FromTag,
		ToTag:   t.translations.ToTag,
		Set:     map[string]Translation{},
	}
	t.translate(fv, tv, path.New(t.translations.FromTag), path.New(t.translations.ToTag))
	return t.translations
}
