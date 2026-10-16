package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDirectoryRepresentationNegotiation(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "document.pdf", "pdf content")
	for _, tc := range []struct {
		name, accept, agent, path string
		markdown                  bool
	}{
		{"browser", "text/html,application/xhtml+xml,*/*;q=0.8", "Mozilla/5.0", "/", false},
		{"default", "", "", "/", false},
		{"wildcard", "*/*", "agent", "/", false},
		{"text wildcard", "text/*", "agent", "/", false},
		{"markdown", "text/markdown", "agent", "/", true},
		{"Claude", "text/markdown, text/html, */*", "Claude-User/1.0", "/", true},
		{"prefers HTML", "text/markdown;q=0.2, text/html;q=0.9", "agent", "/", false},
		{"prefers markdown", "text/html;q=0.2, text/markdown;q=0.9", "agent", "/", true},
		{"rejects markdown", "text/markdown;q=0, */*;q=1", "agent", "/", false},
		{"rejects HTML", "text/html;q=0, text/markdown;q=0.5, */*;q=1", "agent", "/", true},
		{"invalid quality", "text/markdown;q=NaN, text/html", "agent", "/", false},
		{"out of range quality", "text/markdown;q=2", "agent", "/", false},
		{"curl", "*/*", "curl/8.10.1", "/", true},
		{"bare curl", "", "curl", "/", true},
		{"curl overrides Accept", "text/html", "curl/8.10.1", "/", true},
		{"curl lookalike", "*/*", "Notcurl/1.0", "/", false},
		{"no format query", "text/html", "Mozilla/5.0", "/?format=markdown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := securityRequest("GET", tc.path, nil, map[string]string{"Accept": tc.accept, "User-Agent": tc.agent})
			want := "text/html; charset=utf-8"
			if tc.markdown {
				want = "text/markdown; charset=utf-8"
			}
			if w.Code != 200 || w.Header().Get("Content-Type") != want {
				t.Fatalf("response: %d %v", w.Code, w.Header())
			}
			vary := strings.Join(w.Header().Values("Vary"), ", ")
			if !strings.Contains(vary, "Accept") || !strings.Contains(vary, "User-Agent") {
				t.Fatal("missing representation cache dimensions", vary)
			}
			if !strings.Contains(w.Header().Get("Link"), "rel=\"alternate\"; type=\"text/markdown\"") {
				t.Fatal("missing alternate representation link")
			}
			body := w.Body.String()
			if tc.markdown {
				if !strings.Contains(body, "[document.pdf](<http://files.example/document.pdf>)") {
					t.Fatal("missing file link", body)
				}
				for _, unwanted := range []string{"<script", "<style", "XMLHttpRequest", "Delete"} {
					if strings.Contains(body, unwanted) {
						t.Fatal("directory includes controls or example", unwanted)
					}
				}
			} else if !strings.Contains(body, "<link rel=\"alternate\" type=\"text/markdown\" href=\"/\">") {
				t.Fatal("HTML does not advertise Markdown")
			}
		})
	}
}

func TestMarkdownAcceptUsesMultipleHeaderLines(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("Accept", "text/html;q=0.5")
	r.Header.Add("Accept", "text/markdown;q=1")
	if !wantsMarkdown(r) {
		t.Fatal("second Accept header was ignored")
	}
}

