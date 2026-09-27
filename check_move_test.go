package main

import (
	"testing"

	"github.com/chrisguidry/healthchecks-operator/healthchecks/healthcheckstest"
)

// addProject adds the ClusterProject "outside" on a Healthchecks of its
// own, and returns that Healthchecks.
func (h *harness) addProject() *healthcheckstest.Server {
	h.t.Helper()
	server := healthcheckstest.NewServer("outside-key")
	h.t.Cleanup(server.Close)
	h.api.create(secretsResource, secretWith(operatorNamespace, "outside-healthchecks", map[string][]byte{"api-key": []byte("outside-key")}))
	h.api.create(clusterProjectsResource, ClusterProject{
		Metadata: ObjectMeta{Name: "outside"},
		Spec: ClusterProjectSpec{
			URL:          server.URL(),
			APIKeySecret: SecretKeyRef{Name: "outside-healthchecks", Key: "api-key"},
		},
	})
	return server
}

func TestAMovedCheckLeavesItsOldProject(t *testing.T) {
	h := startHarness(t)
	outside := h.addProject()
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()

	h.editCheck("example", "website", func(spec map[string]any) {
		spec["projectRef"] = map[string]any{"kind": "ClusterProject", "name": "outside"}
	})
	h.pass()

	_, left := h.hc.Check("example-website")
	mustMatch(t, left, false)
	stored, found := outside.Check("example-website")
	mustMatch(t, found, true)
	check := h.check("example", "website")
	mustMatch(t, check.Status.Project, "outside")
	mustMatch(t, check.Status.UUID, stored.UUID)
	mustMatch(t, h.log.lines()[2], "check example/website: deleted check example-website from ClusterProject internal, because the Check moved to outside")
}

// A move whose old project is gone cannot delete the check there, so
// it creates nothing in the new project either.
func TestAMoveWaitsForItsOldProject(t *testing.T) {
	h := startHarness(t)
	outside := h.addProject()
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	h.api.delete(clusterProjectsResource, "", "internal")

	h.editCheck("example", "website", func(spec map[string]any) {
		spec["projectRef"] = map[string]any{"kind": "ClusterProject", "name": "outside"}
	})
	h.pass()

	_, created := outside.Check("example-website")
	mustMatch(t, created, false)
	check := h.check("example", "website")
	mustMatch(t, check.Status.Project, "internal")
	ready := conditionOf(check, "Ready")
	mustMatch(t, ready.Reason, "ProjectNotFound")
	mustMatch(t, ready.Message, "ClusterProject internal holds the check and does not exist, so the check stays there and is not created in outside")
}

// Deleting a Check whose move waits deletes the check from the project
// that holds it.
func TestADeletedCheckIsDeletedFromTheProjectThatHoldsIt(t *testing.T) {
	h := startHarness(t)
	h.addProject()
	h.api.create(checksResource, httpCheck("example", "website", "https://example.com/"))
	h.pass()
	h.api.update(secretsResource, operatorNamespace, "outside-healthchecks", func(object map[string]any) {
		object["data"] = map[string]any{}
	})
	h.editCheck("example", "website", func(spec map[string]any) {
		spec["projectRef"] = map[string]any{"kind": "ClusterProject", "name": "outside"}
	})
	h.pass()

	h.api.delete(checksResource, "example", "website")
	h.pass()

	_, found := h.hc.Check("example-website")
	mustMatch(t, found, false)
	mustMatch(t, h.api.read(checksResource, "example", "website", &Check{}), false)
}
