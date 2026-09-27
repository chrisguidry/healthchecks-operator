// healthchecks-operator creates checks on a Healthchecks instance from
// ClusterProject and Check resources, probes HTTP endpoints and TLS
// certificates, reports each run of a CronJob, and sends the pings for
// those checks.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// version is the release this binary was built from. The Dockerfile
// sets it with -ldflags "-X main.version=...". A build without the
// flag, such as go test, reports dev.
var version = "dev"

// settings is the operator's configuration, from its environment.
type settings struct {
	// namespace is where the operator reads the Secrets that a
	// ClusterProject names. A cluster-scoped resource has no namespace
	// of its own.
	namespace string
	// heartbeatProject and heartbeatSlug name the check that the
	// operator pings every minute. Both are empty when the operator
	// sends no heartbeat.
	heartbeatProject string
	heartbeatSlug    string
	metricsPort      int
}

// readSettings reads the configuration with getenv, which is
// os.Getenv outside of tests.
func readSettings(getenv func(string) string) (settings, error) {
	config := settings{
		namespace:        getenv("OPERATOR_NAMESPACE"),
		heartbeatProject: getenv("HEARTBEAT_PROJECT"),
		heartbeatSlug:    getenv("HEARTBEAT_SLUG"),
		metricsPort:      9200,
	}
	if config.namespace == "" {
		return config, errors.New("OPERATOR_NAMESPACE is unset; it names the namespace of the ClusterProject Secrets")
	}
	if (config.heartbeatProject == "") != (config.heartbeatSlug == "") {
		return config, errors.New("HEARTBEAT_PROJECT and HEARTBEAT_SLUG must be set together, or both left unset")
	}
	if port := getenv("METRICS_PORT"); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return config, fmt.Errorf("METRICS_PORT is %q; it must be a port number from 1 to 65535", port)
		}
		config.metricsPort = number
	}
	return config, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	config, err := readSettings(os.Getenv)
	if err != nil {
		return err
	}
	client, err := inClusterKubeClient()
	if err != nil {
		return err
	}
	// The kubelet stops a pod with SIGTERM, and the context ends on it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	readings := newMetrics(version)
	address, err := readings.serve(ctx, fmt.Sprintf(":%d", config.metricsPort))
	if err != nil {
		return fmt.Errorf("serving metrics: %w", err)
	}
	fmt.Fprintf(os.Stderr, "healthchecks-operator %s started, metrics on %s\n", version, address)
	return operate(ctx, config, client, readings)
}

// operate starts the reconcile loop and the heartbeat, and returns when
// ctx ends.
func operate(ctx context.Context, config settings, client *kubeClient, readings *metrics) error {
	return newController(config, client, readings, time.Now, os.Stderr).run(ctx)
}
