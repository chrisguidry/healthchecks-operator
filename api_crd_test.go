package main

// These tests run each CRD through the same code the API server runs:
// the structural schema validator for the shape, and the CEL validator
// for the rules the schema states. An example the cluster would refuse
// fails here instead.

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// mustSucceed and mustMatch live in assert_test.go, shared by every test
// file in this package.

func mustDeepEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// The definitions this repository ships.
const (
	clusterProjectsCRD = "deploy/clusterprojects-crd.yaml"
	checksCRD          = "deploy/checks-crd.yaml"
)

func loadCRDFrom(t *testing.T, path string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(raw, crd); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return crd
}

func TestCRDIdentity(t *testing.T) {
	cases := []struct {
		path       string
		scope      apiextensionsv1.ResourceScope
		kind       string
		listKind   string
		plural     string
		singular   string
		shortNames []string
	}{
		{clusterProjectsCRD, apiextensionsv1.ClusterScoped, "ClusterProject", "ClusterProjectList", "clusterprojects", "clusterproject", []string{"hcp"}},
		{checksCRD, apiextensionsv1.NamespaceScoped, "Check", "CheckList", "checks", "check", []string{"hc"}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			crd := loadCRDFrom(t, c.path)
			mustMatch(t, crd.Spec.Group, "healthchecks.guid.foo")
			mustMatch(t, crd.Spec.Scope, c.scope)
			mustMatch(t, crd.Spec.Names.Kind, c.kind)
			mustMatch(t, crd.Spec.Names.ListKind, c.listKind)
			mustMatch(t, crd.Spec.Names.Plural, c.plural)
			mustMatch(t, crd.Spec.Names.Singular, c.singular)
			mustDeepEqual(t, []string(crd.Spec.Names.ShortNames), c.shortNames)
		})
	}
}

func TestCRDHasOneServedStorageVersion(t *testing.T) {
	for _, path := range []string{clusterProjectsCRD, checksCRD} {
		t.Run(path, func(t *testing.T) {
			crd := loadCRDFrom(t, path)
			if len(crd.Spec.Versions) != 1 {
				t.Fatalf("got %d versions, want 1", len(crd.Spec.Versions))
			}
			v := crd.Spec.Versions[0]
			mustMatch(t, v.Name, "v1alpha1")
			if !v.Served {
				t.Error("version v1alpha1 is not served")
			}
			if !v.Storage {
				t.Error("version v1alpha1 is not the storage version")
			}
		})
	}
}

// The status subresource is what keeps the operator's writes off a spec
// a person declared.
func TestCRDHasAStatusSubresource(t *testing.T) {
	for _, path := range []string{clusterProjectsCRD, checksCRD} {
		t.Run(path, func(t *testing.T) {
			subresources := loadCRDFrom(t, path).Spec.Versions[0].Subresources
			if subresources == nil || subresources.Status == nil {
				t.Fatalf("subresources = %+v, want a status subresource", subresources)
			}
		})
	}
}

// column is one printer column as a test states it: its name, its path,
// and its priority, where 1 is the wide view.
type column struct {
	name, path string
	priority   int32
}

func condition(kind string) string {
	return `.status.conditions[?(@.type=="` + kind + `")].status`
}

func conditionReason(kind string) string {
	return `.status.conditions[?(@.type=="` + kind + `")].reason`
}

const age = ".metadata.creationTimestamp"

func columnsOf(printed []apiextensionsv1.CustomResourceColumnDefinition) []column {
	columns := make([]column, 0, len(printed))
	for _, one := range printed {
		columns = append(columns, column{one.Name, one.JSONPath, one.Priority})
	}
	return columns
}

func TestThePrinterColumns(t *testing.T) {
	cases := []struct {
		path string
		want []column
	}{
		{clusterProjectsCRD, []column{
			{"URL", ".spec.url", 0},
			{"Ready", condition("Ready"), 0},
			{"Age", age, 0},
			{"Channels", ".spec.channels", 1},
			{"Reason", conditionReason("Ready"), 1},
		}},
		{checksCRD, []column{
			{"Project", ".spec.projectRef.name", 0},
			{"Probe", ".status.probe", 0},
			{"Ready", condition("Ready"), 0},
			{"Passing", condition("Passing"), 0},
			{"Age", age, 0},
			{"Slug", ".status.slug", 1},
			{"Reason", conditionReason("Ready"), 1},
		}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			mustDeepEqual(t, columnsOf(loadCRDFrom(t, c.path).Spec.Versions[0].AdditionalPrinterColumns), c.want)
		})
	}

	// A wide view exists for both CRDs: at least one column carries
	// priority 1, the column kubectl get -o wide adds.
	for _, path := range []string{clusterProjectsCRD, checksCRD} {
		t.Run(path+"/wide view", func(t *testing.T) {
			printed := loadCRDFrom(t, path).Spec.Versions[0].AdditionalPrinterColumns
			if !slices.ContainsFunc(printed, func(c apiextensionsv1.CustomResourceColumnDefinition) bool { return c.Priority == 1 }) {
				t.Error("no printer column at priority 1, want a wide view")
			}
		})
	}
}

