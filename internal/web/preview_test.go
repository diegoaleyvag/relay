package web

import (
	"os"
	"strings"
	"testing"
)

func TestStaticPreviewIsClearlyLimitedAndCannotMutate(t *testing.T) {
	html, err := os.ReadFile("../../preview/index.html")
	if err != nil {
		t.Fatalf("read static preview: %v", err)
	}
	body := strings.ToLower(string(html))
	for _, forbidden := range []string{"<form", `method="post"`, "hx-post", "fetch(", "xmlhttprequest", "<script"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("static preview must not contain %q", forbidden)
		}
	}
	for _, required := range []string{
		"static preview",
		"make run",
		`href="favicon.ico"`,
		`href="demo.html"`,
		`href="methodology.html"`,
		`href="https://diegoaleyvag.github.io/"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("static preview missing required disclosure %q", required)
		}
	}
}

func TestFiveDecisionsShellUsesContractTypography(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read control-room stylesheet: %v", err)
	}
	lower := strings.ToLower(string(css))
	for _, required := range []string{"public sans", "martian mono", "prefers-reduced-motion"} {
		if !strings.Contains(lower, required) {
			t.Errorf("control-room shell missing %q", required)
		}
	}
}
