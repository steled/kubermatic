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

// Package maintenance implements an installer-controlled maintenance mode for
// the KKP dashboard and API on top of Envoy Gateway.
//
// Instead of modifying the operator-managed HTTPRoute (which the operator would
// revert on its next reconciliation), enabling the maintenance mode creates a
// separate HTTPRoute for the same hostname and Gateway. Its rules are crafted to
// take precedence over the operator's routes and answer with a static 503 via an
// Envoy Gateway HTTPRouteFilter "directResponse", so no extra pod or image is
// needed. Disabling the maintenance mode deletes the additional resources again.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	kubermaticv1 "k8c.io/kubermatic/sdk/v2/apis/kubermatic/v1"
	"k8c.io/kubermatic/v2/pkg/defaulting"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// defaultLogoPath is the logo the dashboard shows if no branding logo is configured.
	defaultLogoPath = "/assets/images/branding/logo.svg"

	// RouteName is the name of the additional HTTPRoute.
	RouteName = "kubermatic-maintenance"
	// HTMLConfigMapName holds the HTML page.
	HTMLConfigMapName = "kubermatic-maintenance-page"
	// HTMLFilterName is the HTTPRouteFilter answering browsers.
	HTMLFilterName = "kubermatic-maintenance-html"
	// JSONFilterName is the HTTPRouteFilter answering API clients.
	JSONFilterName = "kubermatic-maintenance-json"

	// EnabledAtAnnotation records when the maintenance mode was enabled.
	EnabledAtAnnotation = "kubermatic.k8c.io/maintenance-enabled-at"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kubermatic-installer"

	htmlBodyKey = "response.body"

	envoyGatewayGroup = "gateway.envoyproxy.io"
	filterKind        = "HTTPRouteFilter"
)

var filterGVK = schema.GroupVersionKind{Group: envoyGatewayGroup, Version: "v1alpha1", Kind: filterKind}

// httpMethods are all methods that get an explicit match. The Gateway API rule
// precedence is: exact path > longest prefix > method > header > query. As the
// operator route matches the same paths ("/api" and "/") without a method, a
// match with a method is what lets our rules win over it.
var httpMethods = []gatewayapiv1.HTTPMethod{
	gatewayapiv1.HTTPMethodGet,
	gatewayapiv1.HTTPMethodHead,
	gatewayapiv1.HTTPMethodPost,
	gatewayapiv1.HTTPMethodPut,
	gatewayapiv1.HTTPMethodPatch,
	gatewayapiv1.HTTPMethodDelete,
	gatewayapiv1.HTTPMethodOptions,
}

// Options configures Enable.
type Options struct {
	Page PageOptions
	// RetryAfter is sent as Retry-After header (seconds). Defaults to 300.
	RetryAfter int
	// AssetFetcher loads relative branding assets (e.g. "/branding/logo.svg") from
	// the dashboard, so they can be embedded into the page. Defaults to fetching
	// them from the public domain via HTTPS.
	AssetFetcher AssetFetcher
	// Warn is called for non-fatal problems, e.g. an asset that cannot be embedded.
	Warn func(format string, args ...any)
}

