package main

// The writes that fakeKube accepts: a merge patch on an object, and
// server-side apply on its status.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

func (f *fakeKube) servePatch(w http.ResponseWriter, r *http.Request, key fakeKey, status bool, body []byte) {
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		writeKubeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	before, exists := f.objects[key]
	if !exists {
		writeKubeStatus(w, http.StatusNotFound, key.name+" not found")
		return
	}
	contentType := r.Header.Get("Content-Type")
	var answer int
	var message string
	var after map[string]any
	switch {
	case contentType == mergePatchContentType && !status:
		after, answer, message = mergeObjectPatch(before, patch)
	case contentType == applyContentType && status:
		after, answer, message = f.applyStatus(key, before, patch, r.URL.Query().Get("fieldManager"))
	default:
		answer, message = http.StatusUnsupportedMediaType, "the fake API server does not support "+contentType+" here"
	}
	if answer != http.StatusOK {
		writeKubeStatus(w, answer, message)
		return
	}
	writeKubeJSON(w, f.commit(key, before, after))
}

// mergeObjectPatch applies a JSON merge patch (RFC 7386) to an object.
// The main resource ignores status, as it does for a resource with a
// status subresource.
func mergeObjectPatch(before, patch map[string]any) (map[string]any, int, string) {
	delete(patch, "status")
	patchMeta, _ := patch["metadata"].(map[string]any)
	meta := metadataOf(before)
	if version, stated := patchMeta["resourceVersion"]; stated && version != meta["resourceVersion"] {
		return nil, http.StatusConflict, fmt.Sprintf("the object has been modified: resourceVersion %v, want %v", meta["resourceVersion"], version)
	}
	after := clone(before)
	mergeInto(after, patch)
	if _, deleting := meta["deletionTimestamp"]; deleting {
		held, _ := meta["finalizers"].([]any)
		asked, _ := metadataOf(after)["finalizers"].([]any)
		for _, finalizer := range asked {
			if !slices.Contains(held, finalizer) {
				return nil, http.StatusUnprocessableEntity, fmt.Sprintf("no new finalizers can be added while the object is being deleted: %v", finalizer)
			}
		}
	}
	return after, http.StatusOK, ""
}

// applyStatus merges an applied status into the object, and removes
// the status fields that the same field manager applied before and
// does not state now.
func (f *fakeKube) applyStatus(key fakeKey, before, applied map[string]any, manager string) (map[string]any, int, string) {
	if manager == "" {
		return nil, http.StatusBadRequest, "fieldManager is required for apply"
	}
	status, _ := applied["status"].(map[string]any)
	after := clone(before)
	stored, _ := after["status"].(map[string]any)
	if stored == nil {
		stored = map[string]any{}
	}
	fields := leafPaths(nil, status)
	for _, path := range f.owners[key][manager] {
		if !slices.ContainsFunc(fields, func(field []string) bool { return slices.Equal(field, path) }) {
			deletePath(stored, path)
		}
	}
	mergeInto(stored, status)
	after["status"] = stored
	if f.owners[key] == nil {
		f.owners[key] = map[string][][]string{}
	}
	f.owners[key][manager] = fields
	return after, http.StatusOK, ""
}

// mergeInto merges src into dst: maps merge by key, a null removes the
// key, and any other value replaces the one in dst.
func mergeInto(dst, src map[string]any) {
	for name, value := range src {
		inner, isMap := value.(map[string]any)
		existing, hasMap := dst[name].(map[string]any)
		switch {
		case value == nil:
			delete(dst, name)
		case isMap && hasMap:
			mergeInto(existing, inner)
		default:
			dst[name] = value
		}
	}
}

// leafPaths lists the paths to every value in a map that is not
// itself a map. A list is one value, as in a status field with
// listType atomic.
func leafPaths(prefix []string, object map[string]any) [][]string {
	var paths [][]string
	for name, value := range object {
		path := append(slices.Clone(prefix), name)
		if inner, isMap := value.(map[string]any); isMap && len(inner) > 0 {
			paths = append(paths, leafPaths(path, inner)...)
		} else {
			paths = append(paths, path)
		}
	}
	return paths
}

// deletePath removes one field, and each map above it that the removal
// leaves empty.
func deletePath(object map[string]any, path []string) {
	if len(path) > 1 {
		inner, _ := object[path[0]].(map[string]any)
		if inner == nil {
			return
		}
		deletePath(inner, path[1:])
		if len(inner) > 0 {
			return
		}
	}
	delete(object, path[0])
}
