package main

// A ping Check is a check that the workload pings itself. The operator
// runs no probe and sends no ping. It sets the check's period from the
// spec, and when the spec names a ConfigMap, it writes the ping URL
// there for the workload to read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// pingPeriod is the ping check's own schedule or timeout. A schedule
// gets the same translation as a CronJob's, because the person who
// writes it may copy it from a CronJob.
func pingPeriod(ping PingProbe) (healthchecks.Period, verdict) {
	if ping.Schedule != "" {
		tz := ping.TimeZone
		if tz == "" {
			tz = "UTC"
		}
		return healthchecks.CronSchedule(translateSchedule(ping.Schedule), tz), synced
	}
	timeout, err := time.ParseDuration(ping.Timeout)
	if err != nil {
		return healthchecks.Period{}, verdict{reasonInvalidSpec, "ping.timeout: " + errorText(err)}
	}
	return healthchecks.FixedTimeout(timeout), synced
}

// syncConfigMap writes the ping URL into the ConfigMap the spec names,
// then releases the ConfigMap that status names when the spec names
// another one or none. The new ConfigMap exists before the old one
// goes, so a workload never finds neither, and a write that fails
// leaves the old one as it is.
func (c *controller) syncConfigMap(ctx context.Context, check *Check, state *checkState, w *world, next *CheckStatus) verdict {
	var want *PingConfigMap
	if check.Spec.Ping != nil {
		want = check.Spec.Ping.ConfigMap
	}
	name := ""
	if want != nil {
		if v := c.writeConfigMap(ctx, check, state, w, *want, next.PingURL); !v.ready() {
			return v
		}
		name = want.Name
	}
	if next.ConfigMap != "" && next.ConfigMap != name {
		if v := c.releaseConfigMap(ctx, check, state, w, next.ConfigMap); !v.ready() {
			return v
		}
	}
	next.ConfigMap = name
	return synced
}

// writeConfigMap writes the ping URL into the ConfigMap, unless the
// store holds it there already, so a steady pass makes no request. A
// ConfigMap that another tool created has no label, so it is not in
// the store, and the first pass applies it and takes it over.
func (c *controller) writeConfigMap(ctx context.Context, check *Check, state *checkState, w *world, want PingConfigMap, pingURL string) verdict {
	if pingURL == "" {
		return verdict{reasonConfigMapFailed, "the check has no ping URL, so the operator does not write ConfigMap " + want.Name}
	}
	key := want.EffectiveKey()
	storeKey := state.namespace + "/" + want.Name
	held, found := w.configMaps[storeKey]
	if owner, controlled := held.controller(); found && controlled && owner.UID != check.Metadata.UID {
		// Every Check applies as the same field manager, so a second
		// Check's apply would replace the first one's owner reference,
		// and each write would wake the pass that writes it back.
		return verdict{reasonConfigMapConflict, fmt.Sprintf("ConfigMap %s is controlled by %s %s with uid %s, not this Check, so the operator does not write it", want.Name, owner.Kind, owner.Name, owner.UID)}
	}
	if found && held.Data[key] == pingURL && held.controlledBy(check.Metadata.UID) {
		return synced
	}
	var written json.RawMessage
	err := c.client.apply(ctx, configMapsResource, state.namespace, want.Name, pingConfigMap(check, want.Name, key, pingURL), &written)
	if err != nil {
		return verdict{reasonConfigMapFailed, "writing the ping URL into ConfigMap " + want.Name + ": " + errorText(err)}
	}
	// The watch event for this write can reach the store after the next
	// pass reads it. The store takes the API server's answer now, so that
	// pass finds the URL and does not write again. This pass's world takes
	// it too, so a second Check later in the pass that names the same
	// ConfigMap finds its controller.
	c.watches[configMapsResource].store.put(storeKey, written)
	var answer configMap
	if json.Unmarshal(written, &answer) == nil {
		w.configMaps[storeKey] = answer
	}
	c.log.printf("%s: wrote the ping URL into ConfigMap %s", state.subject(), want.Name)
	return synced
}

// releaseConfigMap lets go of a ConfigMap the Check no longer names. It
// acts only on a ConfigMap that the store holds with this Check as its
// controller. Anything else is not the operator's to remove: another
// object may control it now, or a person took off the label.
//
// The operator writes one key. A ConfigMap with more keys holds data
// that another writer set, and deleting it would delete that data, so
// the operator applies an object that states no fields. Server-side
// apply then removes the fields this manager alone owns: the key, the
// label, and the owner reference. Otherwise the ConfigMap is the
// operator's alone, and it deletes it, on the condition that its uid
// and resourceVersion are the ones the store holds.
func (c *controller) releaseConfigMap(ctx context.Context, check *Check, state *checkState, w *world, name string) verdict {
	held, found := w.configMaps[state.namespace+"/"+name]
	if !found || !held.controlledBy(check.Metadata.UID) {
		return synced
	}
	if held.keys() > 1 {
		err := c.client.apply(ctx, configMapsResource, state.namespace, name, emptyConfigMap(state.namespace, name), nil)
		if err != nil {
			return verdict{reasonConfigMapFailed, "releasing ConfigMap " + name + ": " + errorText(err)}
		}
		c.log.printf("%s: released ConfigMap %s, which holds keys the operator did not write", state.subject(), name)
		return synced
	}
	err := c.client.delete(ctx, configMapsResource, state.namespace, name, held.Metadata.UID, held.Metadata.ResourceVersion)
	switch {
	case errors.Is(err, errNotFound):
		return synced
	case errors.Is(err, errConflict):
		// Another object has its name now, or someone changed it since
		// the store read it. The change's event brings the newer copy,
		// and the next pass decides again.
		return synced
	case err != nil:
		return verdict{reasonConfigMapFailed, "deleting ConfigMap " + name + ": " + errorText(err)}
	}
	c.log.printf("%s: deleted ConfigMap %s", state.subject(), name)
	return synced
}
