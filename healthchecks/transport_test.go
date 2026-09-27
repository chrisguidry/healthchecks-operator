package healthchecks_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

// startClosingServer answers the first request on each connection with
// an HTTP/1.1 200 and no Connection: close header, then closes the
// connection when the next request on it arrives. uWSGI's http-socket
// in the official Healthchecks image behaves this way: the client reads
// a response that allows reuse, and a request it sends on that
// connection gets EOF.
func startClosingServer(t *testing.T, body string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go answerOnce(conn, body)
		}
	}()
	return "http://" + listener.Addr().String()
}

func answerOnce(conn net.Conn, body string) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, request.Body)
	_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "+
		strconv.Itoa(len(body))+"\r\n\r\n"+body)
	// The next request on this connection gets no answer.
	_, _ = http.ReadRequest(reader)
}

func TestTwoUpsertsInARowSucceedAgainstAServerThatClosesItsConnections(t *testing.T) {
	url := startClosingServer(t, `{"uuid": "a", "ping_url": "https://example.com/ping/a", "slug": "backup"}`)
	client := healthchecks.NewClient(url, "test-key", healthchecks.NewHTTPClient(time.Second))
	request := healthchecks.UpsertRequest{Slug: "backup", Grace: time.Minute, Period: healthchecks.FixedTimeout(time.Hour)}

	_, first := client.Upsert(context.Background(), request)
	_, second := client.Upsert(context.Background(), request)

	if first != nil || second != nil {
		t.Fatalf("got %v and %v, want no errors", first, second)
	}
}

func TestTwoPingsInARowSucceedAgainstAServerThatClosesItsConnections(t *testing.T) {
	url := startClosingServer(t, "OK")

	first := healthchecks.Ping(context.Background(), nil, url+"/ping/a", healthchecks.PingSuccess, "")
	second := healthchecks.Ping(context.Background(), nil, url+"/ping/a", healthchecks.PingFail, "reason")

	if first != nil || second != nil {
		t.Fatalf("got %v and %v, want no errors", first, second)
	}
}
