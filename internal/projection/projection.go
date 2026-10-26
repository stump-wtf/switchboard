// Package projection prunes a todo payload down to the fields somebody declared they read.
//
// A webhook todo carries its producer's whole delivery body: a Gitea issue event is about 8 KB, and
// almost all of it is the repository, owner and sender objects a consumer never opens. Every byte of
// a tool response is model context, so whoever declares the fields they actually read should pay for
// those and nothing else. A selector list is the whole mechanism, used in two places: a routing
// rule's action projects what intake stores, and claim, claim_next and get_todo project what they
// return without touching what is stored.
//
// Deliberately NOT jq: routing's jq runs out of process because no jq subset that stays useful for
// routing also bounds memory (ADR-0024, ADR-0038), and a projection has to run in the receiving
// process, on the request path, on somebody else's document. A bounded selector cannot allocate:
// it walks a path through a value that was decoded anyway and copies pointers, so its cost is the
// document's own size and the cap on its output. Governing: ADR-0054.
package projection

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// MaxFields bounds one selector list: a consumer that reads more than this is fetching the
	// document, not a projection of it.
	MaxFields = 24
	// MaxFieldBytes bounds one selector, and with MaxFields caps a tool argument at about 6 KB.
	MaxFieldBytes = 256
	// maxSegments bounds one path's depth, so walking it cannot outlast the request.
	maxSegments = 16
	// MaxProjectedBytes caps what a projection may store or return. A selector that gathers a whole
	// array of sub-objects can rebuild most of the document it was meant to trim; past the cap the
	// projection is reported as not applied rather than silently becoming the thing it replaces.
	MaxProjectedBytes = 256 << 10

	// CauseNotApplied names a projection the caller asked for that produced nothing usable; the
	// detail says why, and the caller keeps the full payload rather than losing the delivery.
	CauseNotApplied = "not_applied"
)

// keyPattern accepts the key names that actually appear in forge payloads. A literal dot inside a
// key cannot be written (the dot is the separator); such a field is not selectable, and that is
// said at save time rather than discovered on live traffic.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_@:$-]{1,64}$`)

// ErrInvalid names a selector list that cannot be accepted; the message names the offending field.
var ErrInvalid = errors.New("invalid payload_fields")

// segment is one step of a path: a key, or "[]" meaning "each element of the array here".
type segment struct {
	key  string
	each bool
}

// parse compiles one selector, or explains why it is not one. A path is dot-separated segments, and
// each segment is a field name, optionally followed by [] meaning "each element of the array here".
func parse(field string) ([]segment, error) {
	if field == "" {
		return nil, fmt.Errorf("%w: empty field", ErrInvalid)
	}
	if len(field) > MaxFieldBytes {
		return nil, fmt.Errorf("%w: %q is longer than %d bytes", ErrInvalid, field, MaxFieldBytes)
	}
	parts := strings.Split(field, ".")
	if len(parts) > maxSegments {
		return nil, fmt.Errorf("%w: %q has more than %d path segments", ErrInvalid, field, maxSegments)
	}
	segs := make([]segment, 0, len(parts))
	for _, p := range parts {
		name, each := strings.CutSuffix(p, "[]")
		switch {
		case name == "" && !each:
			return nil, fmt.Errorf("%w: %q has an empty path segment", ErrInvalid, field)
		case name == "":
			return nil, fmt.Errorf("%w: %q has a [] with no field name before it", ErrInvalid, field)
		case !keyPattern.MatchString(name):
			return nil, fmt.Errorf("%w: %q is not a field name (letters, digits, _ @ : $ - ; a dot separates path segments, [] walks an array)", ErrInvalid, name)
		}
		segs = append(segs, segment{key: name})
		if each {
			segs = append(segs, segment{each: true})
		}
	}
	return segs, nil
}

// Validate reports whether a selector list is usable, before anything is stored or claimed with it.
// Called at every boundary: a rule save, and a claim that asked for a projection.
func Validate(fields []string) error {
	if len(fields) > MaxFields {
		return fmt.Errorf("%w: %d fields; the limit is %d", ErrInvalid, len(fields), MaxFields)
	}
	for _, f := range fields {
		if _, err := parse(f); err != nil {
			return err
		}
	}
	return nil
}

// Apply projects a raw JSON document: it returns the pruned value, whether the projection was
// applied, and why not when it was not. Numbers keep their source text, so a projection never
// rewrites an id into exponent notation.
//
// An empty selector list is not a projection: the caller gets the document back unprojected.
func Apply(raw []byte, fields []string) (any, bool, string) {
	if len(fields) == 0 {
		return nil, false, ""
	}
	if len(raw) == 0 {
		return nil, false, CauseNotApplied + ": the payload is empty"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false, CauseNotApplied + ": the payload is not JSON"
	}
	return ApplyValue(v, fields)
}

// ApplyValue projects an already-decoded JSON value. Same contract as Apply.
func ApplyValue(v any, fields []string) (any, bool, string) {
	if len(fields) == 0 {
		return nil, false, ""
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, false, CauseNotApplied + ": the payload is not a JSON object"
	}
	out := map[string]any{}
	for _, f := range fields {
		segs, err := parse(f)
		if err != nil {
			return nil, false, CauseNotApplied + ": " + err.Error()
		}
		sub, ok := resolve(root, segs)
		if !ok {
			continue
		}
		// A selector always starts with a field name (parse refuses a leading []), so its
		// contribution is an object holding that one key.
		if obj, ok := sub.(map[string]any); ok {
			merge(out, obj)
		}
	}
	if len(out) == 0 {
		// Nothing the caller declared is in this document. Storing or returning an empty object
		// would read as "this todo carries nothing", so the caller keeps the whole payload instead.
		return nil, false, CauseNotApplied + ": none of the declared fields is present in the payload"
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, false, CauseNotApplied + ": the projection does not encode"
	}
	if len(raw) > MaxProjectedBytes {
		return nil, false, fmt.Sprintf("%s: the projection encodes to %d bytes; the limit is %d",
			CauseNotApplied, len(raw), MaxProjectedBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var projected any
	if err := dec.Decode(&projected); err != nil {
		return nil, false, CauseNotApplied + ": the projection does not decode"
	}
	return projected, true, ""
}

// resolve walks one path and returns the value shaped like the original but carrying only what the
// path reaches; ok is false when the path meets a value of the wrong kind or a missing key, which
// leaves the field out of the projection entirely.
func resolve(v any, segs []segment) (any, bool) {
	if len(segs) == 0 {
		return v, true
	}
	s, rest := segs[0], segs[1:]
	if s.each {
		arr, ok := v.([]any)
		if !ok {
			return nil, false
		}
		var kept []any
		for _, el := range arr {
			if sub, ok := resolve(el, rest); ok {
				kept = append(kept, sub)
			}
		}
		if kept == nil {
			return nil, false
		}
		return kept, true
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	child, ok := obj[s.key]
	if !ok {
		return nil, false
	}
	sub, ok := resolve(child, rest)
	if !ok {
		return nil, false
	}
	return map[string]any{s.key: sub}, true
}

// merge folds one field's contribution into the projection; two selectors that share a prefix
// share the objects along it, and a later field replaces an earlier one that wrote the same leaf.
func merge(dst map[string]any, src map[string]any) {
	for k, v := range src {
		if inner, ok := dst[k].(map[string]any); ok {
			if next, ok := v.(map[string]any); ok {
				merge(inner, next)
				continue
			}
		}
		dst[k] = v
	}
}
