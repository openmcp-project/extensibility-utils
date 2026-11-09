package discovery

import (
	"encoding/json"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	ocmv1alpha1 "ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	ocmpath "ocm.software/open-component-model/bindings/go/oci/spec/repository/path"
	ocirepov1 "ocm.software/open-component-model/bindings/go/oci/spec/repository/v1/oci"
	"ocm.software/open-component-model/bindings/go/runtime"
)

// mustRaw marshals an OCIImage access JSON into a *runtime.Raw for use in test resources.
func mustRaw(t *testing.T, imageRef string) *runtime.Raw {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"type":           "OCIImage",
		"imageReference": imageRef,
	})
	if err != nil {
		t.Fatalf("marshal access: %v", err)
	}
	raw := &runtime.Raw{}
	if err := json.Unmarshal(data, raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	return raw
}

// mustDescriptorJSON marshals a v2.Descriptor into a JSON blob for Status.Components.
func mustDescriptorJSON(t *testing.T, desc v2.Descriptor) []byte {
	t.Helper()
	data, err := json.Marshal(desc)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	return data
}

// mustLocalBlobRaw builds a *runtime.Raw for a LocalBlob access, optionally with a global OCIImage access.
func mustLocalBlobRaw(t *testing.T, localRef string, globalImageRef string) *runtime.Raw {
	if t != nil {
		t.Helper()
	}
	m := map[string]any{
		"type":           "LocalBlob/v1",
		"localReference": localRef,
		"mediaType":      "application/vnd.oci.image.manifest.v1+json",
	}
	if globalImageRef != "" {
		m["globalAccess"] = map[string]string{
			"type":           "OCIImage",
			"imageReference": globalImageRef,
		}
	}
	data, _ := json.Marshal(m)
	raw := &runtime.Raw{}
	_ = json.Unmarshal(data, raw)
	return raw
}

