// Package api_test holds the pins between this package and internal/agent.
//
// It is an external test package, and it has to be: internal/agent imports
// internal/api (for api.Store, api.Filters and api.MaxUploadBytes), so a test
// file in package api that imported the agent back would be "import cycle not
// allowed in test". Package api_test is a separate package that both of them
// can be imported into, which is the only place in the module where the two
// declarations can be looked at side by side.
package api_test

import (
	"reflect"
	"testing"

	"github.com/regisoliveira/dispute-router/internal/agent"
	"github.com/regisoliveira/dispute-router/internal/api"
)

// api.ReviewFinding is agent.Finding written out a second time, for the cycle
// above. The rows travel between the two as JSONB in agent_runs.findings, so
// the compiler never sees the shapes together: a field renamed on one side
// decodes as a zero value on the other, silently, and a reviewer is shown a
// finding with no quote in it rather than an error.
//
// Kinds are compared rather than types, so agent's named Check type still
// matches api's string - they are the same JSON. A field retyped to a
// different kind, added, removed, renamed or retagged is what fails here.
func TestReviewFindingMirrorsTheAgentsFinding(t *testing.T) {
	t.Parallel()

	assertSameJSONShape(t, "Finding",
		reflect.TypeFor[agent.Finding](), reflect.TypeFor[api.ReviewFinding]())
}

// api's outcomeRejected is agent.OutcomeRejected spelled again, for the same
// cycle, and it is half of what the decision history calls an override. Rename
// the agent's constant and this one goes on matching a value nothing writes
// any more: the overrides filter returns an empty page, which reads as "nobody
// has overridden the verifier" rather than as a broken query.
func TestTheRejectedOutcomeIsTheAgents(t *testing.T) {
	t.Parallel()

	mine, theirs := api.OutcomeRejected, string(agent.OutcomeRejected)
	if mine != theirs {
		t.Errorf("api's outcomeRejected is %q and agent.OutcomeRejected is %q; "+
			"the overrides filter is matching a verdict the agent no longer writes", mine, theirs)
	}
}

// assertSameJSONShape fails unless two types encode and decode the same JSON:
// the same fields in the same order, under the same names and the same json
// tags, with types of the same kind all the way down.
//
// Reflection rather than a written list of field names, because such a list is
// a fourth copy of the shape and drifts along with the other three.
func assertSameJSONShape(t *testing.T, name string, want, got reflect.Type) {
	t.Helper()
	assertSameKind(t, name, want, got, map[[2]reflect.Type]bool{})
}

func assertSameKind(t *testing.T, path string, want, got reflect.Type, seen map[[2]reflect.Type]bool) {
	t.Helper()

	if want.Kind() != got.Kind() {
		t.Errorf("%s: %s is a %s, %s is a %s", path, want, want.Kind(), got, got.Kind())
		return
	}
	// A type that contains itself would otherwise recurse forever.
	pair := [2]reflect.Type{want, got}
	if seen[pair] {
		return
	}
	seen[pair] = true

	switch want.Kind() {
	case reflect.Struct:
		if want.NumField() != got.NumField() {
			t.Errorf("%s: %s has %d fields, %s has %d",
				path, want, want.NumField(), got, got.NumField())
			return
		}
		for i := range want.NumField() {
			a, b := want.Field(i), got.Field(i)
			if a.Name != b.Name {
				t.Errorf("%s: field %d is %s on %s and %s on %s",
					path, i, a.Name, want, b.Name, got)
				continue
			}
			if a.Tag.Get("json") != b.Tag.Get("json") {
				t.Errorf("%s.%s: json tag is %q on %s and %q on %s",
					path, a.Name, a.Tag.Get("json"), want, b.Tag.Get("json"), got)
			}
			assertSameKind(t, path+"."+a.Name, a.Type, b.Type, seen)
		}
	case reflect.Slice, reflect.Array, reflect.Pointer:
		assertSameKind(t, path+"[]", want.Elem(), got.Elem(), seen)
	case reflect.Map:
		assertSameKind(t, path+"[key]", want.Key(), got.Key(), seen)
		assertSameKind(t, path+"[value]", want.Elem(), got.Elem(), seen)
	}
}
