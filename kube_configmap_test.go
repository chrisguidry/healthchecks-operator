package main

import "testing"

// Only the owner reference that names the Check's uid, and that says it
// is the controller, makes the Check the ConfigMap's controller.
func TestAConfigMapIsControlledOnlyByItsController(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		refs []ownerReference
		want bool
	}{
		{"no owner", nil, false},
		{"the controller", []ownerReference{{Kind: "Check", UID: "uid-1", Controller: &yes}}, true},
		{"an owner that is not the controller", []ownerReference{{Kind: "Check", UID: "uid-1", Controller: &no}}, false},
		{"an owner that does not say", []ownerReference{{Kind: "Check", UID: "uid-1"}}, false},
		{"another object's controller", []ownerReference{{Kind: "Check", UID: "uid-2", Controller: &yes}}, false},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			held := configMap{Metadata: configMapMetadata{OwnerReferences: one.refs}}
			mustMatch(t, held.controlledBy("uid-1"), one.want)
		})
	}
}
