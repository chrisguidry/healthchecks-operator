package main

// The Events of condition transitions. A transition is a new
// condition, a new status, or a new reason; a new message alone posts
// nothing, so a fault whose text changes on every try posts once. An
// Event posts after the status that holds the transition is written,
// so a refused write posts nothing.

// badStatus is the status of each condition that a person may need to
// act on, which posts as a Warning.
var badStatus = map[string]ConditionStatus{
	readyCondition:   ConditionFalse,
	passingCondition: ConditionFalse,
}

// postTransitions posts one Event for each condition of next whose
// status or reason differs from the same condition in before.
func (c *controller) postTransitions(object objectReference, before, next []Condition) {
	for _, condition := range next {
		previous, found := findCondition(before, condition.Type)
		if found && previous.Status == condition.Status && previous.Reason == condition.Reason {
			continue
		}
		if bad, known := badStatus[condition.Type]; known && condition.Status == bad {
			c.events.warning(object, condition.Reason, condition.Message)
			continue
		}
		c.events.normal(object, condition.Reason, condition.Message)
	}
}

// referenceTo names an object for an Event. An object with no
// namespace, a ClusterProject, has its Events in default.
func referenceTo(kind string, meta ObjectMeta) objectReference {
	return objectReference{APIVersion: apiVersion, Kind: kind, Namespace: meta.Namespace, Name: meta.Name, UID: meta.UID}
}