// Enable switches the maintenance mode on. It is idempotent, running it again
// updates the page.
func Enable(ctx context.Context, client ctrlruntimeclient.Client, cfg *kubermaticv1.KubermaticConfiguration, namespace string, opts Options) error {
	var operatorRoute gatewayapiv1.HTTPRoute
	key := types.NamespacedName{Namespace: namespace, Name: defaulting.DefaultHTTPRouteName}
	if err := client.Get(ctx, key, &operatorRoute); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("HTTPRoute %s not found: the maintenance mode requires a KKP installation using the Gateway API", key)
		}
		return fmt.Errorf("failed to get HTTPRoute %s: %w", key, err)
	}

	if opts.Page.Branding == (Branding{}) {
		branding, err := ParseBranding(cfg.Spec.UI.Config)
		if err != nil {
			return err
		}
		opts.Page.Branding = branding
	}

	// without custom logos the dashboard shows its default logo, so does the page
	if opts.Page.Branding.LogoURL == "" && opts.Page.Branding.LogoDarkURL == "" {
		opts.Page.Branding.LogoURL = defaultLogoPath
		opts.Page.InvertLogoInDark = true
	}

	// this has to happen before the route is created, afterwards the dashboard is no longer reachable
	if opts.AssetFetcher == nil {
		opts.AssetFetcher = HTTPAssetFetcher(http.DefaultClient, "https://"+cfg.Spec.Ingress.Domain)
	}
	opts.Page.Branding = ResolveAssets(ctx, opts.AssetFetcher, opts.Page.Branding, opts.Warn)

	if opts.RetryAfter <= 0 {
		opts.RetryAfter = 300
	}

	html, err := RenderPage(opts.Page)
	if err != nil {
		return err
	}

	jsonBody, err := RenderJSON(opts.Page)
	if err != nil {
		return err
	}

	// the page is stored in a ConfigMap, because inline bodies of HTTPRouteFilters are size-limited
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: HTMLConfigMapName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, client, cm, func() error {
		cm.Labels = map[string]string{managedByLabel: managedByValue}
		cm.Data = map[string]string{htmlBodyKey: escapeEnvoyFormat(string(html))}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ConfigMap %s: %w", HTMLConfigMapName, err)
	}

	htmlFilter := directResponseFilter(namespace, HTMLFilterName, "text/html; charset=utf-8", map[string]any{
		"type": "ValueRef",
		"valueRef": map[string]any{
			"group": "",
			"kind":  "ConfigMap",
			"name":  HTMLConfigMapName,
		},
	})
	jsonFilter := directResponseFilter(namespace, JSONFilterName, "application/json", map[string]any{
		"type":   "Inline",
		"inline": escapeEnvoyFormat(string(jsonBody)),
	})

	for _, f := range []*unstructured.Unstructured{htmlFilter, jsonFilter} {
		if err := createOrUpdateFilter(ctx, client, f); err != nil {
			return err
		}
	}

	route := &gatewayapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: RouteName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, client, route, func() error {
		route.Labels = map[string]string{managedByLabel: managedByValue}
		if route.Annotations == nil {
			route.Annotations = map[string]string{}
		}
		if _, ok := route.Annotations[EnabledAtAnnotation]; !ok {
			route.Annotations[EnabledAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
		}

		// follow wherever the operator-managed route is attached (managed or external Gateway)
		route.Spec.ParentRefs = operatorRoute.Spec.ParentRefs
		route.Spec.Hostnames = operatorRoute.Spec.Hostnames
		route.Spec.Rules = buildRules(opts.RetryAfter)

		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile HTTPRoute %s: %w", RouteName, err)
	}

	return nil
}

// Disable switches the maintenance mode off by removing everything Enable created.
func Disable(ctx context.Context, client ctrlruntimeclient.Client, namespace string) error {
	objs := []ctrlruntimeclient.Object{
		&gatewayapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: RouteName, Namespace: namespace}},
		newFilter(namespace, HTMLFilterName),
		newFilter(namespace, JSONFilterName),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: HTMLConfigMapName, Namespace: namespace}},
	}

	var errs []error
	for _, obj := range objs {
		// the route goes first, so the dashboard is served again before the filters vanish
		if err := client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			errs = append(errs, fmt.Errorf("failed to delete %T %s: %w", obj, obj.GetName(), err))
		}
	}

	return errors.Join(errs...)
}

// Status describes the current state of the maintenance mode.
type Status struct {
	Enabled   bool
	EnabledAt string
}

// GetStatus returns whether the maintenance mode is currently enabled.
func GetStatus(ctx context.Context, client ctrlruntimeclient.Client, namespace string) (Status, error) {
	var route gatewayapiv1.HTTPRoute
	if err := client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: RouteName}, &route); err != nil {
		if apierrors.IsNotFound(err) {
			return Status{}, nil
		}
		return Status{}, fmt.Errorf("failed to get HTTPRoute %s: %w", RouteName, err)
	}

	return Status{Enabled: true, EnabledAt: route.Annotations[EnabledAtAnnotation]}, nil
}

