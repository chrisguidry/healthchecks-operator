package healthchecks

import (
	"net/http"
	"time"
)

// NewHTTPClient builds the HTTP client for the management API and for
// pings. It opens a new connection for each request.
//
// The official healthchecks/healthchecks image serves through uWSGI's
// http-socket, which answers HTTP/1.1 with no "Connection: close"
// header and then closes the socket. Go's transport puts the connection
// back in its pool, and the next request on it gets EOF or a reset. The
// transport retries only an idempotent request after that, so an
// upsert, a delete, or a ping fails although the server handled the
// request before. curl reports the same socket as "Connection died,
// retrying a fresh connect". Without keep-alives no connection is
// reused.
//
// A timeout of zero means no timeout.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	return &http.Client{Transport: transport, Timeout: timeout}
}

// defaultHTTPClient is the client for a caller that passes none.
var defaultHTTPClient = NewHTTPClient(0)