func TestMarkdownDirectoryLinksMetadataAndEscaping(t *testing.T) {
	securityWorkspace(t)
	folder := "résumé (2026)"
	if err := os.Mkdir(filepath.Join(filesDir, folder), 0755); err != nil {
		t.Fatal(err)
	}
	name := "a|b [x](evil) <script> \"quote\"\n_日本語.png"
	relative := filepath.Join(folder, name)
	securityWrite(t, relative, "png")
	securityWrite(t, filepath.Join(folder, "a.txt"), "text")
	if err := os.Mkdir(filepath.Join(filesDir, folder, "child"), 0755); err != nil {
		t.Fatal(err)
	}
	securityWrite(t, filepath.Join(folder, metadataDir), "private")
	if err := os.Symlink(filepath.Join(folder, "a.txt"), filepath.Join(filesDir, folder, "alias")); err != nil {
		t.Skip(err)
	}
	fileActions.expirations[relative] = fileExpiry{ExpiresAt: time.Unix(2000000000, 0)}
	listingPath := "/" + url.PathEscape(folder) + "/"
	w := securityRequest("GET", listingPath, nil, map[string]string{
		"Accept": "text/markdown", "X-Forwarded-Proto": "https",
	})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "[Parent directory](<https://files.example/>)") {
		t.Fatal(w.Code, body)
	}
	target := (&url.URL{Scheme: "https", Host: "files.example", Path: "/" + folder + "/" + name}).String()
	if !strings.Contains(body, "(<"+target+">)") || !strings.Contains(body, "a\\|b \\[x\\]\\(evil\\) &lt;script&gt; &#34;quote&#34; \\_日本語.png") {
		t.Fatal("unsafe or incorrect file link", body)
	}
	for _, forbidden := range []string{metadataDir, "alias", "<script>", "\n_日本語"} {
		if strings.Contains(body, forbidden) {
			t.Fatal("invalid listing content", forbidden, body)
		}
	}
	if !strings.Contains(body, "2033-05-18T03:33:20Z") {
		t.Fatal("missing UTC expiry", body)
	}
	if strings.Index(body, "[child/") > strings.Index(body, "[a.txt]") {
		t.Fatal("directories are not sorted first")
	}
	target = (&url.URL{Scheme: "https", Host: "files.example", Path: "/" + folder + "/child/"}).String()
	if !strings.Contains(body, "(<"+target+">)") {
		t.Fatal("incorrect folder link", body)
	}
}

func TestMarkdownHeadAndEmptyDirectory(t *testing.T) {
	securityWorkspace(t)
	get := securityRequest("GET", "/", nil, map[string]string{"User-Agent": "curl/8"})
	head := securityRequest("HEAD", "/", nil, map[string]string{"User-Agent": "curl/8"})
	if !strings.Contains(get.Body.String(), "This directory is empty.") || head.Body.Len() != 0 ||
		head.Header().Get("Content-Type") != get.Header().Get("Content-Type") ||
		head.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
		t.Fatal("HEAD or empty-directory response is incorrect")
	}
	if w := securityRequest("HEAD", "/", nil, map[string]string{"Accept": "text/html"}); w.Body.Len() != 0 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal("HTML HEAD returned a body")
	}
}

func TestMarkdownNegotiationDoesNotChangeFileDownloads(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "image.png", "\x89PNG\r\n\x1a\nfile bytes")
	for _, headers := range []map[string]string{
		{"Accept": "text/markdown"},
		{"User-Agent": "curl/8", "Accept": "*/*"},
	} {
		w := securityRequest("GET", "/image.png", nil, headers)
		if w.Code != 200 || w.Body.String() != "\x89PNG\r\n\x1a\nfile bytes" ||
			w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Disposition") == "" {
			t.Fatal("file download was converted", w.Code, w.Header())
		}
		headers["Range"] = "bytes=0-3"
		w = securityRequest("GET", "/image.png", nil, headers)
		if w.Code != http.StatusPartialContent || w.Body.String() != "\x89PNG" {
			t.Fatal("ranged download was changed", w.Code)
		}
	}
	if w := securityRequest("GET", "/llms.txt", nil, map[string]string{"User-Agent": "curl/8"}); w.Code != 404 {
		t.Fatal("llms.txt endpoint was added")
	}
}

func TestDirectoryRedirectKeepsQuery(t *testing.T) {
	securityWorkspace(t)
	if err := os.Mkdir(filepath.Join(filesDir, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	w := securityRequest("GET", "/folder?existing=value", nil, nil)
	if w.Code != 302 || w.Header().Get("Location") != "/folder/?existing=value" {
		t.Fatal("directory redirect lost query", w.Code, w.Header())
	}
}

func TestDirectoryCurlUploadExample(t *testing.T) {
	securityWorkspace(t)
	if err := os.Mkdir(filepath.Join(filesDir, "my folder"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		enableUpload = enabled
		for _, accept := range []string{"text/html", "text/markdown"} {
			w := securityRequest("GET", "/my%20folder/", nil, map[string]string{
				"Accept": accept, "X-Forwarded-Proto": "https",
			})
			body := w.Body.String()
			if w.Code != 200 || strings.Contains(body, "curl -T file.txt") != enabled {
				t.Fatalf("enabled=%v accept=%s: %d %s", enabled, accept, w.Code, body)
			}
			if enabled && !strings.Contains(body, "https://files.example/my%20folder/file.txt") {
				t.Fatal("upload example does not target the current directory", body)
			}
		}
	}
}
