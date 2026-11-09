/*
Copyright 2026 The Kubermatic Kubernetes Platform contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maintenance

import (
	"context"
	"strings"
	"testing"

	kubermaticv1 "k8c.io/kubermatic/sdk/v2/apis/kubermatic/v1"
	"k8c.io/kubermatic/v2/pkg/defaulting"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const testNamespace = "kubermatic"

func TestParseBranding(t *testing.T) {
	b, err := ParseBranding(`{"branding": {"product_name": "Acme", "logo_url": "https://example.com/l.svg", "colors": {"primary": "#112233"}}, "other": 1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if b.ProductName != "Acme" || b.LogoURL != "https://example.com/l.svg" || b.Colors.Primary != "#112233" {
		t.Errorf("unexpected branding: %+v", b)
	}

	if b, err := ParseBranding(""); err != nil || b != (Branding{}) {
		t.Errorf("empty config should yield empty branding, got %+v, %v", b, err)
	}

	if _, err := ParseBranding("{nope"); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

func TestRenderPageUsesBranding(t *testing.T) {
	page, err := RenderPage(PageOptions{
		Message: "Upgrading to v2.99",
		Until:   "08:00 CET",
		Branding: Branding{
			ProductName:      "Acme Cloud",
			Tagline:          "Your cloud",
			PostfixPageTitle: "| Acme",
			LogoURL:          "https://example.com/logo.svg",
			LogoDarkURL:      "https://example.com/logo-dark.svg",
			Colors:           BrandingColors{Primary: "#abcdef"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"Upgrading to v2.99", "08:00 CET", "Your cloud", "Scheduled maintenance | Acme",
		`src="https://example.com/logo.svg"`, `class="logo dark" src="https://example.com/logo-dark.svg"`, "--primary: #abcdef",
	} {
		if !strings.Contains(string(page), want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestRenderPageSanitizesInput(t *testing.T) {
	page, err := RenderPage(PageOptions{
		Message: `<script>alert(1)</script>`,
		Branding: Branding{
			LogoURL:    "javascript:alert(1)",
			FaviconURL: "assets/branding/favicon.ico",
			FontFamily: "x}</style><script>alert(2)</script>",
			Colors:     BrandingColors{Primary: "red;}</style><script>alert(3)</script>"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := string(page)
	if strings.Contains(s, "<script>") {
		t.Errorf("page contains an unescaped script tag:\n%s", s)
	}
	if strings.Contains(s, "javascript:") || strings.Contains(s, "favicon.ico") {
		t.Errorf("page contains an unsafe or unresolvable URL:\n%s", s)
	}
}

func TestRenderPageDropsDarkLogoWithoutLightLogo(t *testing.T) {
	page, err := RenderPage(PageOptions{Branding: Branding{LogoDarkURL: "https://example.com/dark.svg"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(string(page), "dark.svg") {
		t.Error("dark logo should not be rendered without a light logo")
	}
}

func TestBuildRulesPrecedence(t *testing.T) {
	rules := buildRules(300)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}

	for i, rule := range rules {
		// every match needs a method, otherwise it would only tie with the operator route
		for _, m := range rule.Matches {
			if m.Method == nil {
				t.Errorf("rule %d: match %v has no method", i, m.Path)
			}
		}
	}

	// the HTML rule is the one with the additional header match
	for _, m := range rules[0].Matches {
		if len(m.Headers) != 1 || m.Headers[0].Name != "Accept" {
			t.Errorf("HTML rule match without Accept header: %+v", m)
		}
	}
	for _, m := range rules[1].Matches {
		if len(m.Headers) != 0 {
			t.Errorf("JSON rule match must not have headers: %+v", m)
		}
	}
}

func newTestClient(objs ...ctrlruntimeclient.Object) ctrlruntimeclient.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gatewayapiv1.Install(scheme)

	return fakectrlruntimeclient.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func operatorRoute() *gatewayapiv1.HTTPRoute {
	ns := gatewayapiv1.Namespace("kubermatic")

	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: defaulting.DefaultHTTPRouteName, Namespace: testNamespace},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{{Name: "external-gw", Namespace: &ns}},
			},
			Hostnames: []gatewayapiv1.Hostname{"kkp.example.com"},
		},
	}
}

func TestEnableAndDisable(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(operatorRoute())
	cfg := &kubermaticv1.KubermaticConfiguration{}
	cfg.Spec.UI.Config = `{"branding": {"product_name": "Acme"}}`

	status, err := GetStatus(ctx, client, testNamespace)
	if err != nil || status.Enabled {
		t.Fatalf("expected disabled initially, got %+v, %v", status, err)
	}

	// enabling twice has to work
	for i := 0; i < 2; i++ {
		if err := Enable(ctx, client, cfg, testNamespace, Options{}); err != nil {
			t.Fatalf("enable #%d failed: %v", i, err)
		}
	}

	var route gatewayapiv1.HTTPRoute
	if err := client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: RouteName}, &route); err != nil {
		t.Fatalf("maintenance route not created: %v", err)
	}

	if route.Spec.ParentRefs[0].Name != "external-gw" || route.Spec.Hostnames[0] != "kkp.example.com" {
		t.Errorf("route does not follow the operator route: %+v", route.Spec)
	}

	var cm corev1.ConfigMap
	if err := client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: HTMLConfigMapName}, &cm); err != nil {
		t.Fatalf("page ConfigMap not created: %v", err)
	}
	if !strings.Contains(cm.Data[htmlBodyKey], "Acme") {
		t.Error("page does not use the branding of the KubermaticConfiguration")
	}

	// Envoy interprets single percent signs in response bodies as commands
	if strings.Contains(strings.ReplaceAll(cm.Data[htmlBodyKey], "%%", ""), "%") {
		t.Error("page body contains an unescaped percent sign")
	}

	status, err = GetStatus(ctx, client, testNamespace)
	if err != nil || !status.Enabled || status.EnabledAt == "" {
		t.Fatalf("expected enabled, got %+v, %v", status, err)
	}

	// the operator-managed route must never be touched
	var op gatewayapiv1.HTTPRoute
	if err := client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: defaulting.DefaultHTTPRouteName}, &op); err != nil {
		t.Fatal(err)
	}
	if len(op.Spec.Rules) != 0 || len(op.Annotations) != 0 {
		t.Error("the operator route has been modified")
	}

	// disabling twice has to work as well
	for i := 0; i < 2; i++ {
		if err := Disable(ctx, client, testNamespace); err != nil {
			t.Fatalf("disable #%d failed: %v", i, err)
		}
	}

	err = client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: RouteName}, &route)
	if !apierrors.IsNotFound(err) {
		t.Errorf("maintenance route still exists: %v", err)
	}
}

func TestEnableRequiresOperatorRoute(t *testing.T) {
	err := Enable(context.Background(), newTestClient(), &kubermaticv1.KubermaticConfiguration{}, testNamespace, Options{})
	if err == nil || !strings.Contains(err.Error(), "Gateway API") {
		t.Errorf("expected an error about the missing Gateway API route, got %v", err)
	}
}

func TestEnableUsesDefaultLogoWithoutBranding(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(operatorRoute())

	var requested string
	fetch := func(_ context.Context, path string) ([]byte, string, error) {
		requested = path
		return []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "image/svg+xml", nil
	}

	if err := Enable(ctx, client, &kubermaticv1.KubermaticConfiguration{}, testNamespace, Options{AssetFetcher: fetch}); err != nil {
		t.Fatal(err)
	}

	if requested != defaultLogoPath {
		t.Errorf("expected the default logo %q to be fetched, got %q", defaultLogoPath, requested)
	}

	var cm corev1.ConfigMap
	if err := client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: HTMLConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`class="logo light"`, `<header class=" invert">`} {
		if !strings.Contains(cm.Data[htmlBodyKey], want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestCustomLogoIsNotInverted(t *testing.T) {
	page, err := RenderPage(PageOptions{Branding: Branding{LogoURL: "https://example.com/l.svg"}})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(page), "invert\">") {
		t.Error("a custom logo must not be inverted")
	}
}
