// Package healthchecks is a client for one project on a Healthchecks
// instance (https://healthchecks.io): the management API that creates,
// finds, and deletes checks and lists a project's notification channels,
// and the pinging API that reports a check's outcome.
//
// Sources: the Healthchecks Management API
// (https://healthchecks.io/docs/api/), the Pinging API
// (https://healthchecks.io/docs/http_api/), and the server's own
// request and response handling, read on the master branch of
// https://github.com/healthchecks/healthchecks: hc/api/views.py (what a
// request must carry and what a response contains), hc/api/models.py
// (the to_dict methods that build a response), and hc/api/urls.py (the
// routes).
package healthchecks
