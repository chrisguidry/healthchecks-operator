package main

import (
	"testing"
)

// secretWith is a Secret as the API server stores it: each value in
// data is base64, which encoding/json writes for a []byte.
func secretWith(namespace, name string, data map[string][]byte) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"data":     data,
	}
}

func TestSecretValueDecodesTheKey(t *testing.T) {
	api := startFakeKube(t)
	api.create(secretsResource, secretWith("example", "probe-token", map[string][]byte{"header": []byte("Bearer abc")}))

	value, err := api.client.secretValue(t.Context(), "example", "probe-token", "header")

	mustSucceed(t, err)
	mustMatch(t, value, "Bearer abc")
	mustMatch(t, api.requests()[0].Path, "/api/v1/namespaces/example/secrets/probe-token")
}

func TestSecretValueErrorsNameTheSecretAndKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"missing", "header", `reading key "header" of Secret example/missing: not found`},
		{"probe-token", "other", `Secret example/probe-token has no key "other"`},
	}
	for _, one := range cases {
		t.Run(one.want, func(t *testing.T) {
			api := startFakeKube(t)
			api.create(secretsResource, secretWith("example", "probe-token", map[string][]byte{"header": []byte("Bearer abc")}))

			_, err := api.client.secretValue(t.Context(), "example", one.name, one.key)

			mustMatch(t, err.Error(), one.want)
		})
	}
}
