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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestIsRelativeURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"/branding/logo.svg":         true,
		"assets/branding/logo.svg":   true,
		"https://example.com/l.svg":  false,
		"//example.com/l.svg":        false,
		"data:image/png;base64,AAAA": false,
		"":                           false,
	} {
		if got := isRelativeURL(raw); got != want {
			t.Errorf("isRelativeURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestResolveAssets(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/branding/logo.svg", func(w http.ResponseWriter, r *http.Request) {
		// many servers do not know SVG files
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	})
	mux.HandleFunc("/assets/branding/dark.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\n"))
	})
	mux.HandleFunc("/branding/big.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte(strings.Repeat("a", maxAssetSize+1)))
	})
	mux.HandleFunc("/branding/page.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html></html>")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	got := ResolveAssets(context.Background(), HTTPAssetFetcher(srv.Client(), srv.URL), Branding{
		LogoURL:     "/branding/logo.svg",
		LogoDarkURL: "assets/branding/dark.png",
		FaviconURL:  "/branding/missing.png",
	}, warn)

	if !strings.HasPrefix(got.LogoURL, "data:image/svg+xml;base64,") {
		t.Errorf("logo was not embedded as SVG: %q", got.LogoURL)
	}
	if !strings.HasPrefix(got.LogoDarkURL, "data:image/png;base64,") {
		t.Errorf("dark logo was not embedded as PNG: %q", got.LogoDarkURL)
	}
	if got.FaviconURL != "" || len(warnings) != 1 {
		t.Errorf("missing favicon should be dropped with one warning, got %q and %v", got.FaviconURL, warnings)
	}

	// too big and wrong type
	for _, path := range []string{"/branding/big.png", "/branding/page.html"} {
		warnings = nil
		got := ResolveAssets(context.Background(), HTTPAssetFetcher(srv.Client(), srv.URL), Branding{LogoURL: path}, warn)
		if got.LogoURL != "" || len(warnings) != 1 {
			t.Errorf("%s should be dropped with one warning, got %q and %v", path, got.LogoURL, warnings)
		}
	}

	// absolute URLs are left alone
	got = ResolveAssets(context.Background(), HTTPAssetFetcher(srv.Client(), srv.URL), Branding{LogoURL: "https://example.com/l.svg"}, warn)
	if got.LogoURL != "https://example.com/l.svg" {
		t.Errorf("absolute URL was changed: %q", got.LogoURL)
	}
}

func TestEmbeddedAssetsSurviveRendering(t *testing.T) {
	page, err := RenderPage(PageOptions{Branding: Branding{
		LogoURL:    "data:image/svg+xml;base64,PHN2Zy8+",
		FaviconURL: "data:image/x-icon;base64,AAAA",
	}})
	if err != nil {
		t.Fatal(err)
	}

	// srcset would turn the comma of a data URI into "%2c" and break it
	if strings.Contains(string(page), "srcset") {
		t.Error("page must not use srcset")
	}

	// html/template writes "+" as "&#43;" in attributes, which browsers decode again
	for _, want := range []string{`src="data:image/svg&#43;xml;base64,PHN2Zy8&#43;"`, `href="data:image/x-icon;base64,AAAA"`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestServiceProxyAssetFetcher(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	}))
	defer srv.Close()

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	got := ResolveAssets(context.Background(), ServiceProxyAssetFetcher(clientset.CoreV1().RESTClient(), "kubermatic", "kubermatic-dashboard", 80), Branding{
		LogoURL: "/branding/logo.svg",
	}, nil)

	if want := "/api/v1/namespaces/kubermatic/services/kubermatic-dashboard:80/proxy/branding/logo.svg"; gotPath != want {
		t.Errorf("requested path %q, want %q", gotPath, want)
	}
	if !strings.HasPrefix(got.LogoURL, "data:image/svg+xml;base64,") {
		t.Errorf("logo was not embedded: %q", got.LogoURL)
	}
}
