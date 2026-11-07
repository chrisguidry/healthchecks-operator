package main

// fakeKube selects objects by label the way the API server reads an
// equality selector such as "team=a,tier=web". A watch sends an object
// that starts to match as ADDED, and one that stops matching as
// DELETED, as the API server's watch cache does.

import "strings"

// parseSelector reads an equality selector into the labels it asks
// for. An empty selector asks for none, and matches every object.
func parseSelector(selector string) map[string]string {
	wanted := map[string]string{}
	for _, term := range strings.Split(selector, ",") {
		name, value, found := strings.Cut(term, "=")
		if found {
			wanted[name] = value
		}
	}
	return wanted
}

// fieldPrefix marks a term of wanted that a field selector asked for,
// such as metadata.name, apart from a label of the same name.
const fieldPrefix = "\x00"

// parseRequestSelector reads a request's label selector and its field
// selector into one set of terms. The fake reads the fields of
// metadata only, as the API server does for most resources.
func parseRequestSelector(labels, fields string) map[string]string {
	wanted := parseSelector(labels)
	for name, value := range parseSelector(fields) {
		wanted[fieldPrefix+name] = value
	}
	return wanted
}

// selected reports whether the object carries every label and field
// that wanted asks for.
func selected(wanted map[string]string, object map[string]any) bool {
	meta := metadataOf(object)
	labels, _ := meta["labels"].(map[string]any)
	for name, value := range wanted {
		if field, ok := strings.CutPrefix(name, fieldPrefix+"metadata."); ok {
			if stringAt(meta, field) != value {
				return false
			}
			continue
		}
		if labels[name] != value {
			return false
		}
	}
	return true
}

// selectedEvent returns the event a watch with the selector sends for
// event, and false when it sends none.
func selectedEvent(wanted map[string]string, event fakeEvent) (fakeEvent, bool) {
	now := selected(wanted, event.object)
	was := event.previous != nil && selected(wanted, event.previous)
	switch {
	case event.kind == "DELETED":
		return event, was
	case now && !was:
		event.kind = "ADDED"
		return event, true
	case now:
		return event, true
	case was:
		event.kind = "DELETED"
		return event, true
	}
	return event, false
}