func TestComponentDescriptorBase(t *testing.T) {
	suffix := "/" + ocmpath.DefaultComponentDescriptorPath

	tests := []struct {
		name    string
		repo    ocirepov1.Repository
		want    string
		wantErr bool
	}{
		{
			name: "plain host",
			repo: ocirepov1.Repository{BaseUrl: "ghcr.io"},
			want: "ghcr.io" + suffix,
		},
		{
			name: "host with https scheme",
			repo: ocirepov1.Repository{BaseUrl: "https://ghcr.io"},
			want: "ghcr.io" + suffix,
		},
		{
			name: "trailing slash stripped",
			repo: ocirepov1.Repository{BaseUrl: "ghcr.io/"},
			want: "ghcr.io" + suffix,
		},
		{
			name: "path in BaseUrl kept",
			repo: ocirepov1.Repository{BaseUrl: "ghcr.io/org/repo"},
			want: "ghcr.io/org/repo" + suffix,
		},
		{
			name: "explicit SubPath",
			repo: ocirepov1.Repository{BaseUrl: "ghcr.io", SubPath: "open-control-plane"},
			want: "ghcr.io/open-control-plane" + suffix,
		},
		{
			name: "scheme + path",
			repo: ocirepov1.Repository{BaseUrl: "https://ghcr.io/org/repo"},
			want: "ghcr.io/org/repo" + suffix,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := componentDescriptorBase(&tc.repo)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseDiscovery(t *testing.T) {
	makeDesc := func(name, version string) v2.Descriptor {
		return v2.Descriptor{
			Component: v2.Component{
				ComponentMeta: v2.ComponentMeta{ObjectMeta: v2.ObjectMeta{Name: name, Version: version}},
			},
		}
	}

	tests := []struct {
		name       string
		components []apiextensionsv1.JSON
		wantCount  int
		wantErr    bool
	}{
		{
			name: "single component",
			components: []apiextensionsv1.JSON{
				{Raw: mustDescriptorJSON(t, makeDesc("ghcr.io/org/comp-a", "v1.0.0"))},
			},
			wantCount: 1,
		},
		{
			name: "multiple components",
			components: []apiextensionsv1.JSON{
				{Raw: mustDescriptorJSON(t, makeDesc("ghcr.io/org/comp-a", "v1.0.0"))},
				{Raw: mustDescriptorJSON(t, makeDesc("ghcr.io/org/comp-b", "v2.0.0"))},
			},
			wantCount: 2,
		},
		{
			name:       "invalid JSON returns error",
			components: []apiextensionsv1.JSON{{Raw: []byte("not-json")}},
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disc := ocmv1alpha1.Discovery{
				Status: ocmv1alpha1.DiscoveryStatus{Components: tc.components},
			}
			components, err := parseDiscovery(disc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(components) != tc.wantCount {
				t.Fatalf("want %d components, got %d", tc.wantCount, len(components))
			}
		})
	}
}

func TestFindComponent(t *testing.T) {
	components := []Component{
		{Name: "ghcr.io/org/comp-a", Version: "v1.0.0"},
		{Name: "ghcr.io/org/comp-a", Version: "v2.0.0"},
		{Name: "ghcr.io/org/comp-b", Version: "v1.0.0"},
	}

	tests := []struct {
		name      string
		matchFunc ComponentMatchFunc
		version   string
		wantName  string
		wantFound bool
	}{
		{
			name:      "nil matchFunc matches any component with version",
			matchFunc: nil,
			version:   "v1.0.0",
			wantName:  "ghcr.io/org/comp-a",
			wantFound: true,
		},
		{
			name:      "match by name and version",
			matchFunc: ComponentByName("ghcr.io/org/comp-b"),
			version:   "v1.0.0",
			wantName:  "ghcr.io/org/comp-b",
			wantFound: true,
		},
		{
			name:      "match by short name",
			matchFunc: ComponentByShortName("comp-a"),
			version:   "v2.0.0",
			wantName:  "ghcr.io/org/comp-a",
			wantFound: true,
		},
		{
			name:      "version mismatch returns not found",
			matchFunc: ComponentByName("ghcr.io/org/comp-a"),
			version:   "v9.0.0",
			wantFound: false,
		},
		{
			name:      "no matching component",
			matchFunc: ComponentByName("ghcr.io/org/comp-missing"),
			version:   "v1.0.0",
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := findComponent(components, tc.matchFunc, tc.version)
			if ok != tc.wantFound {
				t.Fatalf("found = %v, want %v", ok, tc.wantFound)
			}
			if ok && got.Name != tc.wantName {
				t.Errorf("got name %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}

func TestFindResource(t *testing.T) {
	imageResource := v2.Resource{
		ElementMeta: v2.ElementMeta{ObjectMeta: v2.ObjectMeta{Name: "test-image"}},
		Type:        "image",
		Access:      mustRaw(t, "ghcr.io/test-image:v1.0.0"),
	}
	component := Component{
		Name:      "ghcr.io/org/repo/comp",
		Resources: []v2.Resource{imageResource},
	}

	tests := []struct {
		name          string
		matchFunc     ResourceMatchFunc
		wantAccessURL string
		wantErr       bool
	}{
		{
			name:    "nil matchFunc returns error",
			wantErr: true,
		},
		{
			name:          "matching resource is returned",
			matchFunc:     ResourceByName("test-image"),
			wantAccessURL: "ghcr.io/test-image:v1.0.0",
		},
		{
			name:      "no matching resource returns error",
			matchFunc: ResourceByName("missing"),
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := findResource(component, "ghcr.io/org/repo", tc.matchFunc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && res.AccessURL != tc.wantAccessURL {
				t.Errorf("got access URL %q, want %q", res.AccessURL, tc.wantAccessURL)
			}
		})
	}
}

func TestMatchFunctions(t *testing.T) {
	production, _ := json.Marshal("production")
	staging, _ := json.Marshal("staging")
	component := Component{
		Name:   "ghcr.io/org/comp-a",
		Labels: []v2.Label{{Name: "env", Value: production}},
	}
	resource := v2.Resource{
		ElementMeta: v2.ElementMeta{
			ObjectMeta: v2.ObjectMeta{
				Name:   "ghcr.io/org/repo/test-image",
				Labels: []v2.Label{{Name: "env", Value: production}},
			},
		},
		Type: "helmChart",
	}

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"ComponentByName exact", ComponentByName("ghcr.io/org/comp-a")(component), true},
		{"ComponentByName wrong", ComponentByName("ghcr.io/org/other")(component), false},
		{"ComponentByShortName exact", ComponentByShortName("comp-a")(component), true},
		{"ComponentByShortName partial path", ComponentByShortName("org/comp-a")(component), false},
		{"ComponentByShortName unrelated", ComponentByShortName("missing")(component), false},
		{"ComponentByLabel matching key and value", ComponentByLabel("env", production)(component), true},
		{"ComponentByLabel matching key wrong value", ComponentByLabel("env", staging)(component), false},
		{"ComponentByLabel missing key", ComponentByLabel("missing", production)(component), false},
		{"ResourceByType matching", ResourceByType("helmChart")(resource), true},
		{"ResourceByType wrong", ResourceByType("image")(resource), false},
		{"ResourceByLabel matching key and value", ResourceByLabel("env", production)(resource), true},
		{"ResourceByLabel matching key wrong value", ResourceByLabel("env", staging)(resource), false},
		{"ResourceByLabel missing key", ResourceByLabel("missing", production)(resource), false},
		{"ResourceByShortName exact", ResourceByShortName("test-image")(resource), true},
		{"ResourceByShortName partial path", ResourceByShortName("org/repo/test-image")(resource), false},
		{"ResourceByShortName unrelated", ResourceByShortName("missing")(resource), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

func TestDecodeAccess(t *testing.T) {
	const basePath = "ghcr.io/org/component-descriptors"
	const componentName = "github.com/org/comp"

	tests := []struct {
		name    string
		access  *runtime.Raw
		wantURL string
		wantErr bool
	}{
		{
			name:    "OCIImage returns image reference",
			access:  mustRaw(t, "registry.example.com/img:v1.0"),
			wantURL: "registry.example.com/img:v1.0",
		},
		{
			name:    "LocalBlob without GlobalAccess builds URL from basePath",
			access:  mustLocalBlobRaw(t, "sha256:abc123", ""),
			wantURL: basePath + "/" + componentName + "@sha256:abc123",
		},
		{
			name:    "LocalBlob with GlobalAccess returns global image reference",
			access:  mustLocalBlobRaw(t, "sha256:abc123", "registry.example.com/global:v1.0"),
			wantURL: "registry.example.com/global:v1.0",
		},
		{
			name:    "nil access returns error",
			access:  nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resource := v2.Resource{
				ElementMeta: v2.ElementMeta{ObjectMeta: v2.ObjectMeta{Name: "res"}},
				Access:      tc.access,
			}
			got, err := decodeAccess(resource, basePath, componentName)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.wantURL {
				t.Errorf("got %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		want     []string
	}{
		{
			name:     "orders by patch",
			versions: []string{"1.2.4", "1.2.3"},
			want:     []string{"1.2.3", "1.2.4"},
		},
		{
			name:     "multi-digit minor sorts semantically, not lexically",
			versions: []string{"1.10.0", "1.9.0", "1.2.0"},
			want:     []string{"1.2.0", "1.9.0", "1.10.0"},
		},
		{
			name:     "v prefix is tolerated",
			versions: []string{"v1.2.4", "1.2.3"},
			want:     []string{"1.2.3", "v1.2.4"},
		},
		{
			name:     "unparseable versions sort after valid ones lexicographically",
			versions: []string{"notaversion-b", "1.2.0", "notaversion-a"},
			want:     []string{"1.2.0", "notaversion-a", "notaversion-b"},
		},
		{
			name:     "mixed",
			versions: []string{"v1.10.0", "1.2.0", "notaversion-a", "1.9.0"},
			want:     []string{"1.2.0", "1.9.0", "v1.10.0", "notaversion-a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sorted := slices.Clone(tc.versions)
			slices.SortFunc(sorted, compareVersions)
			if !slices.Equal(sorted, tc.want) {
				t.Errorf("sorted = %v, want %v", sorted, tc.want)
			}
		})
	}
}