// The API server runs the whole definition through this before it
// serves the resource, and that pass is where a CEL rule is compiled
// and its cost estimated. A rule the estimator refuses is a CRD the
// cluster refuses whole, and no amount of validating objects against
// the schema would find it.
func TestTheAPIServerWouldAcceptTheCRD(t *testing.T) {
	for _, path := range []string{clusterProjectsCRD, checksCRD} {
		t.Run(path, func(t *testing.T) {
			scheme := runtime.NewScheme()
			install.Install(scheme)

			internal := &apiextensions.CustomResourceDefinition{}
			mustSucceed(t, scheme.Convert(loadCRDFrom(t, path), internal, nil))
			// The API server fills the stored versions in when it creates
			// the definition; a manifest states none.
			internal.Status.StoredVersions = []string{internal.Spec.Versions[0].Name}

			if errs := crdvalidation.ValidateCustomResourceDefinition(t.Context(), internal); len(errs) > 0 {
				t.Errorf("the API server would refuse the definition: %v", errs)
			}
		})
	}
}

// internalSchemaFrom converts the CRD's v1 schema into the internal
// type both validators work against, the same conversion the API
// server does before it serves the resource.
func internalSchemaFrom(t *testing.T, path string) *apiextensions.JSONSchemaProps {
	t.Helper()
	crd := loadCRDFrom(t, path)

	scheme := runtime.NewScheme()
	install.Install(scheme)

	internal := &apiextensions.JSONSchemaProps{}
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil); err != nil {
		t.Fatalf("converting the schema: %v", err)
	}
	return internal
}

// schemaValidator builds the validator the API server itself would run
// an object through.
func schemaValidator(t *testing.T, path string) validation.SchemaValidator {
	t.Helper()
	validator, _, err := validation.NewSchemaValidator(internalSchemaFrom(t, path))
	if err != nil {
		t.Fatalf("building the schema validator: %v", err)
	}
	return validator
}

// celValidator builds the validator that runs the schema's
// x-kubernetes-validations rules. It is a separate pass from the
// structural one, the way the API server runs it, so a rule is proved
// only by calling this. A schema that states no rule, such as
// ClusterProject's, builds no validator; validateObject skips the CEL
// pass in that case.
func celValidator(t *testing.T, path string) (*celschema.Validator, *structuralschema.Structural) {
	t.Helper()
	structural, err := structuralschema.NewStructural(internalSchemaFrom(t, path))
	if err != nil {
		t.Fatalf("building the structural schema: %v", err)
	}
	return celschema.NewValidator(structural, true, celconfig.PerCallLimit), structural
}

// validateObject runs both passes, so a case states one verdict for the
// whole object the way the API server answers one.
func validateObject(t *testing.T, path string, object map[string]any) field.ErrorList {
	t.Helper()
	errs := validation.ValidateCustomResource(field.NewPath(""), object, schemaValidator(t, path))
	validator, structural := celValidator(t, path)
	if validator == nil {
		return errs
	}
	celErrs, _ := validator.Validate(
		t.Context(), field.NewPath(""), structural, object, nil, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

// TestChecksCRDCompilesItsCELRules proves the Check schema states at
// least one x-kubernetes-validations rule, and that the estimator
// accepts its cost, separately from any one object validateObject
// checks.
func TestChecksCRDCompilesItsCELRules(t *testing.T) {
	validator, _ := celValidator(t, checksCRD)
	if validator == nil {
		t.Fatal("the Check schema states no x-kubernetes-validations rule")
	}
}

// The examples are what a reader copies, so the cluster has to accept
// them.
func TestTheExamplesValidate(t *testing.T) {
	cases := []struct {
		crd  string
		path string
	}{
		{clusterProjectsCRD, "deploy/examples/clusterproject.yaml"},
		{checksCRD, "deploy/examples/check-http.yaml"},
		{checksCRD, "deploy/examples/check-tls.yaml"},
		{checksCRD, "deploy/examples/check-cronjob.yaml"},
		{checksCRD, "deploy/examples/check-ping.yaml"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			raw, err := os.ReadFile(c.path)
			mustSucceed(t, err)

			example := map[string]any{}
			mustSucceed(t, yaml.UnmarshalStrict(raw, &example))

			if errs := validateObject(t, c.crd, example); len(errs) > 0 {
				t.Errorf("%s: %v", c.path, errs)
			}
		})
	}
}