// escapeEnvoyFormat escapes percent signs, because Envoy evaluates "%COMMAND%"
// substitutions in direct response bodies and rejects the whole route when it
// finds something that is not a valid command (e.g. "width: 100%;" in CSS).
func escapeEnvoyFormat(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

func newFilter(namespace, name string) *unstructured.Unstructured {
	f := &unstructured.Unstructured{}
	f.SetGroupVersionKind(filterGVK)
	f.SetNamespace(namespace)
	f.SetName(name)

	return f
}

func directResponseFilter(namespace, name, contentType string, body map[string]any) *unstructured.Unstructured {
	f := newFilter(namespace, name)
	f.SetLabels(map[string]string{managedByLabel: managedByValue})
	f.Object["spec"] = map[string]any{
		"directResponse": map[string]any{
			"contentType": contentType,
			"statusCode":  int64(503),
			"body":        body,
		},
	}

	return f
}

func createOrUpdateFilter(ctx context.Context, client ctrlruntimeclient.Client, desired *unstructured.Unstructured) error {
	existing := newFilter(desired.GetNamespace(), desired.GetName())
	if _, err := controllerutil.CreateOrUpdate(ctx, client, existing, func() error {
		existing.SetLabels(desired.GetLabels())
		existing.Object["spec"] = desired.Object["spec"]
		return nil
	}); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("the %s/%s CRD is not installed: the maintenance mode requires Envoy Gateway", envoyGatewayGroup, filterKind)
		}
		return fmt.Errorf("failed to reconcile %s %s: %w", filterKind, desired.GetName(), err)
	}

	return nil
}

func buildRules(retryAfter int) []gatewayapiv1.HTTPRouteRule {
	pathPrefix := gatewayapiv1.PathMatchPathPrefix
	headerRegex := gatewayapiv1.HeaderMatchRegularExpression

	var htmlMatches, jsonMatches []gatewayapiv1.HTTPRouteMatch

	// "/api" has to be listed as well, because the operator route matches it with a longer prefix than "/"
	for _, path := range []string{"/api", "/"} {
		for _, method := range httpMethods {
			base := gatewayapiv1.HTTPRouteMatch{
				Path:   &gatewayapiv1.HTTPPathMatch{Type: &pathPrefix, Value: ptr.To(path)},
				Method: ptr.To(method),
			}

			jsonMatches = append(jsonMatches, base)

			// browsers announce text/html in their Accept header, which has precedence over the match above
			withHeader := *base.DeepCopy()
			withHeader.Headers = []gatewayapiv1.HTTPHeaderMatch{{
				Type:  &headerRegex,
				Name:  "Accept",
				Value: ".*text/html.*",
			}}
			htmlMatches = append(htmlMatches, withHeader)
		}
	}

	retryAfterFilter := gatewayapiv1.HTTPRouteFilter{
		Type: gatewayapiv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayapiv1.HTTPHeaderFilter{
			Set: []gatewayapiv1.HTTPHeader{{Name: "Retry-After", Value: fmt.Sprintf("%d", retryAfter)}},
		},
	}

	extensionFilter := func(name string) gatewayapiv1.HTTPRouteFilter {
		return gatewayapiv1.HTTPRouteFilter{
			Type: gatewayapiv1.HTTPRouteFilterExtensionRef,
			ExtensionRef: &gatewayapiv1.LocalObjectReference{
				Group: envoyGatewayGroup,
				Kind:  filterKind,
				Name:  gatewayapiv1.ObjectName(name),
			},
		}
	}

	return []gatewayapiv1.HTTPRouteRule{
		{Matches: htmlMatches, Filters: []gatewayapiv1.HTTPRouteFilter{retryAfterFilter, extensionFilter(HTMLFilterName)}},
		{Matches: jsonMatches, Filters: []gatewayapiv1.HTTPRouteFilter{retryAfterFilter, extensionFilter(JSONFilterName)}},
	}
}

// Summary returns a human-readable description for the CLI.
func (s Status) Summary() string {
	if !s.Enabled {
		return "Maintenance mode is disabled."
	}

	if s.EnabledAt == "" {
		return "Maintenance mode is enabled."
	}

	return fmt.Sprintf("Maintenance mode is enabled (since %s).", strings.TrimSpace(s.EnabledAt))
}
