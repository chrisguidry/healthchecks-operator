package main

import (
	"context"
	"fmt"
)

// secretsResource is the core Secret kind. The operator gets one Secret
// by name when it needs a value, and never lists or watches Secrets, so
// its RBAC is get only and it keeps no Secret in memory.
var secretsResource = kubeResource{Version: "v1", Resource: "secrets"}

// secretValue reads one key of one Secret. Every error names the Secret
// and the key, because the error becomes a probe's failure reason or a
// condition message, and the reader must know which reference to fix.
func (c *kubeClient) secretValue(ctx context.Context, namespace, name, key string) (string, error) {
	// The API server sends data values in base64, and encoding/json
	// decodes base64 into a []byte field.
	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if err := c.get(ctx, secretsResource, namespace, name, &secret); err != nil {
		return "", fmt.Errorf("reading key %q of Secret %s/%s: %w", key, namespace, name, err)
	}
	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("Secret %s/%s has no key %q", namespace, name, key)
	}
	return string(value), nil
}
